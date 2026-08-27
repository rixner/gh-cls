package gh

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// step is one programmed outcome of the fake requestFunc.
type step struct {
	resp *http.Response
	err  error
}

// fakeRequester returns programmed responses in order, recording the method,
// path, and body of each request it received.
type fakeRequester struct {
	steps   []step
	calls   int
	bodies  []string
	methods []string
	paths   []string
}

func (f *fakeRequester) fn(_ context.Context, method, path string, body io.Reader) (*http.Response, error) {
	f.methods = append(f.methods, method)
	f.paths = append(f.paths, path)
	if body != nil {
		b, _ := io.ReadAll(body)
		f.bodies = append(f.bodies, string(b))
	} else {
		f.bodies = append(f.bodies, "")
	}
	i := f.calls
	f.calls++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	return f.steps[i].resp, f.steps[i].err
}

func okResp(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Test": {"yes"}}, Body: io.NopCloser(strings.NewReader(body))}
}

// newTestClient builds a restClient whose retries never actually sleep and
// records how many waits occurred. Waiting still advances the clock, exactly as
// sleeping would: the rate-limit gate re-reads the time after each wait to see
// whether the pause is over, so a clock frozen at the moment the limit was
// recorded would hold every request forever.
func newTestClient(f *fakeRequester, waits *int) *restClient {
	now := time.Unix(0, 0)
	return &restClient{
		request: f.fn,
		policy: retryPolicy{
			maxAttempts: defaultMaxAttempts,
			wait:        func(_ context.Context, d time.Duration) error { *waits++; now = now.Add(d); return nil },
			now:         func() time.Time { return now },
		},
	}
}

