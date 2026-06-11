package gh

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// step is one programmed outcome of the fake requestFunc.
type step struct {
	resp *http.Response
	err  error
}

// fakeRequester returns programmed responses in order, recording the request
// bodies it received.
type fakeRequester struct {
	steps  []step
	calls  int
	bodies []string
}

func (f *fakeRequester) fn(_ context.Context, _, _ string, body io.Reader) (*http.Response, error) {
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
// records how many waits occurred.
func newTestClient(f *fakeRequester, waits *int) *restClient {
	return &restClient{
		request: f.fn,
		policy: retryPolicy{
			maxAttempts: defaultMaxAttempts,
			wait:        func(context.Context, time.Duration) error { *waits++; return nil },
			now:         func() time.Time { return time.Unix(0, 0) },
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
		{err: httpErr(429, http.Header{"Retry-After": {"0"}})},
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
