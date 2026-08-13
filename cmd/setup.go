package cmd

import (
	"context"
	"fmt"
	"io"

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
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	SetPropertyDefinition(ctx context.Context, org string, def gh.PropertyDefinition) error
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
		Short: "Harden the semester organization named in the config",
		Long: `Harden the semester organization named in the config: lock down base
permissions, member repository/Pages creation, private repository forking, and
Actions, and ensure the staff team exists.

The org and staff team are read from the config file (-c/--config or
$GH_CLS_CONFIG); setup never writes the config. All hardening actions are
idempotent, so setup is always safe to re-run.`,
		Example: "  gh cls setup -c gh-cls.yml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "print intended actions without performing them")
	return cmd
}

func (o *setupOpts) run(ctx context.Context, out io.Writer) error {
	// The org and staff team come from the config, already loaded and validated
	// (org is required) before this runs.
	org := o.g.org
	staffTeam := o.g.staffTeam

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN: no changes will be made\n\n")
		fmt.Fprintf(out, "Would harden %s:\n", org)
		fmt.Fprintln(out, "  - set base repository permission to none")
		fmt.Fprintln(out, "  - disable members creating repositories and Pages")
		fmt.Fprintln(out, "  - disable members forking private repositories")
		fmt.Fprintln(out, "  - disable GitHub Actions org-wide")
		fmt.Fprintln(out, "  - report Copilot seat status")
		fmt.Fprintf(out, "  - ensure staff team %q exists\n", staffTeam)
		fmt.Fprintf(out, "  - declare the %q organization property that freeze records deadline state in\n", frozenProperty)
		return nil
	}

	// Verify we own the org before mutating it, so a typo in the config or an org
	// we don't control fails fast rather than partway through hardening.
	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

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

	// Members forking private repositories. Student repos are private, so a fork
	// would put a copy outside the org where none of this hardening applies.
	r, err = toggleOff(ctx, client, org, "members_can_fork_private_repositories", "private repository forking", cur.MembersCanForkPrivateRepos)
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
		results = append(results, result{"Copilot", statusReported, "none present, nothing to disable"})
	} else {
		results = append(results, result{"Copilot", statusWarning, fmt.Sprintf("%d seat(s) present; cancel manually", count)})
	}

	// Staff team. Required by the config, so it is always ensured.
	if _, exists, err := client.GetTeam(ctx, org, staffTeam); err != nil {
		return nil, fmt.Errorf("checking staff team: %w", err)
	} else if exists {
		results = append(results, result{"staff team", statusAlready, staffTeam})
	} else if _, err := client.CreateTeam(ctx, org, staffTeam); err != nil {
		return nil, fmt.Errorf("creating staff team: %w", err)
	} else {
		results = append(results, result{"staff team", statusChanged, "created " + staffTeam})
	}

	// The freeze-state property. freeze records each repo's deadline state here and
	// audit --renew reads it to decide what access to restore, so it must exist
	// before the first freeze rather than being created on demand at a deadline.
	r, err = ensureFrozenProperty(ctx, client, org)
	if err != nil {
		return nil, err
	}
	results = append(results, r)

	// Post-condition: re-read the org and confirm the settings we changed actually
	// took. Some plan tiers silently accept a PATCH without applying it, so a 200
	// is not proof; any setting that did not stick becomes a loud warning so the
	// instructor knows to set it by hand rather than assuming the org is hardened.
	results = append(results, verifyHardening(ctx, client, org)...)

	return results, nil
}

