package gh

import (
	"context"
	"fmt"
	"net/url"
)

// Team is the subset of a team's fields the tool needs: its ID, required to name
// the team as a ruleset bypass actor.
type Team struct {
	ID int64 `json:"id"`
}

// GetTeam fetches a team by slug, reporting existence via the bool.
func (c *restClient) GetTeam(ctx context.Context, org, slug string) (*Team, bool, error) {
	var t Team
	path := fmt.Sprintf("orgs/%s/teams/%s", url.PathEscape(org), url.PathEscape(slug))
	if _, err := c.do(ctx, "GET", path, nil, &t); err != nil {
		if notFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &t, true, nil
}

// CreateTeam creates a closed (org-visible) team and returns it.
func (c *restClient) CreateTeam(ctx context.Context, org, name string) (*Team, error) {
	var t Team
	path := fmt.Sprintf("orgs/%s/teams", url.PathEscape(org))
	body := map[string]any{"name": name, "privacy": "closed"}
	if _, err := c.do(ctx, "POST", path, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// AddTeamRepo grants a team the given permission on a repository.
func (c *restClient) AddTeamRepo(ctx context.Context, org, teamSlug, owner, repo, permission string) error {
	path := fmt.Sprintf("orgs/%s/teams/%s/repos/%s/%s",
		url.PathEscape(org), url.PathEscape(teamSlug), url.PathEscape(owner), url.PathEscape(repo))
	_, err := c.do(ctx, "PUT", path, map[string]any{"permission": permission}, nil)
	return err
}

// teamMember is the subset of a team member the tool reads: the login.
type teamMember struct {
	Login string `json:"login"`
}

// ListTeamMembers returns the logins of a team's current members, paging through
// all results.
func (c *restClient) ListTeamMembers(ctx context.Context, org, slug string) ([]string, error) {
	var out []string
	for page := 1; ; page++ {
		var batch []teamMember
		path := fmt.Sprintf("orgs/%s/teams/%s/members?per_page=100&page=%d",
			url.PathEscape(org), url.PathEscape(slug), page)
		if _, err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return nil, err
		}
		for _, m := range batch {
			out = append(out, m.Login)
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// AddTeamMembership adds (or invites) a user to a team as a plain member,
// returning the resulting state: "active" when the user is already an org member
// and joins immediately, or "pending" when they must first accept an invitation
// to the organization. It is idempotent.
func (c *restClient) AddTeamMembership(ctx context.Context, org, slug, username string) (string, error) {
	var out struct {
		State string `json:"state"`
	}
	path := fmt.Sprintf("orgs/%s/teams/%s/memberships/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(username))
	if _, err := c.do(ctx, "PUT", path, map[string]any{"role": "member"}, &out); err != nil {
		return "", err
	}
	return out.State, nil
}

// RemoveTeamMembership removes a user from a team. It is idempotent: removing a
// user who is not a member is not an error.
func (c *restClient) RemoveTeamMembership(ctx context.Context, org, slug, username string) error {
	path := fmt.Sprintf("orgs/%s/teams/%s/memberships/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(username))
	_, err := c.do(ctx, "DELETE", path, nil, nil)
	return err
}
