package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// requestFunc issues one HTTP request and returns the raw response. It matches
// go-gh's public RESTClient.RequestWithContext, the only mechanism this package
// uses to reach the GitHub API; depending on that public signature (rather than
// any internal transport wiring) keeps the retry loop entirely on the public
// surface and makes it injectable in tests.
type requestFunc func(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)

// restClient is the go-gh-backed implementation of Client. One is built per
// command run and shared by every worker, which is what lets the rate-limit gate
// pause the whole run rather than one request at a time.
type restClient struct {
	request requestFunc
	policy  retryPolicy
	limits  gate
	reads   pacer
	content pacer
	access  pacer
	// inFlight hands out the run's concurrent-request slots. A nil channel caps
	// nothing, which is what a client built for a test wants.
	inFlight chan struct{}
	notices  io.Writer
	log      *requestLog
}

// Option configures a Client at construction.
type Option func(*restClient)

// WithNotices gives the client somewhere to report what the instructor needs to
// see while a run is in progress: a rate-limit pause as it begins, since a run
// that goes silent for a minute is otherwise indistinguishable from a hung one,
// and a request log that had to be abandoned. A nil writer reports nothing.
func WithNotices(w io.Writer) Option {
	return func(c *restClient) {
		if w == nil {
			return
		}
		c.notices = w
		c.limits.notify = func(d time.Duration) {
			fmt.Fprintf(w, "  GitHub is rate limiting this run; waiting %s before continuing\n", d.Round(time.Second))
		}
	}
}

// WithRequestLog records every API request as a JSON line on w.
func WithRequestLog(w io.Writer) Option {
	return func(c *restClient) {
		if w == nil {
			return
		}
		c.log = &requestLog{w: w}
	}
}

// New builds a Client over the user's existing gh authentication and host
// configuration (GH_TOKEN / GH_HOST are honored by go-gh).
func New(opts ...Option) (Client, error) {
	rc, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("creating GitHub client: %w", err)
	}
	c := &restClient{request: rc.RequestWithContext, policy: defaultPolicy()}
	c.reads.spacing = readSpacing
	c.content.spacing = contentSpacing
	c.access.spacing = accessSpacing
	c.inFlight = make(chan struct{}, maxInFlight)
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// writeKind says which of GitHub's budgets a mutating request draws on, and so
// which rate it is paced at. Every caller gets the tight one unless it says
// otherwise, because guessing wrong in that direction only costs time, while
// guessing wrong the other way is what refuses repositories.
type writeKind int

const (
	contentWrite writeKind = iota // creates a repository, commit, ref, PR or issue
	accessWrite                   // only changes who can reach something
)

// do issues a request with rate-limit-aware retry, paced as content creation.
// A non-nil body is sent as JSON; a non-nil out receives the decoded successful
// response. The response headers are returned so callers can read values such
// as Link.
func (c *restClient) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	return c.send(ctx, contentWrite, method, path, body, out)
}

// doAccess is do for a request that grants, changes or revokes access and
// creates nothing, so it is paced at the looser rate those endpoints allow.
// Declaring it at the call site rather than inferring it from the URL keeps the
// classification where the operation's meaning is known.
func (c *restClient) doAccess(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	return c.send(ctx, accessWrite, method, path, body, out)
}

func (c *restClient) send(ctx context.Context, kind writeKind, method, path string, body, out any) (http.Header, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
		payload = b
	}

	var lastErr error
	// retriedAfterAmbiguous records that an earlier attempt failed in a way that
	// leaves its server-side effect unknown, which changes how a 404 reads.
	var retriedAfterAmbiguous bool
	for attempt := 1; attempt <= c.policy.maxAttempts; attempt++ {
		held := c.policy.now()
		if err := c.throttle(ctx, method, kind); err != nil {
			return nil, err
		}
		var r io.Reader
		if payload != nil {
			r = bytes.NewReader(payload)
		}
		start := c.policy.now()
		resp, err := c.issue(ctx, method, path, r)
		c.record(method, path, attempt, held, start, resp, err)
		if err != nil {
			// A DELETE whose earlier attempt may already have committed answers 404
			// on the retry because the resource is now gone: that is the operation
			// having succeeded, not a failure. Reporting it as a hard 404 aborts a
			// freeze over an invitation the tool itself deleted. A 404 on the first
			// attempt, or after a rate-limit rejection (which never reached the
			// resource), still means the resource was genuinely absent.
			if retriedAfterAmbiguous && method == http.MethodDelete && notFound(err) {
				return http.Header{}, nil
			}
			lastErr = err
			delay, retry := c.policy.retryDelay(method, err, attempt)
			if !retry {
				return nil, err
			}
			if attempt == c.policy.maxAttempts {
				return nil, exhausted(err, c.policy.maxAttempts)
			}
			// A rate limit is the run's problem, not this request's: publish it so
			// every worker waits, and let the gate at the top of the loop do the
			// waiting. Anything else (a 5xx, a dropped connection) is local to this
			// request, so only it backs off.
			if rateLimitRejection(err) {
				c.limits.block(c.policy.now(), delay)
			} else if werr := c.policy.wait(ctx, delay); werr != nil {
				return nil, werr
			}
			retriedAfterAmbiguous = retriedAfterAmbiguous || ambiguousFailure(err)
			continue
		}
		return resp.Header, decode(resp, out, method, path)
	}
	return nil, lastErr
}

