package gh

import (
	"context"
	"fmt"
	"net/url"
)

// protectRulesetName identifies the ruleset this tool manages, so re-applying it
// is idempotent.
const protectRulesetName = "gh-cls-protect"

// ApplyRuleset applies an all-branches ruleset that blocks force-pushes and
// branch deletion while allowing ordinary pushes, bypassed by org admins and
// (if non-zero) the staff team. It is idempotent: if the ruleset already exists
// it does nothing.
func (c *restClient) ApplyRuleset(ctx context.Context, org, repo string, staffTeamID int64) error {
	path := fmt.Sprintf("repos/%s/%s/rulesets", url.PathEscape(org), url.PathEscape(repo))

	var existing []struct {
		Name string `json:"name"`
	}
	if _, err := c.do(ctx, "GET", path, nil, &existing); err != nil {
		return err
	}
	for _, r := range existing {
		if r.Name == protectRulesetName {
			return nil
		}
	}

	bypass := []map[string]any{
		{"actor_id": 1, "actor_type": "OrganizationAdmin", "bypass_mode": "always"},
	}
	if staffTeamID != 0 {
		bypass = append(bypass, map[string]any{"actor_id": staffTeamID, "actor_type": "Team", "bypass_mode": "always"})
	}

	body := map[string]any{
		"name":        protectRulesetName,
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{"include": []string{"~ALL"}, "exclude": []string{}},
		},
		"rules": []map[string]any{
			{"type": "non_fast_forward"},
			{"type": "deletion"},
		},
		"bypass_actors": bypass,
	}
	_, err := c.do(ctx, "POST", path, body, nil)
	return err
}
