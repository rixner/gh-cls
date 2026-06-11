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

// CreatePR opens a pull request.
func (c *restClient) CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error {
	path := fmt.Sprintf("repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "POST", path, map[string]any{
		"title": title, "head": head, "base": base, "body": body,
	}, nil)
	return err
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
