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

// ListOrgReposByPrefix returns every repository in the org whose name starts
// with prefix, paging through all results.
func (c *restClient) ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]Repo, error) {
	var out []Repo
	for page := 1; ; page++ {
		var batch []Repo
		path := fmt.Sprintf("orgs/%s/repos?per_page=100&page=%d", url.PathEscape(org), page)
		if _, err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			if strings.HasPrefix(r.Name, prefix) {
				out = append(out, r)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// ListDirectCollaborators returns a repository's direct collaborators (not those
// with access only via a team or org membership).
func (c *restClient) ListDirectCollaborators(ctx context.Context, owner, repo string) ([]Collaborator, error) {
	var out []Collaborator
	for page := 1; ; page++ {
		var batch []Collaborator
		path := fmt.Sprintf("repos/%s/%s/collaborators?affiliation=direct&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		if _, err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}
