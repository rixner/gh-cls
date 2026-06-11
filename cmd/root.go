// Package cmd wires the `gh cls` command tree: a root command carrying the
// flags shared by every subcommand, plus the setup, template, assign, and
// freeze subcommands that do the work.
package cmd

import "github.com/spf13/cobra"

// defaultConcurrency bounds parallel GitHub operations unless -j overrides it.
const defaultConcurrency = 8

// globalOpts holds the flags shared by every subcommand. A single instance is
// bound to the root's persistent flags and handed to each subcommand, so a
// subcommand reads the same values the user set anywhere on the line.
type globalOpts struct {
	org         string
	staffTeam   string
	concurrency int
}

// NewRootCmd builds the root `gh cls` command with all subcommands attached.
func NewRootCmd() *cobra.Command {
	g := &globalOpts{}

	root := &cobra.Command{
		Use:   "cls",
		Short: "Course tooling that replaces GitHub Classroom",
		Long: `gh cls manages a course's per-semester GitHub organization:
hardening the org, preparing squashed assignment templates, bulk-creating
student and team repositories, and freezing them at a deadline.`,
		// Errors are returned to main for reporting; cobra should not also dump
		// usage text on every operational failure.
		SilenceUsage: true,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&g.org, "org", "o", "", "semester GitHub organization")
	pf.StringVarP(&g.staffTeam, "staff-team", "s", "", "staff/TA team slug")
	pf.IntVarP(&g.concurrency, "concurrency", "j", defaultConcurrency, "max concurrent GitHub operations")

	root.AddCommand(
		newSetupCmd(g),
		newTemplateCmd(g),
		newAssignCmd(g),
		newFreezeCmd(g),
	)
	return root
}
