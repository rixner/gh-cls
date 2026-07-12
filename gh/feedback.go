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

// emptyTreeSHA is git's canonical empty tree object, the same constant every git
// install computes for a tree with no entries. GitHub materializes it on demand
// when a commit is created against it, so it can be referenced directly without
// first creating the object -- essential here, because POST git/trees rejects a
// zero-entry tree with "422 Invalid tree info". It is the tree of the empty root
// commit the feedback base is built on (see RebaseOntoEmptyRoot).
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// RebaseOntoEmptyRoot rebuilds branch's initial commit on top of a new empty root
// commit and returns the root's SHA. The branch's tree is unchanged -- its files
// are identical -- so only an empty parent commit is inserted beneath its history.
//
// This is what makes the feedback PR possible. GitHub opens a pull request only
// between branches that share history and refuses one whose branches have no
// common ancestor, so the feedback base cannot be a detached orphan. Pointing the
// feedback branch at the returned root instead gives it a shared ancestor with
// branch (so the PR opens) while keeping their merge base empty (so the whole
// project shows as additions). This mirrors how GitHub Classroom builds its
// feedback PR.
//
// It force-updates branch, so it must run before branch protection is applied and
// only on a repo with no history worth preserving -- a freshly generated one.
func (c *restClient) RebaseOntoEmptyRoot(ctx context.Context, owner, repo, branch string) (string, error) {
	ref := "heads/" + branch
	tip, err := c.GetRef(ctx, owner, repo, ref)
	if err != nil {
		return "", fmt.Errorf("reading %s tip in %s/%s: %w", branch, owner, repo, err)
	}
	tree, message, err := c.getCommit(ctx, owner, repo, tip)
	if err != nil {
		return "", fmt.Errorf("reading %s commit in %s/%s: %w", branch, owner, repo, err)
	}
	root, err := c.createCommit(ctx, owner, repo, message, emptyTreeSHA, nil)
	if err != nil {
		return "", fmt.Errorf("creating empty root commit in %s/%s: %w", owner, repo, err)
	}
	rebased, err := c.createCommit(ctx, owner, repo, message, tree, []string{root})
	if err != nil {
		return "", fmt.Errorf("rebasing %s onto the empty root in %s/%s: %w", branch, owner, repo, err)
	}
	if err := c.updateRef(ctx, owner, repo, ref, rebased, true); err != nil {
		return "", fmt.Errorf("moving %s onto the empty root in %s/%s: %w", branch, owner, repo, err)
	}
	// Post-condition: the branch resolves to the rebased commit. GitHub's ref
	// reads lag its writes, so poll rather than read once. A move that never lands
	// (e.g. a protected branch) fails here instead of surfacing later as a
	// confusing "no history in common" PR error.
	if err := c.awaitConsistent(ctx, func() (bool, error) {
		got, err := c.GetRef(ctx, owner, repo, ref)
		return got == rebased, err
	}); err != nil {
		return "", fmt.Errorf("moving %s onto the empty root in %s/%s did not land (%w); check for branch protection", branch, owner, repo, err)
	}
	return root, nil
}

// getCommit returns a commit's tree SHA and message.
func (c *restClient) getCommit(ctx context.Context, owner, repo, sha string) (tree, message string, err error) {
	var out struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Message string `json:"message"`
	}
	path := fmt.Sprintf("repos/%s/%s/git/commits/%s", url.PathEscape(owner), url.PathEscape(repo), sha)
	if _, err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return "", "", err
	}
	if out.Tree.SHA == "" {
		return "", "", fmt.Errorf("commit %s in %s/%s has no tree", sha, owner, repo)
	}
	return out.Tree.SHA, out.Message, nil
}

// createCommit creates a commit with the given tree and parents (an empty parents
// list makes a root commit) and returns its SHA.
func (c *restClient) createCommit(ctx context.Context, owner, repo, message, tree string, parents []string) (string, error) {
	if parents == nil {
		parents = []string{}
	}
	path := fmt.Sprintf("repos/%s/%s/git/commits", url.PathEscape(owner), url.PathEscape(repo))
	var out struct {
		SHA string `json:"sha"`
	}
	body := map[string]any{"message": message, "tree": tree, "parents": parents}
	if _, err := c.do(ctx, "POST", path, body, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("creating commit in %s/%s returned no SHA", owner, repo)
	}
	return out.SHA, nil
}

