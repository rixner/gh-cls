// Package cmd wires the `gh cls` command tree: a root command carrying the
// flags shared by every subcommand, plus the setup, template, assign, and
// freeze subcommands that do the work.
package cmd

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/rixner/gh-cls/config"
	"github.com/spf13/cobra"
)

// defaultConcurrency bounds parallel GitHub operations unless -j overrides it.
const defaultConcurrency = 8

// version may be stamped at build time with
//
//	-ldflags "-X github.com/rixner/gh-cls/cmd.version=v1.2.3"
//
// but is normally left as "dev"; resolveVersion derives a meaningful value from
// the binary's embedded build information instead. The gh-extension-precompile
// action embeds no version, so released binaries report their commit revision;
// `gh extension list` is what shows users the release tag.
var version = "dev"

// resolveVersion reports the build version: an explicit ldflags stamp if set,
// else the module version (e.g. from `go install ...@v1.2.3`), else the VCS
// revision Go embeds at build time, else "dev".
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev (" + rev + ")"
}

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
		Version:      resolveVersion(),
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

// resolveOrg picks the org to operate on: the -o/--org flag wins, else the
// config's org (written by setup). template, assign, and freeze read it from
// config; setup is the only command that sets it.
func resolveOrg(g *globalOpts, cfg *config.Config) (string, error) {
	if g.org != "" {
		return g.org, nil
	}
	if cfg != nil && cfg.Org != "" {
		return cfg.Org, nil
	}
	return "", fmt.Errorf("no organization set; run `gh cls setup --org <org>` or pass -o/--org")
}

// ownerChecker is the slice of a client the owner guard needs.
type ownerChecker interface {
	OrgRole(ctx context.Context, org string) (string, error)
}

// requireOwner aborts unless the authenticated user is an organization owner
// (admin). This fails fast with a clear message instead of surfacing cryptic
// permission errors partway through a mutating command.
func requireOwner(ctx context.Context, c ownerChecker, org string) error {
	role, err := c.OrgRole(ctx, org)
	if err != nil {
		return fmt.Errorf("checking your role in %s: %w", org, err)
	}
	if role != "admin" {
		return fmt.Errorf("you must be an organization owner of %s to run this command (your role is %q)", org, role)
	}
	return nil
}