// ensureFrozenProperty declares the organization custom property freeze records
// its state in. It re-asserts the declaration whenever the existing one differs,
// which covers the case that matters most: a property widened to let repository
// actors edit values, which would let a repo admin rewrite a deadline record.
func ensureFrozenProperty(ctx context.Context, client setupClient, org string) (result, error) {
	want := gh.PropertyDefinition{
		PropertyName:     frozenProperty,
		ValueType:        gh.PropertyTypeTrueFalse,
		Description:      frozenPropertyDescription,
		ValuesEditableBy: gh.PropertyEditableByOrg,
	}

	cur, exists, err := client.GetPropertyDefinition(ctx, org, frozenProperty)
	if err != nil {
		return result{}, fmt.Errorf("checking the %q organization property: %w", frozenProperty, err)
	}
	if exists && cur.ValueType == want.ValueType && cur.ValuesEditableBy == want.ValuesEditableBy {
		return result{"freeze-state property", statusAlready, frozenProperty}, nil
	}
	if err := client.SetPropertyDefinition(ctx, org, want); err != nil {
		return result{}, fmt.Errorf("declaring the %q organization property: %w", frozenProperty, err)
	}

	// Post-condition: a 200 is not proof. Confirm the property reads back with the
	// restricted edit scope, since that is what keeps a repository-level role from
	// rewriting a deadline record. A declaration that did not stick is a warning
	// rather than an abort, matching every other setting here: setup has already
	// changed the org by this point, so failing outright would leave it half
	// hardened, and freeze refuses to run without the property anyway.
	got, ok, err := client.GetPropertyDefinition(ctx, org, frozenProperty)
	if err != nil {
		return result{}, fmt.Errorf("verifying the %q organization property: %w", frozenProperty, err)
	}
	switch {
	case !ok:
		return result{"freeze-state property", statusWarning,
			fmt.Sprintf("%s is absent after being declared, so freeze will refuse to run; add it under Settings > Custom properties", frozenProperty)}, nil
	case got.ValuesEditableBy != gh.PropertyEditableByOrg:
		return result{"freeze-state property", statusWarning,
			fmt.Sprintf("%s is editable by %q, not %q, so a repository admin could rewrite a deadline record; fix it under Settings > Custom properties", frozenProperty, got.ValuesEditableBy, gh.PropertyEditableByOrg)}, nil
	}
	verb := "created"
	if exists {
		verb = "corrected"
	}
	return result{"freeze-state property", statusChanged, verb + " " + frozenProperty}, nil
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
			fmt.Sprintf("still %q after the change; your plan may not allow it, so set it manually", cur.DefaultRepositoryPermission)})
	}
	if cur.MembersCanCreateRepositories != nil && *cur.MembersCanCreateRepositories {
		warnings = append(warnings, result{"member repository creation", statusWarning, "still enabled after the change; set it manually"})
	}
	if cur.MembersCanCreatePages != nil && *cur.MembersCanCreatePages {
		warnings = append(warnings, result{"member Pages creation", statusWarning, "still enabled after the change; set it manually"})
	}
	if cur.MembersCanForkPrivateRepos != nil && *cur.MembersCanForkPrivateRepos {
		warnings = append(warnings, result{"private repository forking", statusWarning, "still enabled after the change; set it manually"})
	}

	ap, err := client.GetActionsPermissions(ctx, org)
	if err != nil {
		return append(warnings, result{"verification", statusWarning, "could not re-read Actions policy to confirm: " + err.Error()})
	}
	if ap.EnabledRepositories != "none" {
		warnings = append(warnings, result{"GitHub Actions", statusWarning,
			fmt.Sprintf("still %q after the change; set it manually", ap.EnabledRepositories)})
	}

	return warnings
}

// toggleOff sets a boolean org setting to false, reporting changed/already, or a
// warning when the org does not expose the field (current is nil).
func toggleOff(ctx context.Context, client setupClient, org, field, label string, current *bool) (result, error) {
	switch {
	case current == nil:
		return result{label, statusWarning, "not exposed by this org; set manually"}, nil
	case !*current:
		return result{label, statusAlready, "disabled"}, nil
	default:
		if err := client.PatchOrg(ctx, org, map[string]any{field: false}); err != nil {
			return result{}, fmt.Errorf("disabling %s: %w", label, err)
		}
		return result{label, statusChanged, "disabled"}, nil
	}
}
