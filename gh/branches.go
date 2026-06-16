package gh

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// BranchCount is a branch and its exact total commit count.
type BranchCount struct {
	Name    string
	Commits int
}

// branchRef is the subset of a branch list entry the tool reads.
type branchRef struct {
	Name string `json:"name"`
}

// ListBranchesWithCommitCount returns every branch of a repo with its exact
// commit count, used to verify a derived template is fully squashed. It pages
// through the full branch list so a template with many branches is checked in
// full, not just its first page.
func (c *restClient) ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]BranchCount, error) {
	branches, err := getPaged[branchRef](ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/branches?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	})
	if err != nil {
		return nil, err
	}
	out := make([]BranchCount, 0, len(branches))
	for _, b := range branches {
		n, err := c.branchCommitCount(ctx, owner, repo, b.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, BranchCount{Name: b.Name, Commits: n})
	}
	return out, nil
}

// branchCommitCount returns the exact number of commits on a branch using a
// single request: it reads the last-page number from the Link header of a
// one-per-page commit listing. A branch with one commit has no Link header.
func (c *restClient) branchCommitCount(ctx context.Context, owner, repo, branch string) (int, error) {
	path := fmt.Sprintf("repos/%s/%s/commits?sha=%s&per_page=1",
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(branch))
	hdr, err := c.do(ctx, "GET", path, nil, nil)
	if err != nil {
		return 0, err
	}
	return lastPage(hdr.Get("Link")), nil
}

// lastPage extracts the rel="last" page number from a Link header, which (for a
// one-per-page listing) equals the total count. No such link means a single
// page, i.e. one item. The page value is read from the URL's query so it is not
// confused by the per_page parameter, which also contains "page=".
func lastPage(link string) int {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="last"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end < start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(u.Query().Get("page")); err == nil {
			return n
		}
	}
	return 1
}
