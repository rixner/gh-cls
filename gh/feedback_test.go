package gh

import (
	"context"
	"strings"
	"testing"
)

func TestGetRef(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{"object":{"sha":"starter-sha"}}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	sha, err := c.GetRef(context.Background(), "org", "hw1-ada", "heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "starter-sha" {
		t.Errorf("sha = %q", sha)
	}
	// ref segments are part of the path and must not be escaped.
	if f.methods[0] != "GET" || f.paths[0] != "repos/org/hw1-ada/git/ref/heads/main" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
}

func TestCreateRef(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{}`)}, // POST create
		{resp: okResp(`{"object":{"sha":"starter-sha"}}`)}, // GET verify
	}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.CreateRef(context.Background(), "org", "hw1-ada", "refs/heads/feedback", "starter-sha")
	if err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "POST" || f.paths[0] != "repos/org/hw1-ada/git/refs" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	for _, want := range []string{`"ref":"refs/heads/feedback"`, `"sha":"starter-sha"`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("body %s missing %s", f.bodies[0], want)
		}
	}
	// The create is followed by a post-read of the new ref (without "refs/").
	if f.methods[1] != "GET" || f.paths[1] != "repos/org/hw1-ada/git/ref/heads/feedback" {
		t.Errorf("verification request = %s %s", f.methods[1], f.paths[1])
	}
}

// TestCreateRefRejectsMismatch checks the post-read fails the create when the
// ref does not resolve to the requested SHA.
func TestCreateRefRejectsMismatch(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{}`)},                             // POST create
		{resp: okResp(`{"object":{"sha":"other-sha"}}`)}, // GET verify: wrong sha
	}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.CreateRef(context.Background(), "org", "hw1-ada", "refs/heads/feedback", "starter-sha")
	if err == nil || !strings.Contains(err.Error(), "starter-sha") {
		t.Fatalf("a SHA mismatch should fail the create, got %v", err)
	}
}

func TestCreatePR(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.CreatePR(context.Background(), "org", "hw1-ada", "Feedback", "main", "feedback", "body text")
	if err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "POST" || f.paths[0] != "repos/org/hw1-ada/pulls" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	for _, want := range []string{`"title":"Feedback"`, `"head":"main"`, `"base":"feedback"`, `"body":"body text"`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("body %s missing %s", f.bodies[0], want)
		}
	}
}

func TestEnableIssues(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.EnableIssues(context.Background(), "org", "hw1-ada"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PATCH" || f.paths[0] != "repos/org/hw1-ada" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"has_issues":true`) {
		t.Errorf("body %s missing has_issues", f.bodies[0])
	}
}

func TestCreateIssue(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.CreateIssue(context.Background(), "org", "hw1-ada", "Feedback", "body text"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "POST" || f.paths[0] != "repos/org/hw1-ada/issues" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	for _, want := range []string{`"title":"Feedback"`, `"body":"body text"`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("body %s missing %s", f.bodies[0], want)
		}
	}
}

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
	t.Run("absent on 409 empty repository", func(t *testing.T) {
		// A freshly generated repo briefly answers 409 "Git Repository is empty"
		// from the ref endpoint. That must read as absent so waitRepoReady keeps
		// polling instead of surfacing it as fatal and rolling back the new repo.
		f := &fakeRequester{steps: []step{{err: httpErr(409, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		ok, err := c.BranchExists(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || ok {
			t.Fatalf("409 empty repo should mean absent without error, got ok=%v err=%v", ok, err)
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
