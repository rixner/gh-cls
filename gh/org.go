package gh

import (
	"context"
	"fmt"
	"net/url"
)

// OrgSettings is the subset of organization settings setup inspects and changes.
// The bool toggles are pointers so an absent field (some tiers omit them) is
// distinguishable from an explicit false.
type OrgSettings struct {
	DefaultRepositoryPermission  string `json:"default_repository_permission"`
	MembersCanCreateRepositories *bool  `json:"members_can_create_repositories"`
	MembersCanCreatePages        *bool  `json:"members_can_create_pages"`
}

// ActionsPermissions is the org-wide GitHub Actions policy.
type ActionsPermissions struct {
	EnabledRepositories string `json:"enabled_repositories"`
}

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

// GetOrg reads the current organization settings.
func (c *restClient) GetOrg(ctx context.Context, org string) (*OrgSettings, error) {
	var s OrgSettings
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	if _, err := c.do(ctx, "GET", path, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// PatchOrg updates organization settings, sending only the given fields.
func (c *restClient) PatchOrg(ctx context.Context, org string, fields map[string]any) error {
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	_, err := c.do(ctx, "PATCH", path, fields, nil)
	return err
}

// GetActionsPermissions reads the org-wide Actions policy.
func (c *restClient) GetActionsPermissions(ctx context.Context, org string) (*ActionsPermissions, error) {
	var p ActionsPermissions
	path := fmt.Sprintf("orgs/%s/actions/permissions", url.PathEscape(org))
	if _, err := c.do(ctx, "GET", path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetActionsEnabledRepositories sets which repositories may run Actions
// org-wide (e.g. "none" to disable Actions entirely).
func (c *restClient) SetActionsEnabledRepositories(ctx context.Context, org, value string) error {
	path := fmt.Sprintf("orgs/%s/actions/permissions", url.PathEscape(org))
	_, err := c.do(ctx, "PUT", path, map[string]any{"enabled_repositories": value}, nil)
	return err
}

// CopilotSeatCount reports the org's purchased Copilot seats. A free org with no
// subscription returns present=false (the billing endpoint 404s).
func (c *restClient) CopilotSeatCount(ctx context.Context, org string) (count int, present bool, err error) {
	var out struct {
		SeatBreakdown struct {
			Total int `json:"total"`
		} `json:"seat_breakdown"`
	}
	path := fmt.Sprintf("orgs/%s/copilot/billing", url.PathEscape(org))
	if _, err := c.do(ctx, "GET", path, nil, &out); err != nil {
		if notFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return out.SeatBreakdown.Total, true, nil
}
