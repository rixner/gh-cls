package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/rixner/gh-cls/gh"
)

// repoReady is the client slice needed to confirm a generated repository is
// usable: that it exists and its default branch has actually landed.
type repoReady interface {
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
}

// waitRepoReady polls until owner/repo exists with a populated default branch,
// returning the repo. Repository generation from a template is asynchronous, so
// the repo object can appear before its starter commit lands; confirming the
// default branch resolves means later steps act on real content rather than an
// empty shell — whether that is creating a feedback ref on a student repo or
// generating student repos from a freshly built template.
func waitRepoReady(ctx context.Context, c repoReady, sleep func(time.Duration), owner, repo string) (*gh.Repo, error) {
	for i := 0; i < readyAttempts; i++ {
		// A real API error (auth, permissions, a 5xx that outlived its retries) is
		// surfaced immediately rather than swallowed: polling through it would hide
		// the actual cause behind a generic "did not become ready" timeout. Only a
		// not-yet-populated repo (no error, just no default branch) keeps polling.
		r, exists, err := c.GetRepo(ctx, owner, repo)
		if err != nil {
			return nil, fmt.Errorf("waiting for %s/%s to become ready: %w", owner, repo, err)
		}
		if exists && r.DefaultBranch != "" {
			ok, err := c.BranchExists(ctx, owner, repo, r.DefaultBranch)
			if err != nil {
				return nil, fmt.Errorf("waiting for %s/%s to become ready: %w", owner, repo, err)
			}
			if ok {
				return r, nil
			}
		}
		sleep(readyDelay)
	}
	return nil, fmt.Errorf("repository %s/%s did not become ready after generation (no populated default branch after %d attempts)", owner, repo, readyAttempts)
}
