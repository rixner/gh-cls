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

// ListBranchesWithCommitCount returns every branch of a repo with its exact
// commit count, used to verify a derived template is fully squashed.
func (c *restClient) ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]BranchCount, error) {
	var branches []struct {
		Name string `json:"name"`
	}
	path := fmt.Sprintf("repos/%s/%s/branches?per_page=100", url.PathEscape(owner), url.PathEscape(repo))
	if _, err := c.do(ctx, "GET", path, nil, &branches); err != nil {
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
// page, i.e. one item.
func lastPage(link string) int {
	if link == "" {
		return 1
	}
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="last"`) {
			continue
		}
		i := strings.Index(part, "page=")
		if i < 0 {
			continue
		}
		digits := part[i+len("page="):]
		end := 0
		for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
			end++
		}
		if n, err := strconv.Atoi(digits[:end]); err == nil {
			return n
		}
	}
	return 1
}
