package gh

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Activity types, as GitHub reports them.
const (
	ActivityPush            = "push"
	ActivityForcePush       = "force_push"
	ActivityBranchCreation  = "branch_creation"
	ActivityBranchDeletion  = "branch_deletion"
	ActivityPRMerge         = "pr_merge"
	ActivityMergeQueueMerge = "merge_queue_merge"
)

// Activity is one recorded change to a repository's refs: who moved a branch,
// when, and between which commits.
//
// Timestamp is GitHub's own server-side record of when the change happened. It
// is not a commit date (which the pusher controls and can backdate) and not a
// webhook receipt time (which trails the push by delivery latency, and by far
// more when GitHub retries a failed delivery). That makes it the trustworthy
// basis for "what did this repository look like at time T".
type Activity struct {
	ID           int64     `json:"id"`
	Ref          string    `json:"ref"`
	Timestamp    time.Time `json:"timestamp"`
	ActivityType string    `json:"activity_type"`
	// Before and After are the commits the ref moved between. After is all
	// zeroes for a deletion; Before is all zeroes for a creation.
	Before string `json:"before"`
	After  string `json:"after"`
	// Actor is null for some activities, which decodes to an empty Login.
	Actor struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// Branch returns the short branch name this activity touched.
func (a Activity) Branch() string { return strings.TrimPrefix(a.Ref, "refs/heads/") }

// SetsTip reports whether this activity left After as the branch's new tip. A
// deletion does not (its After is all zeroes), so it can never be the answer to
// "what was the tip at time T".
func (a Activity) SetsTip() bool {
	switch a.ActivityType {
	case ActivityPush, ActivityForcePush, ActivityBranchCreation, ActivityPRMerge, ActivityMergeQueueMerge:
		return a.After != "" && strings.Trim(a.After, "0") != ""
	}
	return false
}

// ListRepoActivity returns a repository's recorded ref changes, newest first,
// optionally restricted to one ref ("refs/heads/main").
//
// GitHub documents no retention, completeness or latency guarantee for this
// endpoint, so callers that depend on it should verify rather than assume: see
// the freshness check in `gh cls activity`.
func (c *restClient) ListRepoActivity(ctx context.Context, owner, repo, ref string) ([]Activity, error) {
	base := fmt.Sprintf("repos/%s/%s/activity?per_page=%d",
		url.PathEscape(owner), url.PathEscape(repo), pageSize)
	if ref != "" {
		base += "&ref=" + url.QueryEscape(ref)
	}
	return getCursorPaged[Activity](ctx, c, base)
}

// CommitExists reports whether a commit is still retrievable from a repository.
// A commit orphaned by a force push and since collected is gone, which is the
// case that turns a pinned SHA into an artifact that cannot be collected.
func (c *restClient) CommitExists(ctx context.Context, owner, repo, sha string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	if _, err := c.do(ctx, "GET", path, nil, nil); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
