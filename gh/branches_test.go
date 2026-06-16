package gh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// linkResp builds a 200 response carrying the given Link header (empty for none).
func linkResp(body, link string) *http.Response {
	h := http.Header{}
	if link != "" {
		h.Set("Link", link)
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestLastPage(t *testing.T) {
	cases := []struct {
		name string
		link string
		want int
	}{
		{"no header means one page", "", 1},
		{
			"reads rel=last page number",
			`<https://api.github.com/x?per_page=1&page=2>; rel="next", <https://api.github.com/x?per_page=1&page=7>; rel="last"`,
			7,
		},
		{
			"no rel=last means one page",
			`<https://api.github.com/x?per_page=1&page=2>; rel="next"`,
			1,
		},
		{"garbage falls back to one", `not a real link header`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastPage(tc.link); got != tc.want {
				t.Errorf("lastPage(%q) = %d, want %d", tc.link, got, tc.want)
			}
		})
	}
}

func TestListBranchesWithCommitCount(t *testing.T) {
	lastLink := `<https://api.github.com/repositories/1/commits?per_page=1&page=4>; rel="last"`
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[{"name":"main"},{"name":"solution"}]`)}, // branch list
		{resp: linkResp(`[]`, "")},                              // main: no Link -> 1 commit
		{resp: linkResp(`[]`, lastLink)},                        // solution: 4 commits
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListBranchesWithCommitCount(context.Background(), "org", "hw1-template")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"main": 1, "solution": 4}
	if len(out) != len(want) {
		t.Fatalf("got %d branches, want %d", len(out), len(want))
	}
	for _, b := range out {
		if want[b.Name] != b.Commits {
			t.Errorf("branch %q: got %d commits, want %d", b.Name, b.Commits, want[b.Name])
		}
	}
	if !strings.HasPrefix(f.paths[0], "repos/org/hw1-template/branches") {
		t.Errorf("first request should list branches, got %q", f.paths[0])
	}
	if !strings.Contains(f.paths[1], "commits?sha=main") || !strings.Contains(f.paths[1], "per_page=1") {
		t.Errorf("commit-count request wrong: %q", f.paths[1])
	}
}

// TestListBranchesWithCommitCountPaginates verifies the branch list itself is
// paged: a full first page is followed by a second request, so a template with
// more than one page of branches is checked in full rather than truncated.
func TestListBranchesWithCommitCountPaginates(t *testing.T) {
	var page1 strings.Builder
	page1.WriteByte('[')
	for i := 0; i < pageSize; i++ {
		if i > 0 {
			page1.WriteByte(',')
		}
		fmt.Fprintf(&page1, `{"name":"b%d"}`, i)
	}
	page1.WriteByte(']')

	f := &fakeRequester{steps: []step{
		{resp: okResp(page1.String())},      // branches page 1 (full -> fetch more)
		{resp: okResp(`[{"name":"last"}]`)}, // branches page 2 (short -> stop)
		{resp: linkResp(`[]`, "")},          // every per-branch commit count: 1 commit
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListBranchesWithCommitCount(context.Background(), "org", "big-template")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != pageSize+1 {
		t.Fatalf("got %d branches, want %d (both pages)", len(out), pageSize+1)
	}
	if !strings.Contains(f.paths[0], "branches") || !strings.Contains(f.paths[0], "page=1") {
		t.Errorf("first request should be branches page 1, got %q", f.paths[0])
	}
	if !strings.Contains(f.paths[1], "branches") || !strings.Contains(f.paths[1], "page=2") {
		t.Errorf("second request should be branches page 2, got %q", f.paths[1])
	}
	if !strings.Contains(f.paths[2], "commits") {
		t.Errorf("third request should begin the commit counts, got %q", f.paths[2])
	}
	for _, b := range out {
		if b.Commits != 1 {
			t.Errorf("branch %q: got %d commits, want 1", b.Name, b.Commits)
		}
	}
}
