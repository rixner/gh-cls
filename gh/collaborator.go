package gh

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Collaborator is a repository collaborator and their effective permissions.
type Collaborator struct {
	Login       string `json:"login"`
	Permissions struct {
		Admin    bool `json:"admin"`
		Maintain bool `json:"maintain"`
		Push     bool `json:"push"`
		Triage   bool `json:"triage"`
		Pull     bool `json:"pull"`
	} `json:"permissions"`
}

// CanPush reports whether the collaborator holds effective write (push)
// access: push, maintain, or admin. Triage is not write — a triage-only
// collaborator cannot push.
func (c Collaborator) CanPush() bool {
	return c.Permissions.Admin || c.Permissions.Maintain || c.Permissions.Push
}

// AboveRead reports whether the collaborator holds any permission above
// plain read: push, maintain, or triage. This is freeze's downgrade set —
// triage cannot push but is still more than read, so a freeze reduces it to
// pull. Admin is deliberately excluded: staff keep access through a freeze.
func (c Collaborator) AboveRead() bool {
	return c.Permissions.Push || c.Permissions.Maintain || c.Permissions.Triage
}

// Invitation permission levels. GitHub's invitation API uses its own vocabulary
// for the access an invitation will confer, rather than the collaborator API's
// pull/push/... names.
const (
	InvitationRead     = "read"
	InvitationTriage   = "triage"
	InvitationWrite    = "write"
	InvitationMaintain = "maintain"
	InvitationAdmin    = "admin"
)

// Invitation is a pending repository collaborator invitation. A user added via
// AddCollaborator who is not an organization member receives an invitation
// rather than immediate access, and stays here until they accept it or it
// expires (GitHub expires unaccepted invitations after seven days).
type Invitation struct {
	ID      int64 `json:"id"`
	Invitee struct {
		Login string `json:"login"`
	} `json:"invitee"`
	// Expired is true once the seven-day acceptance window has lapsed; such an
	// invitation conveys no access and must be re-issued for the user to join.
	Expired bool `json:"expired"`
	// Permissions is the access the invitee will hold the moment they accept. An
	// unaccepted invitation keeps whatever it was issued with, independent of the
	// repository's current collaborators, so a freeze that changes only
	// collaborators leaves it as a way in.
	Permissions string `json:"permissions"`
}

// ConfersPush reports whether accepting the invitation would grant effective
// write (push) access. It mirrors Collaborator.CanPush over the invitation
// vocabulary.
func (i Invitation) ConfersPush() bool {
	switch i.Permissions {
	case InvitationWrite, InvitationMaintain, InvitationAdmin:
		return true
	}
	return false
}

// AboveRead reports whether accepting the invitation would grant more than plain
// read. It mirrors Collaborator.AboveRead over the invitation vocabulary,
// including the exclusion of admin: staff keep access through a freeze.
func (i Invitation) AboveRead() bool {
	switch i.Permissions {
	case InvitationWrite, InvitationMaintain, InvitationTriage:
		return true
	}
	return false
}

// ListOrgReposByPrefix returns every repository in the org whose name starts
// with prefix, paging through all results.
func (c *restClient) ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]Repo, error) {
	repos, err := getPaged[Repo](ctx, c, func(page int) string {
		return fmt.Sprintf("orgs/%s/repos?per_page=%d&page=%d", url.PathEscape(org), pageSize, page)
	})
	if err != nil {
		return nil, err
	}
	var out []Repo
	for _, r := range repos {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ListDirectCollaborators returns a repository's direct collaborators (not those
// with access only via a team or org membership).
func (c *restClient) ListDirectCollaborators(ctx context.Context, owner, repo string) ([]Collaborator, error) {
	return getPaged[Collaborator](ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/collaborators?affiliation=direct&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	})
}

// DeleteRepoInvitation cancels a repository invitation by its ID. Renewing an
// expired invitation is done by cancelling it and re-adding the collaborator,
// which issues a fresh one.
func (c *restClient) DeleteRepoInvitation(ctx context.Context, owner, repo string, id int64) error {
	path := fmt.Sprintf("repos/%s/%s/invitations/%d", url.PathEscape(owner), url.PathEscape(repo), id)
	_, err := c.do(ctx, "DELETE", path, nil, nil)
	return err
}

// UpdateRepoInvitation changes the access a pending invitation will confer once
// accepted. permission uses the invitation vocabulary (the Invitation* constants
// above), not the collaborator API's pull/push names.
//
// The bool reports whether the invitation was still pending. A 404 means it was
// accepted (or cancelled) between being listed and this call, which is a normal
// race rather than a failure: the invitee is a collaborator now, so their access
// is governed by the collaborator API instead.
func (c *restClient) UpdateRepoInvitation(ctx context.Context, owner, repo string, id int64, permission string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/invitations/%d", url.PathEscape(owner), url.PathEscape(repo), id)
	if _, err := c.do(ctx, "PATCH", path, map[string]any{"permissions": permission}, nil); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListRepoInvitations returns a repository's pending collaborator invitations:
// users granted access who are not organization members and have not yet
// accepted, so they hold no access despite a successful grant call.
func (c *restClient) ListRepoInvitations(ctx context.Context, owner, repo string) ([]Invitation, error) {
	return getPaged[Invitation](ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/invitations?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	})
}