// updateRef points an existing ref at sha. ref is given without the leading
// "refs/", e.g. "heads/main". force permits a non-fast-forward move.
func (c *restClient) updateRef(ctx context.Context, owner, repo, ref, sha string, force bool) error {
	path := fmt.Sprintf("repos/%s/%s/git/refs/%s", url.PathEscape(owner), url.PathEscape(repo), ref)
	_, err := c.do(ctx, "PATCH", path, map[string]any{"sha": sha, "force": force}, nil)
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

// FindPRByBase returns the number and state ("open"/"closed") of a pull request
// targeting base in the repository. The feedback PR is the only one whose base is
// the feedback branch, so this locates an already-created feedback PR without
// reopening a closed one. found is false (with a nil error) when none matches.
func (c *restClient) FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls?state=all&base=%s&per_page=1",
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(base))
	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if _, err := c.do(ctx, "GET", path, nil, &prs); err != nil {
		return 0, "", false, err
	}
	if len(prs) == 0 {
		return 0, "", false, nil
	}
	return prs[0].Number, prs[0].State, true, nil
}

// PRExists reports whether any pull request (any state) targets base.
func (c *restClient) PRExists(ctx context.Context, owner, repo, base string) (bool, error) {
	_, _, found, err := c.FindPRByBase(ctx, owner, repo, base)
	return found, err
}

// EnableIssues turns on the Issues feature for a repository.
func (c *restClient) EnableIssues(ctx context.Context, owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "PATCH", path, map[string]any{"has_issues": true}, nil)
	return err
}

// consistencyChecks bounds how often a just-written resource is re-read while
// waiting for GitHub's eventually-consistent read endpoints to reflect it. A
// write is confirmed by re-reading rather than trusted.
const consistencyChecks = 5

// awaitConsistent polls check until it reports true, waiting between attempts so
// GitHub's eventually-consistent reads can catch up with a just-applied write. It
// returns a generic error after consistencyChecks failed attempts; callers wrap
// it with what was being confirmed. The wait is the retry policy's, a no-op under
// test.
func (c *restClient) awaitConsistent(ctx context.Context, check func() (bool, error)) error {
	for attempt := 1; ; attempt++ {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if attempt >= consistencyChecks {
			return fmt.Errorf("still not reflected after %d checks", attempt)
		}
		if err := c.policy.wait(ctx, backoff(attempt)); err != nil {
			return err
		}
	}
}

// CreateIssue opens an issue and confirms it is listable before returning.
// GitHub's issue-list index lags briefly behind the create, so a caller that
// immediately looks the issue up by title -- as the feedback command does on its
// own run -- can otherwise miss it and fail with "no feedback issue". Confirming
// it through the same query consumers use makes the post-condition hold before
// assign reports success.
func (c *restClient) CreateIssue(ctx context.Context, owner, repo, title, body string) error {
	path := fmt.Sprintf("repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(repo))
	if _, err := c.do(ctx, "POST", path, map[string]any{"title": title, "body": body}, nil); err != nil {
		return err
	}
	if err := c.awaitConsistent(ctx, func() (bool, error) {
		return c.IssueExists(ctx, owner, repo, title)
	}); err != nil {
		return fmt.Errorf("issue %q created in %s/%s but %w; re-run assign to continue", title, owner, repo, err)
	}
	return nil
}

// FindIssueByTitle returns the number and state ("open"/"closed") of an issue
// with the given title. The issues endpoint also lists pull requests, which carry
// a pull_request field and are skipped. found is false (with a nil error) when no
// issue matches.
func (c *restClient) FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error) {
	type issue struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		State       string    `json:"state"`
		PullRequest *struct{} `json:"pull_request"`
	}
	it, found, err := selectPaged(ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	}, func(it issue) bool {
		return it.PullRequest == nil && it.Title == title
	})
	return it.Number, it.State, found, err
}

// IssueExists reports whether an issue (any state) with the given title exists.
func (c *restClient) IssueExists(ctx context.Context, owner, repo, title string) (bool, error) {
	_, _, found, err := c.FindIssueByTitle(ctx, owner, repo, title)
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