func TestDoDecodesAndReturnsHeaders(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{"name":"hw1","is_template":true}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	var repo Repo
	hdr, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, &repo)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "hw1" || !repo.IsTemplate {
		t.Errorf("decoded %+v, want name=hw1 is_template=true", repo)
	}
	if hdr.Get("X-Test") != "yes" {
		t.Error("response headers should be returned")
	}
	if waits != 0 {
		t.Errorf("no retries expected, got %d waits", waits)
	}
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	f := &fakeRequester{steps: []step{
		// A non-zero Retry-After, so there is a wait to count: a zero-length pause
		// is skipped outright rather than slept through.
		{err: httpErr(429, http.Header{"Retry-After": {"1"}})},
		{resp: okResp(`{"name":"hw1"}`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	var repo Repo
	if _, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.Name != "hw1" {
		t.Errorf("got %+v", repo)
	}
	if waits != 1 {
		t.Errorf("expected exactly one retry wait, got %d", waits)
	}
	if f.calls != 2 {
		t.Errorf("expected 2 request attempts, got %d", f.calls)
	}
}

func TestDoExhaustsRetriesOn5xx(t *testing.T) {
	f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
	var waits int
	c := newTestClient(f, &waits)

	if _, err := c.do(context.Background(), "GET", "x", nil, nil); err == nil {
		t.Fatal("repeated 5xx should ultimately fail")
	}
	if f.calls != defaultMaxAttempts {
		t.Errorf("attempts = %d, want %d", f.calls, defaultMaxAttempts)
	}
	if waits != defaultMaxAttempts-1 {
		t.Errorf("waits = %d, want %d", waits, defaultMaxAttempts-1)
	}
}

func TestDoDoesNotRetryPostOn5xx(t *testing.T) {
	// A create that 5xxes may already have committed; retrying would duplicate it,
	// so do must fail fast and let the command's existence checks make a re-run safe.
	f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
	var waits int
	c := newTestClient(f, &waits)

	if _, err := c.do(context.Background(), "POST", "repos/o/x/issues", map[string]any{"title": "t"}, nil); err == nil {
		t.Fatal("a POST 5xx should fail without retry")
	}
	if f.calls != 1 || waits != 0 {
		t.Errorf("a non-idempotent POST should not be retried: calls=%d waits=%d", f.calls, waits)
	}
}

func TestDoDoesNotRetryClientError(t *testing.T) {
	f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
	var waits int
	c := newTestClient(f, &waits)

	_, err := c.do(context.Background(), "GET", "x", nil, nil)
	if err == nil || !notFound(err) {
		t.Fatalf("want 404 error, got %v", err)
	}
	if f.calls != 1 || waits != 0 {
		t.Errorf("client error should not retry: calls=%d waits=%d", f.calls, waits)
	}
}

func TestDoTreatsRetriedDeleteNotFoundAsDone(t *testing.T) {
	// The first DELETE reached the server and committed; its answer was lost. The
	// retry then finds nothing to delete. Reporting that 404 as a failure aborts a
	// freeze over an invitation the tool itself removed.
	f := &fakeRequester{steps: []step{
		{err: errors.New("connection reset by peer")},
		{err: httpErr(404, nil)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	if _, err := c.do(context.Background(), "DELETE", "repos/o/hw1-ada/invitations/5", nil, nil); err != nil {
		t.Fatalf("a retried DELETE that already took effect should succeed, got %v", err)
	}
	if f.calls != 2 {
		t.Errorf("attempts = %d, want 2", f.calls)
	}
}

func TestDoKeepsDefiniteDeleteNotFound(t *testing.T) {
	t.Run("first attempt", func(t *testing.T) {
		// Nothing preceded this 404, so the resource genuinely was not there.
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		if _, err := c.do(context.Background(), "DELETE", "repos/o/hw1-ada", nil, nil); err == nil || !notFound(err) {
			t.Fatalf("want a 404 error, got %v", err)
		}
	})
	t.Run("after a rate-limit rejection", func(t *testing.T) {
		// A 429 is the server refusing the request outright, so it never reached the
		// resource: the following 404 is still the definite answer.
		f := &fakeRequester{steps: []step{
			{err: httpErr(429, http.Header{"Retry-After": {"0"}})},
			{err: httpErr(404, nil)},
		}}
		var waits int
		c := newTestClient(f, &waits)
		if _, err := c.do(context.Background(), "DELETE", "repos/o/hw1-ada", nil, nil); err == nil || !notFound(err) {
			t.Fatalf("want a 404 error, got %v", err)
		}
	})
	t.Run("other methods", func(t *testing.T) {
		// Only DELETE has "gone" as its intended end state; a GET's 404 after a
		// retry is a real answer.
		f := &fakeRequester{steps: []step{
			{err: errors.New("connection reset by peer")},
			{err: httpErr(404, nil)},
		}}
		var waits int
		c := newTestClient(f, &waits)
		if _, err := c.do(context.Background(), "GET", "repos/o/hw1-ada", nil, nil); err == nil || !notFound(err) {
			t.Fatalf("want a 404 error, got %v", err)
		}
	})
}

func TestDoSendsJSONBody(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	body := map[string]any{"permission": "push"}
	if _, err := c.do(context.Background(), "PUT", "x", body, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 1 || !strings.Contains(f.bodies[0], `"permission":"push"`) {
		t.Errorf("body not sent as JSON: %v", f.bodies)
	}
}

func TestEmptyRepoRequiresEmptyMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"409 with empty message", &api.HTTPError{StatusCode: 409, Message: "Git Repository is empty."}, true},
		{"409 with a different message", &api.HTTPError{StatusCode: 409, Message: "Merge conflict"}, false},
		{"404", &api.HTTPError{StatusCode: 404, Message: "Git Repository is empty."}, false},
		{"non-HTTPError", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyRepo(tc.err); got != tc.want {
				t.Errorf("emptyRepo(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDoStopsOnContextCancelDuringWait(t *testing.T) {
	f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
	c := &restClient{
		request: f.fn,
		policy: retryPolicy{
			maxAttempts: defaultMaxAttempts,
			wait:        func(context.Context, time.Duration) error { return context.Canceled },
			now:         func() time.Time { return time.Unix(0, 0) },
		},
	}
	if _, err := c.do(context.Background(), "GET", "x", nil, nil); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if f.calls != 1 {
		t.Errorf("should stop after the first attempt's wait is cancelled, calls=%d", f.calls)
	}
}

func TestDoWaitsOutALimitAnotherRequestHit(t *testing.T) {
	// The request itself is fine and would succeed immediately. It waits anyway,
	// because the run is rate-limited: this is the whole point of holding the
	// limit on the shared client instead of inside one request's retry loop.
	f := &fakeRequester{steps: []step{{resp: okResp(`{"name":"hw1"}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	start := c.policy.now()
	c.limits.block(start, 30*time.Second)

	if _, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Errorf("waits = %d, want the request to wait out the run's pause once", waits)
	}
	if f.calls != 1 {
		t.Errorf("calls = %d, want the request issued once, after the pause", f.calls)
	}
	if got := c.policy.now().Sub(start); got != 30*time.Second {
		t.Errorf("issued %v after the limit, want 30s", got)
	}
}

func TestDoPublishesARateLimitToTheWholeRun(t *testing.T) {
	// A rate-limited request must record the pause where every other request will
	// see it, not just sleep on its own.
	f := &fakeRequester{steps: []step{
		{err: httpErr(429, http.Header{"Retry-After": {"60"}})},
		{resp: okResp(`{"name":"hw1"}`)},
	}}
	var waits int
	c := newTestClient(f, &waits)
	start := c.policy.now()

	if _, err := c.do(context.Background(), "POST", "repos/o/t/generate", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if d := c.limits.hold(start); d != 60*time.Second {
		t.Errorf("gate hold at the moment of the limit = %v, want the 60s Retry-After", d)
	}
}

func TestDoDoesNotPublishAnOrdinaryFailure(t *testing.T) {
	// A 5xx or a dropped connection is this request's problem. Pausing the whole
	// run for it would stall every other repo over one flaky call.
	f := &fakeRequester{steps: []step{{err: httpErr(503, nil)}}}
	var waits int
	c := newTestClient(f, &waits)
	start := c.policy.now()

	if _, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, nil); err == nil {
		t.Fatal("want the 503 to be returned after its retries")
	}
	if d := c.limits.hold(start); d > 0 {
		t.Errorf("a 5xx must not pause the run, got a %v hold", d)
	}
}

func TestGateWaitCostsNoAttempt(t *testing.T) {
	// Waiting for someone else's limit is not an attempt. A request held at the
	// gate must still get its full retry budget once the run resumes, or a run
	// that pauses would fail requests that never got to try.
	steps := make([]step, defaultMaxAttempts)
	for i := range steps[:defaultMaxAttempts-1] {
		steps[i] = step{err: httpErr(503, nil)}
	}
	steps[defaultMaxAttempts-1] = step{resp: okResp(`{"name":"hw1"}`)}
	f := &fakeRequester{steps: steps}
	var waits int
	c := newTestClient(f, &waits)
	c.limits.block(c.policy.now(), time.Minute)

	if _, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, nil); err != nil {
		t.Fatalf("the request should still have all %d attempts after the pause: %v", defaultMaxAttempts, err)
	}
	if f.calls != defaultMaxAttempts {
		t.Errorf("calls = %d, want %d", f.calls, defaultMaxAttempts)
	}
}
