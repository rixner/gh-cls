package gh

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func httpErr(status int, header http.Header) *api.HTTPError {
	if header == nil {
		header = http.Header{}
	}
	return &api.HTTPError{StatusCode: status, Headers: header}
}

func testPolicy(now time.Time) retryPolicy {
	return retryPolicy{maxAttempts: 5, wait: func(context.Context, time.Duration) error { return nil }, now: func() time.Time { return now }}
}

func TestRetryDelayClassification(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := testPolicy(now)

	t.Run("429 honors Retry-After seconds, even on POST", func(t *testing.T) {
		// A rate limit means the request was rejected, not applied, so it is safe
		// to retry regardless of method.
		d, ok := p.retryDelay(http.MethodPost, httpErr(429, http.Header{"Retry-After": {"7"}}), 1)
		if !ok || d != 7*time.Second {
			t.Errorf("got %v,%v want 7s,true", d, ok)
		}
	})

	t.Run("403 secondary limit via X-RateLimit-Reset", func(t *testing.T) {
		reset := now.Add(20 * time.Second).Unix()
		// Build via Set so keys are canonicalized exactly as net/http delivers
		// them on a real response.
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		d, ok := p.retryDelay(http.MethodPost, httpErr(403, h), 1)
		if !ok || d != 20*time.Second {
			t.Errorf("got %v,%v want 20s,true", d, ok)
		}
	})

	t.Run("ordinary 403 is not retryable", func(t *testing.T) {
		if _, ok := p.retryDelay(http.MethodGet, httpErr(403, nil), 1); ok {
			t.Error("a permission 403 should not be retried")
		}
	})

	t.Run("5xx retries an idempotent method with backoff", func(t *testing.T) {
		d, ok := p.retryDelay(http.MethodGet, httpErr(503, nil), 1)
		if !ok || d != baseBackoff {
			t.Errorf("got %v,%v want %v,true", d, ok, baseBackoff)
		}
	})

	t.Run("5xx on POST is not retried", func(t *testing.T) {
		// A 5xx after a create is ambiguous: the resource may already exist, so a
		// retry could duplicate it.
		if _, ok := p.retryDelay(http.MethodPost, httpErr(503, nil), 1); ok {
			t.Error("a 5xx on a non-idempotent POST should not be retried")
		}
	})

	t.Run("404 is not retryable", func(t *testing.T) {
		if _, ok := p.retryDelay(http.MethodGet, httpErr(404, nil), 1); ok {
			t.Error("404 should not be retried")
		}
	})

	t.Run("transport error retries an idempotent method", func(t *testing.T) {
		if _, ok := p.retryDelay(http.MethodGet, errors.New("connection reset"), 1); !ok {
			t.Error("a transport error should be retried for an idempotent method")
		}
	})

	t.Run("transport error on POST is not retried", func(t *testing.T) {
		if _, ok := p.retryDelay(http.MethodPost, errors.New("connection reset"), 1); ok {
			t.Error("a transport error on a non-idempotent POST should not be retried")
		}
	})
}

func TestRetryAfterCaps(t *testing.T) {
	now := time.Unix(0, 0)
	// A far-future reset is clamped to maxSingleWait.
	h := http.Header{"Retry-After": {"100000"}}
	if d := testPolicy(now).limitDelay(h, 1); d != maxSingleWait {
		t.Errorf("limitDelay = %v, want clamp to %v", d, maxSingleWait)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if backoff(1) != baseBackoff {
		t.Errorf("backoff(1) = %v, want %v", backoff(1), baseBackoff)
	}
	if backoff(2) <= backoff(1) {
		t.Error("backoff should grow")
	}
	if backoff(100) != maxBackoff {
		t.Errorf("backoff(100) = %v, want cap %v", backoff(100), maxBackoff)
	}
}

func TestWaitCtxCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitCtx(ctx, time.Hour); err == nil {
		t.Error("waitCtx should return the context error when cancelled")
	}
}

// tooQuicklyErr builds the 422 GitHub answers when repositories are being
// generated faster than it allows, in the shape go-gh delivers it.
func tooQuicklyErr(message string, items ...api.HTTPErrorItem) *api.HTTPError {
	return &api.HTTPError{StatusCode: 422, Headers: http.Header{}, Message: message, Errors: items}
}

func TestContentLimit422IsARateLimit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := testPolicy(now)

	t.Run("retried on POST, the method that hits it", func(t *testing.T) {
		// Repository generation is a POST, so the ordinary rule (never retry a
		// create, it may already have been applied) would refuse this. A rejection
		// is not ambiguous: nothing was created, so re-sending is safe.
		err := tooQuicklyErr("Could not clone: was submitted too quickly")
		d, ok := p.retryDelay(http.MethodPost, err, 1)
		if !ok {
			t.Fatal("a content-creation rejection must be retried")
		}
		if d != time.Minute {
			t.Errorf("first delay = %v, want 1m", d)
		}
	})

	t.Run("found on an error item when the message omits it", func(t *testing.T) {
		// go-gh rewrites an item carrying a recognized code into a canned phrase,
		// which drops the original text from Message; the item still has it.
		err := tooQuicklyErr("Repository creation failed.", api.HTTPErrorItem{
			Resource: "Repository",
			Field:    "repository",
			Code:     "unprocessable",
			Message:  "Could not clone: was submitted too quickly",
		})
		if _, ok := p.retryDelay(http.MethodPost, err, 1); !ok {
			t.Error("the rejection must be recognized from the error item too")
		}
	})

	t.Run("an ordinary 422 still fails fast", func(t *testing.T) {
		// The empty-tree 422 the feedback base once hit. A real validation error
		// is not a rate limit, and retrying it just fails five times slower.
		if _, ok := p.retryDelay(http.MethodPost, tooQuicklyErr("Invalid tree info"), 1); ok {
			t.Error("a validation 422 must not be retried")
		}
	})

	t.Run("the run waits longer each time it is refused", func(t *testing.T) {
		// No Retry-After comes with this one, so the documented fallback applies:
		// at least a minute, then exponentially longer, capped.
		want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, maxContentWait, maxContentWait}
		for i, w := range want {
			if got := contentBackoff(i + 1); got != w {
				t.Errorf("attempt %d delay = %v, want %v", i+1, got, w)
			}
		}
	})

	t.Run("pauses the whole run, not just this request", func(t *testing.T) {
		if !rateLimitRejection(tooQuicklyErr("Could not clone: was submitted too quickly")) {
			t.Error("a content-creation rejection must pause every request")
		}
		if rateLimitRejection(tooQuicklyErr("Invalid tree info")) {
			t.Error("a validation 422 must not pause the run")
		}
	})
}
