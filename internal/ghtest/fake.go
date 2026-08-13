// Package ghtest provides a single, concurrency-safe fake implementation of
// gh.Client for command tests, replacing one hand-rolled fake per test file.
// A test builds a Fake and sets only the *Func fields the command under test
// calls; every other method panics if invoked, so a forgotten dependency
// fails loudly instead of silently returning a zero value.
package ghtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/rixner/gh-cls/gh"
)

// Fake is a stand-in for gh.Client. Each method records its name in Calls
// (guarded by a mutex, since runConcurrent hits fakes from multiple
// goroutines) and then delegates to the matching *Func field. Tests that need
// their own locking around captured state can reuse Fake's mutex via
// Lock/Unlock instead of declaring their own.
type Fake struct {
	mu    sync.Mutex
	Calls []string

	OrgRoleFunc                       func(ctx context.Context, org string) (string, error)
	UserExistsFunc                    func(ctx context.Context, username string) (bool, error)
	GetRepoFunc                       func(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	SetRepoTemplateFunc               func(ctx context.Context, owner, name string) error
	DeleteRepoFunc                    func(ctx context.Context, org, name string) error
	GetOrgFunc                        func(ctx context.Context, org string) (*gh.OrgSettings, error)
	PatchOrgFunc                      func(ctx context.Context, org string, fields map[string]any) error
	GetActionsPermissionsFunc         func(ctx context.Context, org string) (*gh.ActionsPermissions, error)
	SetActionsEnabledRepositoriesFunc func(ctx context.Context, org, value string) error
	CopilotSeatCountFunc              func(ctx context.Context, org string) (int, bool, error)
	GetTeamFunc                       func(ctx context.Context, org, slug string) (*gh.Team, bool, error)
	CreateTeamFunc                    func(ctx context.Context, org, name string) (*gh.Team, error)
	AddTeamRepoFunc                   func(ctx context.Context, org, teamSlug, owner, repo, permission string) error
	ListTeamMembersFunc               func(ctx context.Context, org, slug string) ([]string, error)
	AddTeamMembershipFunc             func(ctx context.Context, org, slug, username string) (string, error)
	RemoveTeamMembershipFunc          func(ctx context.Context, org, slug, username string) error
	ListBranchesWithCommitCountFunc   func(ctx context.Context, owner, repo string) ([]gh.BranchCount, error)
	GenerateFromTemplateFunc          func(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	AddCollaboratorFunc               func(ctx context.Context, owner, repo, username, permission string) error
	ApplyRulesetFunc                  func(ctx context.Context, org, repo string) error
	GetRefFunc                        func(ctx context.Context, owner, repo, ref string) (string, error)
	CreateRefFunc                     func(ctx context.Context, owner, repo, ref, sha string) error
	RebaseOntoEmptyRootFunc           func(ctx context.Context, owner, repo, branch string) (string, error)
	BranchExistsFunc                  func(ctx context.Context, owner, repo, branch string) (bool, error)
	CreatePRFunc                      func(ctx context.Context, owner, repo, title, head, base, body string) error
	PRExistsFunc                      func(ctx context.Context, owner, repo, base string) (bool, error)
	FindPRByBaseFunc                  func(ctx context.Context, owner, repo, base string) (int, string, bool, error)
	EnableIssuesFunc                  func(ctx context.Context, owner, repo string) error
	CreateIssueFunc                   func(ctx context.Context, owner, repo, title, body string) error
	IssueExistsFunc                   func(ctx context.Context, owner, repo, title string) (bool, error)
	FindIssueByTitleFunc              func(ctx context.Context, owner, repo, title string) (int, string, bool, error)
	ListIssueCommentsFunc             func(ctx context.Context, owner, repo string, number int) ([]gh.Comment, error)
	AddCommentFunc                    func(ctx context.Context, owner, repo string, number int, body string) (string, error)
	ListOrgReposByPrefixFunc          func(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListDirectCollaboratorsFunc       func(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	ListRepoInvitationsFunc           func(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
	DeleteRepoInvitationFunc          func(ctx context.Context, owner, repo string, id int64) error
	UpdateRepoInvitationFunc          func(ctx context.Context, owner, repo string, id int64, permission string) error
	GetPropertyDefinitionFunc         func(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	SetPropertyDefinitionFunc         func(ctx context.Context, org string, def gh.PropertyDefinition) error
	ListRepoPropertyValuesFunc        func(ctx context.Context, org string) (map[string]map[string]string, error)
	GetRepoPropertyValuesFunc         func(ctx context.Context, org, repo string) (map[string]string, error)
	SetRepoPropertyValueFunc          func(ctx context.Context, org, repo, name, value string) error
}

var _ gh.Client = (*Fake)(nil)

// Lock and Unlock guard Calls; tests may reuse them to protect their own
// captured state instead of declaring a separate mutex.
func (f *Fake) Lock()   { f.mu.Lock() }
func (f *Fake) Unlock() { f.mu.Unlock() }

// record appends name to Calls under the shared lock.
func (f *Fake) record(name string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, name)
	f.mu.Unlock()
}

func missing(name string) {
	panic(fmt.Sprintf("ghtest.Fake: %sFunc not set", name))
}

func (f *Fake) OrgRole(ctx context.Context, org string) (string, error) {
	f.record("OrgRole")
	if f.OrgRoleFunc == nil {
		missing("OrgRole")
	}
	return f.OrgRoleFunc(ctx, org)
}

func (f *Fake) UserExists(ctx context.Context, username string) (bool, error) {
	f.record("UserExists")
	if f.UserExistsFunc == nil {
		missing("UserExists")
	}
	return f.UserExistsFunc(ctx, username)
}

func (f *Fake) GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error) {
	f.record("GetRepo")
	if f.GetRepoFunc == nil {
		missing("GetRepo")
	}
	return f.GetRepoFunc(ctx, owner, name)
}

func (f *Fake) SetRepoTemplate(ctx context.Context, owner, name string) error {
	f.record("SetRepoTemplate")
	if f.SetRepoTemplateFunc == nil {
		missing("SetRepoTemplate")
	}
	return f.SetRepoTemplateFunc(ctx, owner, name)
}

func (f *Fake) DeleteRepo(ctx context.Context, org, name string) error {
	f.record("DeleteRepo")
	if f.DeleteRepoFunc == nil {
		missing("DeleteRepo")
	}
	return f.DeleteRepoFunc(ctx, org, name)
}

func (f *Fake) GetOrg(ctx context.Context, org string) (*gh.OrgSettings, error) {
	f.record("GetOrg")
	if f.GetOrgFunc == nil {
		missing("GetOrg")
	}
	return f.GetOrgFunc(ctx, org)
}

func (f *Fake) PatchOrg(ctx context.Context, org string, fields map[string]any) error {
	f.record("PatchOrg")
	if f.PatchOrgFunc == nil {
		missing("PatchOrg")
	}
	return f.PatchOrgFunc(ctx, org, fields)
}

func (f *Fake) GetActionsPermissions(ctx context.Context, org string) (*gh.ActionsPermissions, error) {
	f.record("GetActionsPermissions")
	if f.GetActionsPermissionsFunc == nil {
		missing("GetActionsPermissions")
	}
	return f.GetActionsPermissionsFunc(ctx, org)
}

func (f *Fake) SetActionsEnabledRepositories(ctx context.Context, org, value string) error {
	f.record("SetActionsEnabledRepositories")
	if f.SetActionsEnabledRepositoriesFunc == nil {
		missing("SetActionsEnabledRepositories")
	}
	return f.SetActionsEnabledRepositoriesFunc(ctx, org, value)
}

func (f *Fake) CopilotSeatCount(ctx context.Context, org string) (int, bool, error) {
	f.record("CopilotSeatCount")
	if f.CopilotSeatCountFunc == nil {
		missing("CopilotSeatCount")
	}
	return f.CopilotSeatCountFunc(ctx, org)
}

func (f *Fake) GetTeam(ctx context.Context, org, slug string) (*gh.Team, bool, error) {
	f.record("GetTeam")
	if f.GetTeamFunc == nil {
		missing("GetTeam")
	}
	return f.GetTeamFunc(ctx, org, slug)
}

func (f *Fake) CreateTeam(ctx context.Context, org, name string) (*gh.Team, error) {
	f.record("CreateTeam")
	if f.CreateTeamFunc == nil {
		missing("CreateTeam")
	}
	return f.CreateTeamFunc(ctx, org, name)
}

func (f *Fake) AddTeamRepo(ctx context.Context, org, teamSlug, owner, repo, permission string) error {
	f.record("AddTeamRepo")
	if f.AddTeamRepoFunc == nil {
		missing("AddTeamRepo")
	}
	return f.AddTeamRepoFunc(ctx, org, teamSlug, owner, repo, permission)
}

func (f *Fake) ListTeamMembers(ctx context.Context, org, slug string) ([]string, error) {
	f.record("ListTeamMembers")
	if f.ListTeamMembersFunc == nil {
		missing("ListTeamMembers")
	}
	return f.ListTeamMembersFunc(ctx, org, slug)
}

func (f *Fake) AddTeamMembership(ctx context.Context, org, slug, username string) (string, error) {
	f.record("AddTeamMembership")
	if f.AddTeamMembershipFunc == nil {
		missing("AddTeamMembership")
	}
	return f.AddTeamMembershipFunc(ctx, org, slug, username)
}

func (f *Fake) RemoveTeamMembership(ctx context.Context, org, slug, username string) error {
	f.record("RemoveTeamMembership")
	if f.RemoveTeamMembershipFunc == nil {
		missing("RemoveTeamMembership")
	}
	return f.RemoveTeamMembershipFunc(ctx, org, slug, username)
}

func (f *Fake) ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]gh.BranchCount, error) {
	f.record("ListBranchesWithCommitCount")
	if f.ListBranchesWithCommitCountFunc == nil {
		missing("ListBranchesWithCommitCount")
	}
	return f.ListBranchesWithCommitCountFunc(ctx, owner, repo)
}

func (f *Fake) GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error {
	f.record("GenerateFromTemplate")
	if f.GenerateFromTemplateFunc == nil {
		missing("GenerateFromTemplate")
	}
	return f.GenerateFromTemplateFunc(ctx, tmplOwner, tmplRepo, owner, name, private, includeAllBranches)
}

func (f *Fake) AddCollaborator(ctx context.Context, owner, repo, username, permission string) error {
	f.record("AddCollaborator")
	if f.AddCollaboratorFunc == nil {
		missing("AddCollaborator")
	}
	return f.AddCollaboratorFunc(ctx, owner, repo, username, permission)
}

func (f *Fake) ApplyRuleset(ctx context.Context, org, repo string) error {
	f.record("ApplyRuleset")
	if f.ApplyRulesetFunc == nil {
		missing("ApplyRuleset")
	}
	return f.ApplyRulesetFunc(ctx, org, repo)
}

func (f *Fake) GetRef(ctx context.Context, owner, repo, ref string) (string, error) {
	f.record("GetRef")
	if f.GetRefFunc == nil {
		missing("GetRef")
	}
	return f.GetRefFunc(ctx, owner, repo, ref)
}

func (f *Fake) CreateRef(ctx context.Context, owner, repo, ref, sha string) error {
	f.record("CreateRef")
	if f.CreateRefFunc == nil {
		missing("CreateRef")
	}
	return f.CreateRefFunc(ctx, owner, repo, ref, sha)
}

func (f *Fake) RebaseOntoEmptyRoot(ctx context.Context, owner, repo, branch string) (string, error) {
	f.record("RebaseOntoEmptyRoot")
	if f.RebaseOntoEmptyRootFunc == nil {
		missing("RebaseOntoEmptyRoot")
	}
	return f.RebaseOntoEmptyRootFunc(ctx, owner, repo, branch)
}

func (f *Fake) BranchExists(ctx context.Context, owner, repo, branch string) (bool, error) {
	f.record("BranchExists")
	if f.BranchExistsFunc == nil {
		missing("BranchExists")
	}
	return f.BranchExistsFunc(ctx, owner, repo, branch)
}

func (f *Fake) CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error {
	f.record("CreatePR")
	if f.CreatePRFunc == nil {
		missing("CreatePR")
	}
	return f.CreatePRFunc(ctx, owner, repo, title, head, base, body)
}

func (f *Fake) PRExists(ctx context.Context, owner, repo, base string) (bool, error) {
	f.record("PRExists")
	if f.PRExistsFunc == nil {
		missing("PRExists")
	}
	return f.PRExistsFunc(ctx, owner, repo, base)
}

func (f *Fake) FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error) {
	f.record("FindPRByBase")
	if f.FindPRByBaseFunc == nil {
		missing("FindPRByBase")
	}
	return f.FindPRByBaseFunc(ctx, owner, repo, base)
}

func (f *Fake) EnableIssues(ctx context.Context, owner, repo string) error {
	f.record("EnableIssues")
	if f.EnableIssuesFunc == nil {
		missing("EnableIssues")
	}
	return f.EnableIssuesFunc(ctx, owner, repo)
}

func (f *Fake) CreateIssue(ctx context.Context, owner, repo, title, body string) error {
	f.record("CreateIssue")
	if f.CreateIssueFunc == nil {
		missing("CreateIssue")
	}
	return f.CreateIssueFunc(ctx, owner, repo, title, body)
}

func (f *Fake) IssueExists(ctx context.Context, owner, repo, title string) (bool, error) {
	f.record("IssueExists")
	if f.IssueExistsFunc == nil {
		missing("IssueExists")
	}
	return f.IssueExistsFunc(ctx, owner, repo, title)
}

func (f *Fake) FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error) {
	f.record("FindIssueByTitle")
	if f.FindIssueByTitleFunc == nil {
		missing("FindIssueByTitle")
	}
	return f.FindIssueByTitleFunc(ctx, owner, repo, title)
}

