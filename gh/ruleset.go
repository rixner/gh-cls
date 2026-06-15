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

	active, found, err := c.findManagedRuleset(ctx, path)
	if err != nil {
		return err
	}
	if found {
		// Already present: confirm it is actually enforcing. A ruleset that exists
		// but is disabled would leave student work unprotected while looking applied,
		// so this is a loud failure rather than a silent idempotent skip.
		if !active {
			return fmt.Errorf("branch-protection ruleset %q already exists on %s/%s but is not active; inspect it and set its enforcement to active manually", protectRulesetName, org, repo)
		}
		return nil
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
	if _, err := c.do(ctx, "POST", path, body, nil); err != nil {
		return err
	}

	// Post-condition: re-read and confirm the ruleset now exists and is enforcing.
	// A 200 on the create is not proof the protection is live, and an unprotected
	// student repo that looks protected is exactly the failure this guards against.
	active, found, err = c.findManagedRuleset(ctx, path)
	if err != nil {
		return fmt.Errorf("verifying branch protection on %s/%s: %w", org, repo, err)
	}
	if !found || !active {
		return fmt.Errorf("branch protection on %s/%s did not take effect: ruleset %q is missing or not active after creation; apply it manually", org, repo, protectRulesetName)
	}
	return nil
}

// findManagedRuleset reports whether this tool's ruleset exists on the repo and,
// if so, whether it is actively enforcing.
func (c *restClient) findManagedRuleset(ctx context.Context, rulesetsPath string) (active, found bool, err error) {
	var existing []struct {
		Name        string `json:"name"`
		Enforcement string `json:"enforcement"`
	}
	if _, err := c.do(ctx, "GET", rulesetsPath, nil, &existing); err != nil {
		return false, false, err
	}
	for _, r := range existing {
		if r.Name == protectRulesetName {
			return r.Enforcement == "active", true, nil
		}
	}
	return false, false, nil
}
