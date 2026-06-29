package gh

import (
	"context"
	"fmt"
	"net/url"
	"strings"
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
// "refs/heads/feedback". It re-reads the ref to confirm the create took effect:
// the next step opens the feedback PR against this branch, so a ref that did not
// actually land would otherwise fail later with a confusing error.
func (c *restClient) CreateRef(ctx context.Context, owner, repo, ref, sha string) error {
	path := fmt.Sprintf("repos/%s/%s/git/refs", url.PathEscape(owner), url.PathEscape(repo))
	if _, err := c.do(ctx, "POST", path, map[string]any{"ref": ref, "sha": sha}, nil); err != nil {
		return err
	}
	// Post-condition: the ref resolves to the requested SHA. GetRef takes the ref
	// without the leading "refs/".
	got, err := c.GetRef(ctx, owner, repo, strings.TrimPrefix(ref, "refs/"))
	if err != nil {
		return fmt.Errorf("verifying created ref %s in %s/%s: %w", ref, owner, repo, err)
	}
	if got != sha {
		return fmt.Errorf("created ref %s in %s/%s resolves to %s, want %s; create it manually", ref, owner, repo, got, sha)
	}
	return nil
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

// FindPRByBase returns the number of a pull request (open or closed) targeting
// base in the repository. The feedback PR is the only one whose base is the
// feedback branch, so this locates an already-created feedback PR without
// reopening a closed one. found is false (with a nil error) when none matches.
func (c *restClient) FindPRByBase(ctx context.Context, owner, repo, base string) (int, bool, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls?state=all&base=%s&per_page=1",
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(base))
	var prs []struct {
		Number int `json:"number"`
	}
	if _, err := c.do(ctx, "GET", path, nil, &prs); err != nil {
		return 0, false, err
	}
	if len(prs) == 0 {
		return 0, false, nil
	}
	return prs[0].Number, true, nil
}

// PRExists reports whether any pull request (any state) targets base.
func (c *restClient) PRExists(ctx context.Context, owner, repo, base string) (bool, error) {
	_, found, err := c.FindPRByBase(ctx, owner, repo, base)
	return found, err
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

// FindIssueByTitle returns the number of an issue (open or closed) with the
// given title. The issues endpoint also lists pull requests, which carry a
// pull_request field and are skipped. found is false (with a nil error) when no
// issue matches.
func (c *restClient) FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, bool, error) {
	type issue struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		PullRequest *struct{} `json:"pull_request"`
	}
	it, found, err := selectPaged(ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	}, func(it issue) bool {
		return it.PullRequest == nil && it.Title == title
	})
	return it.Number, found, err
}

// IssueExists reports whether an issue (any state) with the given title exists.
func (c *restClient) IssueExists(ctx context.Context, owner, repo, title string) (bool, error) {
	_, found, err := c.FindIssueByTitle(ctx, owner, repo, title)
	return found, err
}

// Comment is the subset of an issue or pull-request comment the tool inspects.
type Comment struct {
	Body string `json:"body"`
}

// ListIssueComments returns every comment on an issue or pull request. The
// issue-comments endpoint serves pull requests too (a PR is an issue), so it
// covers both feedback modes.
func (c *restClient) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]Comment, error) {
	return getPaged[Comment](ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), number, pageSize, page)
	})
}

// AddComment posts a comment to an issue or pull request and returns its HTML
// URL. The issue-comments endpoint serves pull requests too. The URL is read
// back from the response so the caller can confirm the comment actually landed.
func (c *restClient) AddComment(ctx context.Context, owner, repo string, number int, body string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(repo), number)
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := c.do(ctx, "POST", path, map[string]any{"body": body}, &out); err != nil {
		return "", err
	}
	if out.HTMLURL == "" {
		return "", fmt.Errorf("comment on %s/%s#%d returned no URL; it may not have been created", owner, repo, number)
	}
	return out.HTMLURL, nil
}
