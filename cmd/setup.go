package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// setupClient is the narrow set of GitHub operations setup needs.
type setupClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetOrg(ctx context.Context, org string) (*gh.OrgSettings, error)
	PatchOrg(ctx context.Context, org string, fields map[string]any) error
	GetActionsPermissions(ctx context.Context, org string) (*gh.ActionsPermissions, error)
	SetActionsEnabledRepositories(ctx context.Context, org, value string) error
	CopilotSeatCount(ctx context.Context, org string) (int, bool, error)
	GetTeam(ctx context.Context, org, slug string) (*gh.Team, bool, error)
	CreateTeam(ctx context.Context, org, name string) (*gh.Team, error)
}

// setupOpts carries the resolved flags and dependencies for `gh cls setup`.
type setupOpts struct {
	g         *globalOpts
	dryRun    bool
	newClient func(context.Context) (setupClient, error)
}

func newSetupCmd(g *globalOpts) *cobra.Command {
	o := &setupOpts{
		g:         g,
		newClient: func(context.Context) (setupClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Harden the semester organization and record it in config",
		Long: `Set up and harden the semester organization, and write the chosen org
into the config so later commands target it.

--org is required and is never read from config: stating it explicitly each
semester is the single deliberate act that establishes (or changes) the org.
All hardening actions are idempotent, so setup is always safe to re-run.`,
		Example: "  gh cls setup --org cs101-spring26 --staff staff",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "print intended actions without performing them")
	return cmd
}

func (o *setupOpts) run(ctx context.Context, out io.Writer) error {
	org := o.g.org
	if org == "" {
		// --org has no default and is not read from config; it must be stated
		// each time so the semester org can never be inherited stale.
		return fmt.Errorf("setup requires --org (it is never read from config)")
	}

	// Read any existing config for the staff team and the previous org. A
	// missing or unreadable file is fine here: setup is about to (re)write it,
	// and WriteOrg surfaces a genuinely malformed file.
	cfg, _, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	staffTeam := o.g.staffTeam
	if staffTeam == "" {
		staffTeam = cfg.StaffTeam
	}
	writePath := config.DefaultPath()

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN — no changes will be made\n\n")
		printOrgWarning(out, org, cfg.Org)
		fmt.Fprintf(out, "\nWould harden %s:\n", org)
		fmt.Fprintln(out, "  - set base repository permission to none")
		fmt.Fprintln(out, "  - disable members creating repositories and Pages")
		fmt.Fprintln(out, "  - disable GitHub Actions org-wide")
		fmt.Fprintln(out, "  - report Copilot seat status")
		if staffTeam != "" {
			fmt.Fprintf(out, "  - ensure staff team %q exists\n", staffTeam)
		}
		return nil
	}

	// Verify the org exists and we own it before persisting it, so a typo or an
	// org we don't control can never poison the config that later commands read.
	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// The org write is the deliberate act that establishes the semester; it is
	// announced loudly, and happens only once the ownership check has passed.
	prev, err := config.WriteOrg(writePath, org)
	if err != nil {
		return err
	}
	printOrgWarning(out, org, prev)

	results, err := hardenOrg(ctx, client, org, staffTeam)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nHardening %s:\n", org)
	printResults(out, results)
	printManualSteps(out, []string{
		"Confirm the Copilot policy toggle is off (no public API on a free/Education org).",
		"Review any school-specific member-privilege settings.",
		"Add TAs to the staff team; you remain an Owner.",
		"Verify in Billing & plans that the org shows \"Team\" (required for --branch-protection).",
	})
	printOptionalHardening(out, []string{
		"Restrict members from installing apps / granting third-party integration access, if you want owners-only.",
		"Restrict members from changing repository visibility.",
		"Restrict members from deleting or transferring repositories.",
		"Restrict members from creating teams.",
	})
	return nil
}

