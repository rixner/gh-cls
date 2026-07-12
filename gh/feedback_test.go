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

func TestRebaseOntoEmptyRoot(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{"object":{"sha":"tip-sha"}}`)},                     // GET main tip
		{resp: okResp(`{"tree":{"sha":"content-tree"},"message":"Init"}`)}, // GET its commit
		{resp: okResp(`{"sha":"root-sha"}`)},                               // POST empty root
		{resp: okResp(`{"sha":"rebased-sha"}`)},                            // POST content on the root
		{resp: okResp(`{"object":{"sha":"rebased-sha"}}`)},                 // PATCH main (force)
		{resp: okResp(`{"object":{"sha":"rebased-sha"}}`)},                 // GET main tip (verify)
	}}
	var waits int
	c := newTestClient(f, &waits)
	root, err := c.RebaseOntoEmptyRoot(context.Background(), "org", "hw1-ada", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Returns the empty root's SHA: the base the feedback branch is pointed at.
	if root != "root-sha" {
		t.Errorf("root = %q, want root-sha", root)
	}
	// The empty root is a parent-less commit over git's canonical empty tree.
	if f.methods[2] != "POST" || f.paths[2] != "repos/org/hw1-ada/git/commits" {
		t.Errorf("empty-root request = %s %s", f.methods[2], f.paths[2])
	}
	for _, want := range []string{`"tree":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"`, `"parents":[]`} {
		if !strings.Contains(f.bodies[2], want) {
			t.Errorf("empty-root body %s missing %s", f.bodies[2], want)
		}
	}
	// The rebased commit keeps the branch's tree but now descends from the root.
	for _, want := range []string{`"tree":"content-tree"`, `"parents":["root-sha"]`} {
		if !strings.Contains(f.bodies[3], want) {
			t.Errorf("rebased body %s missing %s", f.bodies[3], want)
		}
	}
	// The default branch is force-moved onto the rebased commit.
	if f.methods[4] != "PATCH" || f.paths[4] != "repos/org/hw1-ada/git/refs/heads/main" {
		t.Errorf("move request = %s %s", f.methods[4], f.paths[4])
	}
	for _, want := range []string{`"sha":"rebased-sha"`, `"force":true`} {
		if !strings.Contains(f.bodies[4], want) {
			t.Errorf("move body %s missing %s", f.bodies[4], want)
		}
	}
	// Post-condition: the branch is re-read to confirm the rewrite landed.
	if f.methods[5] != "GET" || f.paths[5] != "repos/org/hw1-ada/git/ref/heads/main" {
		t.Errorf("verify request = %s %s", f.methods[5], f.paths[5])
	}
}

// TestRebaseOntoEmptyRootDetectsFailedMove guards the post-condition: if the
// force-move silently does not land (e.g. a protected branch), the branch still
// resolves to its old tip and the rebase must fail rather than return a root that
// the default branch does not descend from.
func TestRebaseOntoEmptyRootDetectsFailedMove(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{"object":{"sha":"tip-sha"}}`)},
		{resp: okResp(`{"tree":{"sha":"content-tree"},"message":"Init"}`)},
		{resp: okResp(`{"sha":"root-sha"}`)},
		{resp: okResp(`{"sha":"rebased-sha"}`)},
		{resp: okResp(`{"object":{"sha":"rebased-sha"}}`)},
		{resp: okResp(`{"object":{"sha":"tip-sha"}}`)}, // verify: still the old tip
	}}
	var waits int
	c := newTestClient(f, &waits)
	if _, err := c.RebaseOntoEmptyRoot(context.Background(), "org", "hw1-ada", "main"); err == nil {
		t.Fatal("a branch that did not move should fail the rebase")
	}
}

// TestRebaseOntoEmptyRootWaitsForRef covers the ref-read lag the live test hit:
// the force-move lands, but the branch first reads back as its old tip and only
// reflects the rebased commit after a wait. The rebase must tolerate that rather
// than fail.
func TestRebaseOntoEmptyRootWaitsForRef(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{"object":{"sha":"tip-sha"}}`)},
		{resp: okResp(`{"tree":{"sha":"content-tree"},"message":"Init"}`)},
		{resp: okResp(`{"sha":"root-sha"}`)},
		{resp: okResp(`{"sha":"rebased-sha"}`)},
		{resp: okResp(`{"object":{"sha":"rebased-sha"}}`)}, // PATCH main
		{resp: okResp(`{"object":{"sha":"tip-sha"}}`)},     // verify: stale old tip
		{resp: okResp(`{"object":{"sha":"rebased-sha"}}`)}, // verify: now the rebased commit
	}}
	var waits int
	c := newTestClient(f, &waits)
	root, err := c.RebaseOntoEmptyRoot(context.Background(), "org", "hw1-ada", "main")
	if err != nil {
		t.Fatal(err)
	}
	if root != "root-sha" {
		t.Errorf("root = %q, want root-sha", root)
	}
	if waits != 1 {
		t.Errorf("a ref reflecting the move on the second read should wait once, got %d", waits)
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
	// The create POSTs, then re-reads the issue list to confirm the issue is
	// listable before returning: a POST followed by a list that already contains
	// the issue, so the post-condition holds on the first check (no wait).
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{}`)},
		{resp: okResp(`[{"number":1,"title":"Feedback","state":"open"}]`)},
	}}
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
	if f.methods[1] != "GET" || !strings.HasPrefix(f.paths[1], "repos/org/hw1-ada/issues?") {
		t.Errorf("post-condition should list issues, got %s %s", f.methods[1], f.paths[1])
	}
	if waits != 0 {
		t.Errorf("an immediately-listable issue should not wait, got %d waits", waits)
	}
}

// TestCreateIssueWaitsForVisibility covers the eventual-consistency path: the
// issue is not listable on the first check and only appears after a wait. This is
// the regression the live test hit as a flaky "no feedback issue" failure.
func TestCreateIssueWaitsForVisibility(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{}`)}, // POST create
		{resp: okResp(`[]`)}, // list: not visible yet
		{resp: okResp(`[{"number":1,"title":"Feedback","state":"open"}]`)}, // list: now visible
	}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.CreateIssue(context.Background(), "org", "hw1-ada", "Feedback", "body text"); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Errorf("a create that becomes visible on the second check should wait once, got %d", waits)
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

