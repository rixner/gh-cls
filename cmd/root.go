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

// globalOpts holds the course config, loaded once by the root before any
// subcommand runs, plus the shared --concurrency flag. The org and staff team
// are read from the config (never overridden on the command line), so every
// subcommand sees the same configured semester. configPath is the -c flag.
type globalOpts struct {
	configPath  string
	cfg         *config.Config
	org         string
	staffTeam   string
	concurrency int
}

// load resolves the config path (-c flag or $GH_CLS_CONFIG), reads the config
// once, and exposes the org and staff team it records to every subcommand.
func (g *globalOpts) load() error {
	path, err := config.ResolvePath(g.configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	g.cfg = cfg
	g.org = cfg.Org
	g.staffTeam = cfg.StaffTeam
	return nil
}

// NewRootCmd builds the root `gh cls` command with all subcommands attached.
func NewRootCmd() *cobra.Command {
	g := &globalOpts{}

	root := &cobra.Command{
		Use:   "cls",
		Short: "Course tooling that replaces GitHub Classroom",
		Long: `gh cls manages a course's per-semester GitHub organization:
hardening the org, preparing squashed assignment templates, bulk-creating
student and team repositories, and freezing them at a deadline.

The org and staff team come from a user-authored config file, located with
-c/--config or $GH_CLS_CONFIG; the tool only reads it, never writes it.`,
		// Errors are returned to main for reporting; cobra should neither print
		// the error itself (main does, once) nor dump usage text on every
		// operational failure.
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       resolveVersion(),
		// Load the config once, up front, so every subcommand shares it. Runs for
		// all subcommands; --version/--help short-circuit before this.
		PersistentPreRunE: func(*cobra.Command, []string) error { return g.load() },
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&g.configPath, "config", "c", "", "path to the course config file (or set $GH_CLS_CONFIG)")
	pf.IntVarP(&g.concurrency, "concurrency", "j", defaultConcurrency, "max concurrent GitHub operations")

	root.AddCommand(
		newSetupCmd(g),
		newTemplateCmd(g),
		newAssignCmd(g),
		newFreezeCmd(g),
		newAuditCmd(g),
	)
	return root
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
