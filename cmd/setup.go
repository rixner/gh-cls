package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// setupOpts carries the resolved flags for `gh cls setup`.
type setupOpts struct {
	g      *globalOpts
	dryRun bool
}

func newSetupCmd(g *globalOpts) *cobra.Command {
	o := &setupOpts{g: g}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Harden the semester organization and record it in config",
		Long: `Set up and harden the semester organization, and write the chosen org
into the config so later commands target it.

--org is required and is never read from config: stating it explicitly each
semester is the single deliberate act that establishes (or changes) the org.
All hardening actions are idempotent, so setup is always safe to re-run.`,
		Example: "  gh cls setup --org cs101-spring26 --staff-team staff",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd)
		},
	}
	cmd.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "print intended actions without performing them")
	return cmd
}

func (o *setupOpts) run(cmd *cobra.Command) error {
	// --org has no default and is not read from config; it must be stated each
	// time so the semester org can never be inherited stale from a prior term.
	if o.g.org == "" {
		return fmt.Errorf("setup requires --org (it is never read from config)")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "setup: org=%s staff-team=%q dry-run=%t (not yet implemented)\n",
		o.g.org, o.g.staffTeam, o.dryRun)
	return nil
}