func TestFindIssueByTitle(t *testing.T) {
	t.Run("returns the issue number and state, skipping a like-titled PR", func(t *testing.T) {
		// First entry is a PR sharing the title (issues endpoint includes them);
		// the second is the real issue, whose number and state must be returned.
		f := &fakeRequester{steps: []step{{resp: okResp(
			`[{"number":3,"title":"Feedback","state":"open","pull_request":{"url":"x"}},{"number":9,"title":"Feedback","state":"closed"}]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		n, state, found, err := c.FindIssueByTitle(context.Background(), "org", "hw1-ada", "Feedback")
		if err != nil || !found {
			t.Fatalf("want found, got found=%v err=%v", found, err)
		}
		if n != 9 || state != "closed" {
			t.Errorf("got number=%d state=%q, want 9/closed (the issue, not the PR)", n, state)
		}
		if f.paths[0] != "repos/org/hw1-ada/issues?state=all&per_page=100&page=1" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("not found on empty list", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		n, state, found, err := c.FindIssueByTitle(context.Background(), "org", "hw1-ada", "Feedback")
		if err != nil || found || n != 0 || state != "" {
			t.Fatalf("empty list should mean not found, got n=%d state=%q found=%v err=%v", n, state, found, err)
		}
	})
}

func TestFindPRByBase(t *testing.T) {
	t.Run("returns the PR number and state", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[{"number":7,"state":"open"}]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		n, state, found, err := c.FindPRByBase(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || !found {
			t.Fatalf("want found, got found=%v err=%v", found, err)
		}
		if n != 7 || state != "open" {
			t.Errorf("got number=%d state=%q, want 7/open", n, state)
		}
		if f.paths[0] != "repos/org/hw1-ada/pulls?state=all&base=feedback&per_page=1" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("not found on empty list", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`[]`)}}}
		var waits int
		c := newTestClient(f, &waits)
		n, state, found, err := c.FindPRByBase(context.Background(), "org", "hw1-ada", "feedback")
		if err != nil || found || n != 0 || state != "" {
			t.Fatalf("empty list should mean not found, got n=%d state=%q found=%v err=%v", n, state, found, err)
		}
	})
}

func TestListIssueComments(t *testing.T) {
	// Two full pages then a short page: getPaged must concatenate all of them.
	first := make([]string, pageSize)
	for i := range first {
		first[i] = `{"body":"a"}`
	}
	f := &fakeRequester{steps: []step{
		{resp: okResp("[" + strings.Join(first, ",") + "]")},
		{resp: okResp(`[{"body":"marker here"},{"body":"b"}]`)},
	}}
	var waits int
	c := newTestClient(f, &waits)
	comments, err := c.ListIssueComments(context.Background(), "org", "hw1-ada", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != pageSize+2 {
		t.Fatalf("got %d comments, want %d (both pages)", len(comments), pageSize+2)
	}
	if comments[pageSize].Body != "marker here" {
		t.Errorf("second page body = %q", comments[pageSize].Body)
	}
	if f.paths[0] != "repos/org/hw1-ada/issues/9/comments?per_page=100&page=1" {
		t.Errorf("path = %q", f.paths[0])
	}
}

func TestAddComment(t *testing.T) {
	t.Run("posts the body and returns the URL", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"html_url":"https://github.com/org/hw1-ada/issues/9#issuecomment-1"}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		url, err := c.AddComment(context.Background(), "org", "hw1-ada", 9, "well done")
		if err != nil {
			t.Fatal(err)
		}
		if url != "https://github.com/org/hw1-ada/issues/9#issuecomment-1" {
			t.Errorf("url = %q", url)
		}
		if f.methods[0] != "POST" || f.paths[0] != "repos/org/hw1-ada/issues/9/comments" {
			t.Errorf("request = %s %s", f.methods[0], f.paths[0])
		}
		if !strings.Contains(f.bodies[0], `"body":"well done"`) {
			t.Errorf("body %s missing comment text", f.bodies[0])
		}
	})
	t.Run("fails when the response carries no URL", func(t *testing.T) {
		// A 200 with no html_url means the comment cannot be confirmed; the
		// post-condition must turn that into a loud error, not a silent success.
		f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		if _, err := c.AddComment(context.Background(), "org", "hw1-ada", 9, "x"); err == nil {
			t.Fatal("a response without html_url should fail")
		}
	})
}
