package gh

import (
	"context"
	"fmt"
	"net/url"
)

// OrgRole returns the authenticated user's membership role in org.
func (c *restClient) OrgRole(ctx context.Context, org string) (string, error) {
	var out struct {
		Role string `json:"role"`
	}
	path := fmt.Sprintf("user/memberships/orgs/%s", url.PathEscape(org))
	if _, err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return "", err
	}
	return out.Role, nil
}
