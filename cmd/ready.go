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
		if r, exists, err := c.GetRepo(ctx, owner, repo); err == nil && exists && r.DefaultBranch != "" {
			if ok, err := c.BranchExists(ctx, owner, repo, r.DefaultBranch); err == nil && ok {
				return r, nil
			}
		}
		sleep(readyDelay)
	}
	return nil, fmt.Errorf("repository %s/%s did not become ready after generation (no populated default branch after %d attempts)", owner, repo, readyAttempts)
}
