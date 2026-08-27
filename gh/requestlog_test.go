package gh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// decodeLog parses the JSON lines a run wrote.
func decodeLog(t *testing.T, s string) []logEntry {
	t.Helper()
	var out []logEntry
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		var e logEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line %q is not valid JSON: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestRequestLogRecordsEveryAttempt(t *testing.T) {
	// Both the refusal and the retry that succeeded have to appear, with the
	// headers GitHub sent: a log that showed only the outcome would say nothing
	// about the rate at which the limit was reached.
	f := &fakeRequester{steps: []step{
		{err: httpErr(429, http.Header{"Retry-After": {"60"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Resource": {"core"}})},
		{resp: okResp(`{"name":"hw1-s01"}`)},
	}}
	var waits int
	c := newTestClient(f, &waits)
	var log strings.Builder
	WithRequestLog(&log)(c)

	if _, err := c.do(context.Background(), "POST", "repos/o/tmpl/generate", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}

	t.Logf("log as written:\n%s", log.String())
	entries := decodeLog(t, log.String())
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want one per attempt", len(entries))
	}

	refused, ok := entries[0], entries[1]
	if refused.Method != "POST" || refused.Path != "repos/o/tmpl/generate" {
		t.Errorf("first entry = %s %s, want the create", refused.Method, refused.Path)
	}
	if refused.Attempt != 1 || refused.Status != 429 {
		t.Errorf("first entry = attempt %d status %d, want attempt 1 status 429", refused.Attempt, refused.Status)
	}
	if refused.Limits["retry_after"] != "60" || refused.Limits["remaining"] != "0" || refused.Limits["resource"] != "core" {
		t.Errorf("first entry limits = %v, want the headers GitHub sent", refused.Limits)
	}
	if refused.Error == "" {
		t.Error("a refused request should record what it was told")
	}
	if ok.Attempt != 2 || ok.Status != 200 {
		t.Errorf("second entry = attempt %d status %d, want attempt 2 status 200", ok.Attempt, ok.Status)
	}
	if ok.Limits != nil {
		t.Errorf("second entry limits = %v, want none recorded when none were sent", ok.Limits)
	}
	if ok.Error != "" {
		t.Errorf("a successful request recorded an error: %q", ok.Error)
	}
}

func TestRequestLogRecordsHowLongTheRunHeldARequest(t *testing.T) {
	// The log has to separate GitHub's delay from ours, or a paced run reads as a
	// slow one and the pacing cannot be judged from it.
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	var log strings.Builder
	WithRequestLog(&log)(c)
	c.limits.block(c.policy.now(), 30*time.Second)

	if _, err := c.do(context.Background(), "POST", "repos/o/tmpl/generate", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	entries := decodeLog(t, log.String())
	if entries[0].HeldMs != 30_000 {
		t.Errorf("held_ms = %d, want the 30s the run was paused", entries[0].HeldMs)
	}
}

// brokenWriter fails every write, as a full disk would.
type brokenWriter struct{ writes int }

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.writes++
	return 0, errors.New("no space left on device")
}

func TestRequestLogAbandonedOnWriteFailureDoesNotFailTheRun(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	var notices strings.Builder
	broken := &brokenWriter{}
	WithNotices(&notices)(c)
	WithRequestLog(broken)(c)

	for i := 0; i < 3; i++ {
		if _, err := c.do(context.Background(), "GET", "repos/o/hw1", nil, nil); err != nil {
			t.Fatalf("a broken log must not fail the run: %v", err)
		}
	}

	t.Logf("line as the instructor sees it:\n%s", notices.String())
	if broken.writes != 1 {
		t.Errorf("attempted %d writes, want the log abandoned after the first failure", broken.writes)
	}
	if n := strings.Count(notices.String(), "\n"); n != 1 {
		t.Errorf("reported the failure %d times, want once:\n%s", n, notices.String())
	}
	if !strings.Contains(notices.String(), "no space left on device") {
		t.Errorf("the notice should say what went wrong, got: %q", notices.String())
	}
}

func TestLimitSnapshotOmitsWhatWasNotSent(t *testing.T) {
	if got := limitSnapshot(http.Header{}); got != nil {
		t.Errorf("got %v, want nil when the response carried no rate-limit headers", got)
	}
	h := http.Header{}
	h.Set("X-RateLimit-Used", "4001")
	if got := limitSnapshot(h); len(got) != 1 || got["used"] != "4001" {
		t.Errorf("got %v, want just the header that was sent", got)
	}
}