func (f *Fake) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.Comment, error) {
	f.record("ListIssueComments")
	if f.ListIssueCommentsFunc == nil {
		missing("ListIssueComments")
	}
	return f.ListIssueCommentsFunc(ctx, owner, repo, number)
}

func (f *Fake) AddComment(ctx context.Context, owner, repo string, number int, body string) (string, error) {
	f.record("AddComment")
	if f.AddCommentFunc == nil {
		missing("AddComment")
	}
	return f.AddCommentFunc(ctx, owner, repo, number, body)
}

func (f *Fake) ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error) {
	f.record("ListOrgReposByPrefix")
	if f.ListOrgReposByPrefixFunc == nil {
		missing("ListOrgReposByPrefix")
	}
	return f.ListOrgReposByPrefixFunc(ctx, org, prefix)
}

func (f *Fake) ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error) {
	f.record("ListDirectCollaborators")
	if f.ListDirectCollaboratorsFunc == nil {
		missing("ListDirectCollaborators")
	}
	return f.ListDirectCollaboratorsFunc(ctx, owner, repo)
}

func (f *Fake) ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error) {
	f.record("ListRepoInvitations")
	if f.ListRepoInvitationsFunc == nil {
		missing("ListRepoInvitations")
	}
	return f.ListRepoInvitationsFunc(ctx, owner, repo)
}

