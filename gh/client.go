// Package gh is the GitHub API surface the tool depends on, expressed as domain
// operations rather than raw HTTP. It is built entirely on go-gh's public API;
// the Client interface is the seam that lets commands run against a fake in
// tests.
package gh

import "context"

// Client is the set of GitHub operations the commands use. It grows as
// commands need more endpoints.
type Client interface {
	// OrgRole returns the authenticated user's membership role in org; "admin"
	// for owners. Used to guard org-mutating commands.
	OrgRole(ctx context.Context, org string) (string, error)

	// GetRepo fetches a repository. The bool is false (with a nil error) when
	// the repo does not exist.
	GetRepo(ctx context.Context, owner, name string) (*Repo, bool, error)
}

// Repo is the subset of a repository's fields the tool inspects.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	IsTemplate    bool   `json:"is_template"`
	HasIssues     bool   `json:"has_issues"`
}

// restClient must satisfy Client.
var _ Client = (*restClient)(nil)
