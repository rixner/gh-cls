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