// hardenOrg applies the idempotent hardening actions, returning a per-action
// report. It aborts on the first API error.
func hardenOrg(ctx context.Context, client setupClient, org, staffTeam string) ([]result, error) {
	var results []result

	cur, err := client.GetOrg(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reading %s settings: %w", org, err)
	}

	// Base repository permission -> none.
	if cur.DefaultRepositoryPermission == "none" {
		results = append(results, result{"base repository permission", statusAlready, "none"})
	} else {
		if err := client.PatchOrg(ctx, org, map[string]any{"default_repository_permission": "none"}); err != nil {
			return nil, fmt.Errorf("setting base permission: %w", err)
		}
		results = append(results, result{"base repository permission", statusChanged, "was " + cur.DefaultRepositoryPermission + ", now none"})
	}

	// Members creating repositories.
	r, err := toggleOff(ctx, client, org, "members_can_create_repositories", "member repository creation", cur.MembersCanCreateRepositories)
	if err != nil {
		return nil, err
	}
	results = append(results, r)

	// Members creating Pages.
	r, err = toggleOff(ctx, client, org, "members_can_create_pages", "member Pages creation", cur.MembersCanCreatePages)
	if err != nil {
		return nil, err
	}
	results = append(results, r)

	// GitHub Actions org-wide.
	ap, err := client.GetActionsPermissions(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reading Actions policy: %w", err)
	}
	if ap.EnabledRepositories == "none" {
		results = append(results, result{"GitHub Actions", statusAlready, "disabled org-wide"})
	} else {
		if err := client.SetActionsEnabledRepositories(ctx, org, "none"); err != nil {
			return nil, fmt.Errorf("disabling Actions: %w", err)
		}
		results = append(results, result{"GitHub Actions", statusChanged, "disabled org-wide"})
	}

	// Copilot is reported, never changed (no master toggle via the API).
	count, present, err := client.CopilotSeatCount(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reading Copilot status: %w", err)
	}
	if !present {
		results = append(results, result{"Copilot", statusReported, "none present — nothing to disable"})
	} else {
		results = append(results, result{"Copilot", statusWarning, fmt.Sprintf("%d seat(s) present — cancel manually", count)})
	}

	// Staff team.
	if staffTeam == "" {
		results = append(results, result{"staff team", statusReported, "none configured — skipped"})
	} else if _, exists, err := client.GetTeam(ctx, org, staffTeam); err != nil {
		return nil, fmt.Errorf("checking staff team: %w", err)
	} else if exists {
		results = append(results, result{"staff team", statusAlready, staffTeam})
	} else if _, err := client.CreateTeam(ctx, org, staffTeam); err != nil {
		return nil, fmt.Errorf("creating staff team: %w", err)
	} else {
		results = append(results, result{"staff team", statusChanged, "created " + staffTeam})
	}

	// Post-condition: re-read the org and confirm the settings we changed actually
	// took. Some plan tiers silently accept a PATCH without applying it, so a 200
	// is not proof; any setting that did not stick becomes a loud warning so the
	// instructor knows to set it by hand rather than assuming the org is hardened.
	results = append(results, verifyHardening(ctx, client, org)...)

	return results, nil
}

// verifyHardening re-reads the org and returns a warning for each setting that
// did not reach its hardened value. It returns nothing when everything stuck,
// keeping a clean run quiet.
func verifyHardening(ctx context.Context, client setupClient, org string) []result {
	var warnings []result

	cur, err := client.GetOrg(ctx, org)
	if err != nil {
		return append(warnings, result{"verification", statusWarning, "could not re-read org settings to confirm: " + err.Error()})
	}
	if cur.DefaultRepositoryPermission != "none" {
		warnings = append(warnings, result{"base repository permission", statusWarning,
			fmt.Sprintf("still %q after the change — your plan may not allow it; set it manually", cur.DefaultRepositoryPermission)})
	}
	if cur.MembersCanCreateRepositories != nil && *cur.MembersCanCreateRepositories {
		warnings = append(warnings, result{"member repository creation", statusWarning, "still enabled after the change — set it manually"})
	}
	if cur.MembersCanCreatePages != nil && *cur.MembersCanCreatePages {
		warnings = append(warnings, result{"member Pages creation", statusWarning, "still enabled after the change — set it manually"})
	}

	ap, err := client.GetActionsPermissions(ctx, org)
	if err != nil {
		return append(warnings, result{"verification", statusWarning, "could not re-read Actions policy to confirm: " + err.Error()})
	}
	if ap.EnabledRepositories != "none" {
		warnings = append(warnings, result{"GitHub Actions", statusWarning,
			fmt.Sprintf("still %q after the change — set it manually", ap.EnabledRepositories)})
	}

	return warnings
}

// toggleOff sets a boolean org setting to false, reporting changed/already, or a
// warning when the org does not expose the field (current is nil).
func toggleOff(ctx context.Context, client setupClient, org, field, label string, current *bool) (result, error) {
	switch {
	case current == nil:
		return result{label, statusWarning, "not exposed by this org — set manually"}, nil
	case !*current:
		return result{label, statusAlready, "disabled"}, nil
	default:
		if err := client.PatchOrg(ctx, org, map[string]any{field: false}); err != nil {
			return result{}, fmt.Errorf("disabling %s: %w", label, err)
		}
		return result{label, statusChanged, "disabled"}, nil
	}
}

// printOrgWarning loudly announces the org value written to config.
func printOrgWarning(w io.Writer, org, previous string) {
	if previous == "" {
		previous = "none"
	}
	fmt.Fprintf(w, "⚠️  CONFIG ORG SET → %s\n", org)
	fmt.Fprintf(w, "    (previous: %s)\n", previous)
	fmt.Fprintf(w, "    All subsequent template/assign/freeze commands will target %s.\n", org)
}
