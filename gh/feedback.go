package gh

import (
	"context"
	"fmt"
	"net/url"
)

// GetRef returns the commit SHA a ref points at. ref is given without the
// leading "refs/", e.g. "heads/main".
func (c *restClient) GetRef(ctx context.Context, owner, repo, ref string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	// ref segments (e.g. heads/main) are part of the path and must not be escaped.
	path := fmt.Sprintf("repos/%s/%s/git/ref/%s", url.PathEscape(owner), url.PathEscape(repo), ref)
	if _, err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

// CreateRef creates a git ref. ref is fully qualified, e.g.
// "refs/heads/feedback".
func (c *restClient) CreateRef(ctx context.Context, owner, repo, ref, sha string) error {
	path := fmt.Sprintf("repos/%s/%s/git/refs", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "POST", path, map[string]any{"ref": ref, "sha": sha}, nil)
	return err
}

// BranchExists reports whether a branch exists in the repository. branch is the
// short name, e.g. "feedback".
func (c *restClient) BranchExists(ctx context.Context, owner, repo, branch string) (bool, error) {
	// ref segments (heads/<branch>) are part of the path and must not be escaped.
	path := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", url.PathEscape(owner), url.PathEscape(repo), branch)
	if _, err := c.do(ctx, "GET", path, nil, nil); err != nil {
		// 404 means the repo has commits but not this branch; 409 ("Git Repository
		// is empty") means it has no commits at all. Either way the branch is
		// absent — a normal answer, not a failure — which also lets a freshly
		// generated repo be polled until its starter commit lands.
		if notFound(err) || emptyRepo(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CreatePR opens a pull request.
func (c *restClient) CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error {
	path := fmt.Sprintf("repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "POST", path, map[string]any{
		"title": title, "head": head, "base": base, "body": body,
	}, nil)
	return err
}

// PRExists reports whether any pull request (open or closed) targets base in the
// repository. The feedback PR is the only one whose base is the feedback branch,
// so this detects an already-created feedback PR without reopening a closed one.
func (c *restClient) PRExists(ctx context.Context, owner, repo, base string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls?state=all&base=%s&per_page=1",
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(base))
	var prs []struct {
		Number int `json:"number"`
	}
	if _, err := c.do(ctx, "GET", path, nil, &prs); err != nil {
		return false, err
	}
	return len(prs) > 0, nil
}

// EnableIssues turns on the Issues feature for a repository.
func (c *restClient) EnableIssues(ctx context.Context, owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "PATCH", path, map[string]any{"has_issues": true}, nil)
	return err
}

// CreateIssue opens an issue.
func (c *restClient) CreateIssue(ctx context.Context, owner, repo, title, body string) error {
	path := fmt.Sprintf("repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "POST", path, map[string]any{"title": title, "body": body}, nil)
	return err
}

// IssueExists reports whether an issue (open or closed) with the given title
// exists in the repository. The issues endpoint also lists pull requests, which
// carry a pull_request field and are skipped.
func (c *restClient) IssueExists(ctx context.Context, owner, repo, title string) (bool, error) {
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		var batch []struct {
			Title       string    `json:"title"`
			PullRequest *struct{} `json:"pull_request"`
		}
		if _, err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return false, err
		}
		for _, it := range batch {
			if it.PullRequest == nil && it.Title == title {
				return true, nil
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return false, nil
}
