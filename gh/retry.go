package gh

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

const (
	defaultMaxAttempts = 5
	baseBackoff        = 1 * time.Second
	maxBackoff         = 30 * time.Second
	// maxSingleWait caps any one wait so a far-future rate-limit reset cannot
	// hang the tool for minutes; if the limit persists, retries are exhausted
	// and the caller reports the failure rather than blocking.
	maxSingleWait = 60 * time.Second
)

// retryPolicy decides whether and how long to wait before retrying a failed
// request. The wait and now functions are injectable so tests run without real
// time.
type retryPolicy struct {
	maxAttempts int
	wait        func(ctx context.Context, d time.Duration) error
	now         func() time.Time
}

func defaultPolicy() retryPolicy {
	return retryPolicy{maxAttempts: defaultMaxAttempts, wait: waitCtx, now: time.Now}
}

// retryDelay reports the wait before retrying a failed request and whether the
// failure is retryable at all. A rate-limit response (429 or a secondary
// rate-limit 403) is always retryable: the server rejected the request outright,
// so retrying cannot duplicate work. A 5xx or transport-level error is ambiguous
// — the request may already have been applied — so it is retried only for
// idempotent methods. A definite client error (404, 422, an ordinary 403, ...)
// is never retried.
func (p retryPolicy) retryDelay(method string, err error, attempt int) (time.Duration, bool) {
	var he *api.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusTooManyRequests:
			return p.limitDelay(he.Headers, attempt), true
		case he.StatusCode == http.StatusForbidden && rateLimited(he.Headers):
			return p.limitDelay(he.Headers, attempt), true
		case he.StatusCode >= 500:
			return backoff(attempt), idempotent(method)
		}
		return 0, false
	}
	if err != nil {
		// A transport-level error (timeout, reset connection) is ambiguous: the
		// request may have reached the server and taken effect, so only an
		// idempotent method is safe to retry.
		return backoff(attempt), idempotent(method)
	}
	return 0, false
}

// idempotent reports whether a method is safe to retry after an ambiguous failure
// — one where the request may already have taken effect server-side. POST is the
// only non-idempotent method this tool issues (repo, PR, issue, team, ref, and
// ruleset creation), so retrying it after a 5xx or dropped connection could
// duplicate the resource. Re-running the command is the safe recovery instead,
// since every create is guarded by an existence check.
func idempotent(method string) bool {
	return method != http.MethodPost
}

// limitDelay derives a wait from rate-limit headers, preferring an explicit
// Retry-After, then the X-RateLimit-Reset timestamp, then plain backoff.
func (p retryPolicy) limitDelay(h http.Header, attempt int) time.Duration {
	now := p.now()
	if d, ok := retryAfter(h, now); ok {
		return clamp(d)
	}
	if d, ok := rateLimitReset(h, now); ok {
		return clamp(d)
	}
	return backoff(attempt)
}

// retryAfter parses a Retry-After header, which is either a number of seconds or
// an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return nonNegative(time.Duration(secs) * time.Second), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return nonNegative(t.Sub(now)), true
	}
	return 0, false
}

// rateLimitReset parses the X-RateLimit-Reset header (a Unix timestamp) into the
// wait until the limit resets.
func rateLimitReset(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("X-RateLimit-Reset")
	if v == "" {
		return 0, false
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return nonNegative(time.Unix(sec, 0).Sub(now)), true
}

// rateLimited reports whether a 403 is a rate limit (primary or secondary)
// rather than an ordinary permission denial.
func rateLimited(h http.Header) bool {
	return h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0"
}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := baseBackoff << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func clamp(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxSingleWait {
		return maxSingleWait
	}
	return d
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// waitCtx sleeps for d, returning early if the context is cancelled.
func waitCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