// issue sends one request, holding one of the run's in-flight slots for as long
// as it is outstanding. The slot is taken after the request has waited out the
// gate and its rate, so a request waiting its turn is not occupying one.
func (c *restClient) issue(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if c.inFlight == nil {
		return c.request(ctx, method, path, body)
	}
	select {
	case c.inFlight <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.inFlight }()
	return c.request(ctx, method, path, body)
}

// pacerFor reports which rate a request queues behind. The three budgets are
// separate because GitHub's limits on them differ by more than an order of
// magnitude, and sharing one rate would mean pacing content far too fast or
// everything else far too slowly.
func (c *restClient) pacerFor(method string, kind writeKind) *pacer {
	switch {
	case !mutating(method):
		return &c.reads
	case kind == accessWrite:
		return &c.access
	default:
		return &c.content
	}
}

// record writes one line of the request log, if one is being kept: one attempt,
// what it was answered with, how long it took, and how long the run's own limits
// held it back before it went. A log that cannot be written is reported once,
// where a rate-limit pause is reported, and then abandoned: it is a diagnostic,
// and failing an assign run partway through a class because its log file broke
// would cost far more than the log is worth.
func (c *restClient) record(method, path string, attempt int, held, start time.Time, resp *http.Response, err error) {
	if c.log == nil {
		return
	}
	e := logEntry{
		Time:    start.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Method:  method,
		Path:    path,
		Attempt: attempt,
		Millis:  c.policy.now().Sub(start).Milliseconds(),
		HeldMs:  start.Sub(held).Milliseconds(),
		Status:  status(resp, err),
		Limits:  limitSnapshot(headers(resp, err)),
		Error:   message(err),
	}
	if logErr := c.log.record(e); logErr != nil && c.notices != nil {
		fmt.Fprintf(c.notices, "  request log abandoned: %v\n", logErr)
	}
}

// status, headers and message read what a request returned, from the response
// on success and from the error on failure: go-gh hands back one or the other,
// never both.
func status(resp *http.Response, err error) int {
	if resp != nil {
		return resp.StatusCode
	}
	var he *api.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode
	}
	return 0
}

func headers(resp *http.Response, err error) http.Header {
	if resp != nil {
		return resp.Header
	}
	var he *api.HTTPError
	if errors.As(err, &he) {
		return he.Headers
	}
	return nil
}

func message(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// throttle holds a request until the run's limits allow it to go: first any
// pause GitHub has already put the run in, then its turn in the run-wide spacing
// for its kind. Reads and writes are paced separately because the limits on them
// differ by more than an order of magnitude, and sharing one rate would mean
// either pacing writes far too fast or reads far too slowly.
//
// Waiting here costs no attempt, since only a request that was issued and
// refused consumes one, so a request held while its siblings back off still has
// its full budget when the run resumes.
func (c *restClient) throttle(ctx context.Context, method string, kind writeKind) error {
	if err := c.limits.await(ctx, c.policy.wait, c.policy.now); err != nil {
		return err
	}
	p := c.pacerFor(method, kind)
	if d := p.reserve(c.policy.now()); d > 0 {
		return c.policy.wait(ctx, d)
	}
	return ctx.Err()
}

// decode reads (and always closes) a successful response body, unmarshaling into
// out when out is non-nil and draining it otherwise so the connection can be
// reused.
func decode(resp *http.Response, out any, method, path string) error {
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// exhausted annotates a failure that survived every retry. Only a
// content-creation rejection is worth explaining: the run waited out GitHub's
// per-minute limit repeatedly and was still refused, which points at the hourly
// cap instead, and no further waiting inside this run will clear that.
func exhausted(err error, attempts int) error {
	var he *api.HTTPError
	if errors.As(err, &he) && submittedTooQuickly(he) {
		return fmt.Errorf("%w; GitHub still refused to create content after %d attempts spread over more than ten minutes, which points at its hourly content-creation limit rather than the per-minute one. Nothing was created; wait for the hour to turn over and re-run, which skips everything that already exists", err, attempts)
	}
	return err
}

// notFound reports whether err is a 404 from the API.
func notFound(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusNotFound
}

// emptyRepo reports whether err is the specific 409 ("Git Repository is
// empty") the API answers for a repository that exists but has no commits
// yet, the transient state a freshly generated repo passes through before
// its starter commit lands. The message is checked alongside the status so a
// different 409 on the same endpoint is treated as a real error rather than
// mistaken for "no commits yet".
func emptyRepo(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusConflict && strings.Contains(he.Message, "empty")
}
