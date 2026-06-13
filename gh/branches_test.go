package gh

import (
	"context"
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
