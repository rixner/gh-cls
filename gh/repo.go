package gh

import (
	"context"
	"fmt"
	"net/url"
)

// GetRepo fetches a repository, reporting existence via the bool so callers can
// branch on it without inspecting error strings.
func (c *restClient) GetRepo(ctx context.Context, owner, name string) (*Repo, bool, error) {
	var repo Repo
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	if _, err := c.do(ctx, "GET", path, nil, &repo); err != nil {
		if notFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &repo, true, nil
}

// SetRepoTemplate marks a repository as a template repository.
func (c *restClient) SetRepoTemplate(ctx context.Context, owner, name string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	_, err := c.do(ctx, "PATCH", path, map[string]any{"is_template": true}, nil)
	return err
}

// DeleteRepo deletes a repository.
func (c *restClient) DeleteRepo(ctx context.Context, org, name string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(name))
	_, err := c.do(ctx, "DELETE", path, nil, nil)
	return err
}

// GenerateFromTemplate creates a new repository (owner/name) from a template
// repository. Generation copies the template's tree; with includeAllBranches it
// copies every branch, otherwise only the default branch.
func (c *restClient) GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error {
	path := fmt.Sprintf("repos/%s/%s/generate", url.PathEscape(tmplOwner), url.PathEscape(tmplRepo))
	body := map[string]any{
		"owner":                owner,
		"name":                 name,
		"private":              private,
		"include_all_branches": includeAllBranches,
	}
	_, err := c.do(ctx, "POST", path, body, nil)
	return err
}

// AddCollaborator grants a user the given permission on a repository.
func (c *restClient) AddCollaborator(ctx context.Context, owner, repo, username, permission string) error {
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(username))
	_, err := c.do(ctx, "PUT", path, map[string]any{"permission": permission}, nil)
	return err
}
