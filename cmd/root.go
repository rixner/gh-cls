// Package cmd wires the `gh cls` command tree: a root command carrying the
// flags shared by every subcommand, plus the setup, template, assign, and
// freeze subcommands that do the work.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// defaultConcurrency bounds how many items a bulk command works on at once
// unless -j overrides it. It does not set how fast a run writes: the client
// paces mutating requests run-wide, so this bounds the reads and the waiting,
// which is most of what a worker spends its time on.
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
	// notices is the command's output, safe to write from any goroutine. The
	// client reports rate-limit pauses to it while workers are printing progress.
	notices io.Writer
	// logPath is --log-requests; logFile is the file it named, held open for the
	// life of the command.
	logPath string
	logFile *os.File
}

// client builds the GitHub client a subcommand runs on. Every command goes
// through here so that what the client is told about the run (where to report a
// pause, whether to log its requests) is decided in one place.
func (g *globalOpts) client() (gh.Client, error) {
	opts := []gh.Option{gh.WithNotices(g.notices)}
	if g.logFile != nil {
		opts = append(opts, gh.WithRequestLog(g.logFile))
	}
	return gh.New(opts...)
}

// openRequestLog opens the file --log-requests named, if any. It appends rather
// than truncating, so pointing two runs at one path accumulates both instead of
// destroying the first, and it opens before the command does any work, so a path
// that cannot be written fails the run before it has changed anything.
func (g *globalOpts) openRequestLog() error {
	if g.logPath == "" {
		return nil
	}
	f, err := os.OpenFile(g.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// The path is in the wrapped error already, spelled the way the OS
		// reports it, so it is not repeated here.
		return fmt.Errorf("opening the request log: %w; name a writable path with --log-requests, or drop the flag", err)
	}
	g.logFile = f
	return nil
}

// closeRequestLog closes the request log. Entries are written as they happen and
// are not buffered, so a run that fails before reaching this still leaves a
// complete log of everything it sent.
func (g *globalOpts) closeRequestLog() error {
	if g.logFile == nil {
		return nil
	}
	f := g.logFile
	g.logFile = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the request log: %w", err)
	}
	return nil
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

// textOnly reports whether a command only prints text and so needs no course
// config: cobra's help and completion commands (and the __complete the shell
// calls). The --help flag short-circuits before PersistentPreRunE runs, but the
// help subcommand does not, so `gh cls help assign` and shell completion would
// otherwise fail on any machine with no config set. The parent chain is walked so
// a child such as `completion bash` is covered too.
func textOnly(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
	}
	return false
}

// NewRootCmd builds the root `gh cls` command with all subcommands attached.
func NewRootCmd() *cobra.Command {
	g := &globalOpts{}

	root := &cobra.Command{
		Use:   "cls",
		Short: "Course tooling that replaces GitHub Classroom",
		Long: `gh cls manages a course's per-semester GitHub organization:
hardening the org, preparing squashed assignment templates, bulk-creating
student and group repositories, and freezing them at a deadline.

The org and staff team come from a user-authored config file, located with
-c/--config or $GH_CLS_CONFIG; the tool only reads it, never writes it.`,
		// Errors are returned to main for reporting; cobra should neither print
		// the error itself (main does, once) nor dump usage text on every
		// operational failure.
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       resolveVersion(),
		// Load the config once, up front, so every subcommand shares it. Runs for
		// every subcommand that does work; the --version and --help flags
		// short-circuit before this, and textOnly covers the commands that produce
		// text without doing any.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if textOnly(cmd) {
				return nil
			}
			// Everything a command prints goes through one lock from here on, so
			// progress lines and the client's rate-limit notices cannot collide.
			out := &syncWriter{w: cmd.OutOrStdout()}
			cmd.SetOut(out)
			g.notices = out
			if err := g.load(); err != nil {
				return err
			}
			return g.openRequestLog()
		},
		PersistentPostRunE: func(*cobra.Command, []string) error { return g.closeRequestLog() },
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&g.configPath, "config", "c", "", "path to the course config file (or set $GH_CLS_CONFIG)")
	pf.IntVarP(&g.concurrency, "concurrency", "j", defaultConcurrency, "max items worked on at once (writes are paced run-wide regardless)")
	pf.StringVar(&g.logPath, "log-requests", "", "append a JSON line per GitHub API request to this file, for diagnosing rate limits")

	root.AddCommand(
		newSetupCmd(g),
		newStaffCmd(g),
		newTemplateCmd(g),
		newAssignCmd(g),
		newFreezeCmd(g),
		newAuditCmd(g),
		newFeedbackCmd(g),
		newStatusCmd(g),
		newCollectCmd(g),
		newActivityCmd(g),
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
