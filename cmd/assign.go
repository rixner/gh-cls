package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// feedback modes accepted by -f.
const (
	feedbackPR    = "pr"
	feedbackIssue = "issue"
)

// assignOpts carries the resolved flags for `gh cls assign`.
type assignOpts struct {
	g                *globalOpts
	roster           string
	teams            string
	public           bool
	branchProtection bool
	allBranches      bool
	feedback         string
	allowUnsquashed  bool
	dryRun           bool
}

func newAssignCmd(g *globalOpts) *cobra.Command {
	o := &assignOpts{g: g}
	cmd := &cobra.Command{
		Use:   "assign <name>",
		Short: "Bulk-create assignment repositories from the squashed template",
		Long: `Create one repository per unit (each student for an individual assignment,
each team for a group assignment) from the derived <name>-template, granting
push to the unit's members and to the staff team.`,
		Example: `  gh cls assign hw1 --roster roster.csv
  gh cls assign project --roster roster.csv --teams teams.yml --branch-protection`,
		Args:    cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error { return o.validate() },
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.teams, "teams", "T", "", "path to the teams file (required for group, rejected for individual)")
	f.BoolVarP(&o.public, "public", "p", false, "create public repos (default private)")
	f.BoolVarP(&o.branchProtection, "branch-protection", "b", false, "apply an all-branches protection ruleset")
	f.BoolVarP(&o.allBranches, "all-branches", "a", false, "include all template branches (default: default branch only)")
	f.StringVarP(&o.feedback, "feedback", "f", "", "create a feedback artifact: pr or issue")
	f.BoolVarP(&o.allowUnsquashed, "allow-unsquashed", "U", false, "proceed even if a template branch has more than one commit")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "list what would be created without doing it")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// validate checks flag values that don't depend on config or the filesystem.
func (o *assignOpts) validate() error {
	switch o.feedback {
	case "", feedbackPR, feedbackIssue:
	default:
		return fmt.Errorf("invalid --feedback %q: must be %q or %q", o.feedback, feedbackPR, feedbackIssue)
	}
	return nil
}

func (o *assignOpts) run(cmd *cobra.Command, name string) error {
	fmt.Fprintf(cmd.OutOrStdout(), "assign: name=%s org=%s roster=%s teams=%q feedback=%q dry-run=%t (not yet implemented)\n",
		name, o.g.org, o.roster, o.teams, o.feedback, o.dryRun)
	return nil
}
