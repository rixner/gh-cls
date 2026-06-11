package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// defaultSquashMessage is the commit message of the single flattened commit in
// a derived template, used unless -m overrides it.
const defaultSquashMessage = "Provided starter code"

// templateOpts carries the resolved flags for `gh cls template`.
type templateOpts struct {
	g       *globalOpts
	source  string
	message string
	force   bool
	dryRun  bool
}

func newTemplateCmd(g *globalOpts) *cobra.Command {
	o := &templateOpts{g: g}
	cmd := &cobra.Command{
		Use:   "template <name>",
		Short: "Prepare a squashed template for an assignment",
		Long: `Derive a single-commit template repo (<name>-template) in the semester org
from the maintained source template, so the source template's development
history is never exposed to students. Run once per assignment, before assign.`,
		Example: "  gh cls template hw1",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.source, "template", "t", "", "source template repo (owner/name), overriding config")
	f.StringVarP(&o.message, "message", "m", defaultSquashMessage, "commit message for the flattened commit")
	f.BoolVarP(&o.force, "force", "F", false, "overwrite an existing <name>-template")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "describe what would be created without doing it")
	return cmd
}

func (o *templateOpts) run(cmd *cobra.Command, name string) error {
	fmt.Fprintf(cmd.OutOrStdout(), "template: name=%s org=%s source=%q dry-run=%t (not yet implemented)\n",
		name, o.g.org, o.source, o.dryRun)
	return nil
}
