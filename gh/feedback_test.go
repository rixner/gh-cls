package gh

import (
	"context"
	"testing"
)

func TestBranchExists(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"object":{"sha":"abc"}}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.BranchExists(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || !ok {
			t.Fatalf("want exists, got ok=%v err=%v", ok, err)
		}
		if f.paths[0] != "repos/org/hw1-ada/git/ref/heads/feedback" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("absent on 404", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.BranchExists(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || ok {
			t.Fatalf("404 should mean absent without error, got ok=%v err=%v", ok, err)
		}
	})
	t.Run("propagates other errors", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		if _, err := c.BranchExists(context.Background(), "org", "hw1-ada", "feedback"); err == nil {
			t.Fatal("server error should propagate, not be read as absent")
		}
	})
}

func TestPRExists(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[{"number":7}]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.PRExists(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || !ok {
			t.Fatalf("want exists, got ok=%v err=%v", ok, err)
		}
		if f.paths[0] != "repos/org/hw1-ada/pulls?state=all&base=feedback&per_page=1" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("absent on empty list", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.PRExists(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || ok {
			t.Fatalf("empty list should mean absent, got ok=%v err=%v", ok, err)
		}
	})
}

func TestIssueExists(t *testing.T) {
	t.Run("matches title and skips pull requests", func(t *testing.T) {
		// First entry is a PR (issues endpoint includes them); second is the issue.
		f := &fakeRequester{steps: []step{{resp: okResp(
			`[{"title":"Feedback","pull_request":{"url":"x"}},{"title":"Feedback"}]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.IssueExists(context.Background(), "org", "hw1-ada", "Feedback")
		if err != nil || !ok {
			t.Fatalf("want exists, got ok=%v err=%v", ok, err)
		}
		if f.paths[0] != "repos/org/hw1-ada/issues?state=all&per_page=100&page=1" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("a pull request with the title is not the issue", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(
			`[{"title":"Feedback","pull_request":{"url":"x"}}]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.IssueExists(context.Background(), "org", "hw1-ada", "Feedback")
		if err != nil || ok {
			t.Fatalf("a PR titled Feedback must not count as the issue, got ok=%v err=%v", ok, err)
		}
	})
	t.Run("absent on empty list", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.IssueExists(context.Background(), "org", "hw1-ada", "Feedback")
		if err != nil || ok {
			t.Fatalf("empty list should mean absent, got ok=%v err=%v", ok, err)
		}
	})
}
