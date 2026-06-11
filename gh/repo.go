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
