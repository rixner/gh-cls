// Package gh is the GitHub API surface the tool depends on, expressed as domain
// operations rather than raw HTTP. It is built entirely on go-gh's public API;
// the Client interface is the seam that lets commands run against a fake in
// tests.
package gh

import "context"

// Client is the set of GitHub operations the commands use. It grows as
// commands need more endpoints. Commands depend on narrow subsets of it (see
// each command's own interface), so this full surface exists to document the
// implementation's contract and as the type New returns.
type Client interface {
	// OrgRole returns the authenticated user's membership role in org; "admin"
	// for owners. Used to guard org-mutating commands.
	OrgRole(ctx context.Context, org string) (string, error)

	// GetRepo fetches a repository. The bool is false (with a nil error) when
	// the repo does not exist.
	GetRepo(ctx context.Context, owner, name string) (*Repo, bool, error)
	// CreateOrgRepo creates an empty repository in the org.
	CreateOrgRepo(ctx context.Context, org, name string, private bool) (*Repo, error)
	// SetRepoTemplate marks a repository as a template repository.
	SetRepoTemplate(ctx context.Context, owner, name string) error
	// DeleteRepo deletes a repository.
	DeleteRepo(ctx context.Context, org, name string) error

	// GetOrg reads current organization settings.
	GetOrg(ctx context.Context, org string) (*OrgSettings, error)
	// PatchOrg updates organization settings with the given fields.
	PatchOrg(ctx context.Context, org string, fields map[string]any) error
	// GetActionsPermissions reads the org-wide Actions policy.
	GetActionsPermissions(ctx context.Context, org string) (*ActionsPermissions, error)
	// SetActionsEnabledRepositories sets which repositories may run Actions org-wide.
	SetActionsEnabledRepositories(ctx context.Context, org, value string) error
	// CopilotSeatCount reports purchased Copilot seats; present is false on a
	// free org with no subscription.
	CopilotSeatCount(ctx context.Context, org string) (count int, present bool, err error)
	// GetTeam fetches a team by slug; the bool reports existence.
	GetTeam(ctx context.Context, org, slug string) (*Team, bool, error)
	// CreateTeam creates a closed team.
	CreateTeam(ctx context.Context, org, name string) (*Team, error)
	// AddTeamRepo grants a team the given permission on a repository.
	AddTeamRepo(ctx context.Context, org, teamSlug, owner, repo, permission string) error

	// ListBranchesWithCommitCount returns every branch with its exact commit count.
	ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]BranchCount, error)
	// GenerateFromTemplate creates owner/name from a template repository.
	GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	// AddCollaborator grants a user the given permission on a repository.
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error

	// ApplyRuleset applies the all-branches force-push/deletion-blocking ruleset.
	ApplyRuleset(ctx context.Context, org, repo string, staffTeamID int64) error
	// GetRef returns the SHA a ref (e.g. "heads/main") points at.
	GetRef(ctx context.Context, owner, repo, ref string) (string, error)
	// CreateRef creates a fully-qualified ref (e.g. "refs/heads/feedback").
	CreateRef(ctx context.Context, owner, repo, ref, sha string) error
	// BranchExists reports whether a branch (short name) exists.
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	// CreatePR opens a pull request.
	CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error
	// PRExists reports whether any pull request (any state) targets base.
	PRExists(ctx context.Context, owner, repo, base string) (bool, error)
	// EnableIssues turns on the Issues feature for a repository.
	EnableIssues(ctx context.Context, owner, repo string) error
	// CreateIssue opens an issue.
	CreateIssue(ctx context.Context, owner, repo, title, body string) error
	// IssueExists reports whether an issue (any state) with title exists.
	IssueExists(ctx context.Context, owner, repo, title string) (bool, error)

	// ListOrgReposByPrefix returns org repos whose name starts with prefix.
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]Repo, error)
	// ListDirectCollaborators returns a repo's direct collaborators.
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]Collaborator, error)
}

// Repo is the subset of a repository's fields the tool inspects.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	IsTemplate    bool   `json:"is_template"`
	HasIssues     bool   `json:"has_issues"`
	CloneURL      string `json:"clone_url"`
}

// restClient must satisfy Client.
var _ Client = (*restClient)(nil)
