package gh

import (
	"context"
	"fmt"
	"net/url"
)

// UserExists reports whether a GitHub account with the given login exists. It is
// used to validate roster usernames before any repository is created: a typo'd or
// bogus handle otherwise surfaces only when the user is invited to an
// already-generated repo, leaving a stray repo behind.
func (c *restClient) UserExists(ctx context.Context, username string) (bool, error) {
	path := fmt.Sprintf("users/%s", url.PathEscape(username))
	if _, err := c.do(ctx, "GET", path, nil, nil); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