func (f *Fake) DeleteRepoInvitation(ctx context.Context, owner, repo string, id int64) error {
	f.record("DeleteRepoInvitation")
	if f.DeleteRepoInvitationFunc == nil {
		missing("DeleteRepoInvitation")
	}
	return f.DeleteRepoInvitationFunc(ctx, owner, repo, id)
}

func (f *Fake) UpdateRepoInvitation(ctx context.Context, owner, repo string, id int64, permission string) error {
	f.record("UpdateRepoInvitation")
	if f.UpdateRepoInvitationFunc == nil {
		missing("UpdateRepoInvitation")
	}
	return f.UpdateRepoInvitationFunc(ctx, owner, repo, id, permission)
}

func (f *Fake) GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error) {
	f.record("GetPropertyDefinition")
	if f.GetPropertyDefinitionFunc == nil {
		missing("GetPropertyDefinition")
	}
	return f.GetPropertyDefinitionFunc(ctx, org, name)
}

func (f *Fake) SetPropertyDefinition(ctx context.Context, org string, def gh.PropertyDefinition) error {
	f.record("SetPropertyDefinition")
	if f.SetPropertyDefinitionFunc == nil {
		missing("SetPropertyDefinition")
	}
	return f.SetPropertyDefinitionFunc(ctx, org, def)
}

func (f *Fake) ListRepoPropertyValues(ctx context.Context, org string) (map[string]map[string]string, error) {
	f.record("ListRepoPropertyValues")
	if f.ListRepoPropertyValuesFunc == nil {
		missing("ListRepoPropertyValues")
	}
	return f.ListRepoPropertyValuesFunc(ctx, org)
}

func (f *Fake) GetRepoPropertyValues(ctx context.Context, org, repo string) (map[string]string, error) {
	f.record("GetRepoPropertyValues")
	if f.GetRepoPropertyValuesFunc == nil {
		missing("GetRepoPropertyValues")
	}
	return f.GetRepoPropertyValuesFunc(ctx, org, repo)
}

func (f *Fake) SetRepoPropertyValue(ctx context.Context, org, repo, name, value string) error {
	f.record("SetRepoPropertyValue")
	if f.SetRepoPropertyValueFunc == nil {
		missing("SetRepoPropertyValue")
	}
	return f.SetRepoPropertyValueFunc(ctx, org, repo, name, value)
}
