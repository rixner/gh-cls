package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// freezeOpts carries the resolved flags for `gh cls freeze`.
type freezeOpts struct {
	g      *globalOpts
	undo   bool
	dryRun bool
}

func newFreezeCmd(g *globalOpts) *cobra.Command {
	o := &freezeOpts{g: g}
	cmd := &cobra.Command{
		Use:   "freeze <name>",
		Short: "Freeze (or unfreeze) an assignment's repositories",
		Long: `Downgrade every non-admin direct collaborator on the <name>-* repos from
write to read, a hard repo-wide deadline freeze. --undo restores push. The
operation reads each repo's current collaborators and never consults the
roster, so a drifted roster cannot let a student escape the freeze.`,
		Example: `  gh cls freeze hw1
  gh cls freeze hw1 --undo`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd, args[0])
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&o.undo, "undo", "u", false, "reverse a freeze: restore push to non-admin direct collaborators")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would change without doing it")
	return cmd
}

func (o *freezeOpts) run(cmd *cobra.Command, name string) error {
	fmt.Fprintf(cmd.OutOrStdout(), "freeze: name=%s org=%s undo=%t dry-run=%t (not yet implemented)\n",
		name, o.g.org, o.undo, o.dryRun)
	return nil
}
