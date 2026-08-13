package gh

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// cursorResp is a 200 whose Link header advertises a next cursor, built on the
// shared linkResp helper.
func cursorResp(body, after string) *http.Response {
	return linkResp(body, `<https://api.github.com/repos/o/r/activity?per_page=100&after=`+after+`>; rel="next"`)
}

func TestListRepoActivityFollowsCursor(t *testing.T) {
	// This endpoint ignores `page`, so paging must follow the Link header's
	// cursor. A page-number loop would re-read page one forever.
	f := &fakeRequester{steps: []step{
		{resp: cursorResp(`[{"activity_type":"push","ref":"refs/heads/main","after":"aaa"}]`, "CUR2")},
		{resp: cursorResp(`[{"activity_type":"force_push","ref":"refs/heads/main","after":"bbb"}]`, "CUR3")},
		{resp: okResp(`[{"activity_type":"push","ref":"refs/heads/main","after":"ccc"}]`)}, // no Link: last page
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoActivity(context.Background(), "org", "hw1-ada", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d activities across 3 pages, want 3: %+v", len(out), out)
	}
	if f.calls != 3 {
		t.Errorf("expected three requests, got %d", f.calls)
	}
	// The cursor from each response must appear on the next request, and never a
	// page number.
	if strings.Contains(f.paths[0], "after=") {
		t.Errorf("the first request should carry no cursor: %q", f.paths[0])
	}
	if !strings.Contains(f.paths[1], "after=CUR2") {
		t.Errorf("request 2 should use the first response's cursor: %q", f.paths[1])
	}
	if !strings.Contains(f.paths[2], "after=CUR3") {
		t.Errorf("request 3 should use the second response's cursor: %q", f.paths[2])
	}
	for _, p := range f.paths {
		if strings.Contains(p, "page=") && !strings.Contains(p, "per_page=") {
			t.Errorf("must not paginate by page number: %q", p)
		}
	}
	if !strings.Contains(f.paths[0], "ref=refs%2Fheads%2Fmain") {
		t.Errorf("the ref filter should be sent: %q", f.paths[0])
	}
}

func TestListRepoActivityStopsOnRepeatedCursor(t *testing.T) {
	// A server that keeps advertising the same cursor would otherwise spin
	// forever. Every response here repeats CUR1.
	// Two distinct responses, both advertising CUR1: the fake cannot replay a
	// drained body, so the repeat has to be spelled out.
	f := &fakeRequester{steps: []step{
		{resp: cursorResp(`[{"activity_type":"push","after":"aaa"}]`, "CUR1")},
		{resp: cursorResp(`[{"activity_type":"push","after":"bbb"}]`, "CUR1")},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoActivity(context.Background(), "org", "hw1-ada", "")
	if err != nil {
		t.Fatal(err)
	}
	// Two requests: the first, then one with CUR1 whose response repeats CUR1.
	if f.calls != 2 {
		t.Errorf("a repeated cursor should stop the walk, got %d requests", f.calls)
	}
	if len(out) != 2 {
		t.Errorf("got %d activities, want the two fetched pages", len(out))
	}
}

func TestListRepoActivityStopsOnEmptyPage(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: cursorResp(`[{"activity_type":"push","after":"aaa"}]`, "CUR2")},
		{resp: cursorResp(`[]`, "CUR3")}, // empty despite advertising more
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoActivity(context.Background(), "org", "hw1-ada", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || len(out) != 1 {
		t.Errorf("an empty page should end the walk: %d requests, %d activities", f.calls, len(out))
	}
}

func TestActivityDecoding(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`[
		{"id":1,"ref":"refs/heads/feature/models","timestamp":"2025-11-10T06:39:43Z",
		 "activity_type":"force_push","before":"b1","after":"a1","actor":{"login":"ada"}},
		{"id":2,"ref":"refs/heads/main","timestamp":"2025-11-10T06:40:00Z",
		 "activity_type":"push","before":"b2","after":"a2","actor":null}
	]`)}}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoActivity(context.Background(), "org", "hw1-ada", "")
	if err != nil {
		t.Fatal(err)
	}
	// A branch name containing a slash must survive intact: these are exactly the
	// branches a naive path-based recorder loses.
	if out[0].Branch() != "feature/models" {
		t.Errorf("Branch() = %q, want feature/models", out[0].Branch())
	}
	if out[0].Timestamp.IsZero() {
		t.Error("timestamp should decode")
	}
	// A null actor must decode to an empty login rather than failing the request.
	if out[1].Actor.Login != "" {
		t.Errorf("a null actor should leave an empty login, got %q", out[1].Actor.Login)
	}
}

func TestActivitySetsTip(t *testing.T) {
	// Only activities that leave a real commit as the tip can answer "what was
	// the tip at time T"; a deletion's all-zero After must never be pinned.
	tests := []struct {
		atype, after string
		want         bool
	}{
		{ActivityPush, "abc123", true},
		{ActivityForcePush, "abc123", true},
		{ActivityBranchCreation, "abc123", true},
		{ActivityPRMerge, "abc123", true},
		{ActivityMergeQueueMerge, "abc123", true},
		{ActivityBranchDeletion, "0000000000000000000000000000000000000000", false},
		{ActivityPush, "0000000000000000000000000000000000000000", false},
		{ActivityPush, "", false},
	}
	for _, tc := range tests {
		got := Activity{ActivityType: tc.atype, After: tc.after}.SetsTip()
		if got != tc.want {
			t.Errorf("SetsTip(%s, %q) = %v, want %v", tc.atype, tc.after, got, tc.want)
		}
	}
}

func TestCommitExists(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"sha":"abc"}`)}}}
		var waits int
		ok, err := newTestClient(f, &waits).CommitExists(context.Background(), "org", "hw1-ada", "abc")
		if err != nil || !ok {
			t.Fatalf("got %v, %v; want true, nil", ok, err)
		}
		if f.paths[0] != "repos/org/hw1-ada/commits/abc" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("orphaned", func(t *testing.T) {
		// A commit removed by a force push and since collected 404s. That is an
		// answer, not a failure: it is what makes a pinned SHA uncollectable.
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		ok, err := newTestClient(f, &waits).CommitExists(context.Background(), "org", "hw1-ada", "gone")
		if err != nil {
			t.Fatalf("a missing commit is not an error: %v", err)
		}
		if ok {
			t.Error("want false for a commit that is gone")
		}
	})
}

func TestNextCursor(t *testing.T) {
	tests := map[string]struct {
		link string
		want string
	}{
		"next only":       {`<https://api.github.com/x?after=ABC>; rel="next"`, "ABC"},
		"next among many": {`<https://api.github.com/x?after=ABC>; rel="next", <https://api.github.com/y>; rel="first"`, "ABC"},
		"no next":         {`<https://api.github.com/y?after=ZZZ>; rel="prev"`, ""},
		"absent":          {"", ""},
		"unparseable":     {`garbage`, ""},
	}
	for name, tc := range tests {
		h := http.Header{}
		if tc.link != "" {
			h.Set("Link", tc.link)
		}
		if got := nextCursor(h); got != tc.want {
			t.Errorf("%s: nextCursor = %q, want %q", name, got, tc.want)
		}
	}
}
