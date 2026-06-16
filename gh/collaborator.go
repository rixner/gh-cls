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

// ListRepoInvitations returns a repository's pending collaborator invitations:
// users granted access who are not organization members and have not yet
// accepted, so they hold no access despite a successful grant call.
func (c *restClient) ListRepoInvitations(ctx context.Context, owner, repo string) ([]Invitation, error) {
	return getPaged[Invitation](ctx, c, func(page int) string {
		return fmt.Sprintf("repos/%s/%s/invitations?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), pageSize, page)
	})
}
