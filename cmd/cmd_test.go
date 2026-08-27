package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// execute runs the root command with the given args, capturing output, and
// returns the error from Execute.
func execute(args ...string) (string, error) {
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// withConfig writes a config file and points $GH_CLS_CONFIG at it, so an
// execute()-based test exercises the real root config load. It returns the path.
func withConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh-cls.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CLS_CONFIG", p)
	return p
}

// subcommand returns the named child of a fresh root command.
func subcommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("subcommand %q not found", name)
	return nil
}

// TestPersistentFlagMatrix checks the shared flags exist with the expected
// shorthands on the root command. The org and staff team are not flags anywhere:
// they come from the config file (see TestOrgIsConfigOnly).
func TestPersistentFlagMatrix(t *testing.T) {
	pf := NewRootCmd().PersistentFlags()
	for short, long := range map[string]string{"c": "config", "j": "concurrency"} {
		f := pf.ShorthandLookup(short)
		if f == nil {
			t.Fatalf("persistent shorthand -%s not defined", short)
		}
		if f.Name != long {
			t.Errorf("persistent -%s maps to %q, want %q", short, f.Name, long)
		}
	}
}

// TestOrgIsConfigOnly guards the design that the org and staff team are read
// only from the config file: no command, not even setup, accepts them as
// flags, so a stray -o/--org can never target an unconfigured org.
func TestOrgIsConfigOnly(t *testing.T) {
	if NewRootCmd().PersistentFlags().Lookup("org") != nil {
		t.Error("--org must not be a flag")
	}
	for _, name := range []string{"setup", "staff", "template", "assign", "freeze", "audit", "feedback", "status", "collect"} {
		withConfig(t, "org: cs101-spring26\nstaff_team: staff\n")
		if _, err := execute(name, "x", "--org", "foo"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s should reject --org as an unknown flag, got %v", name, err)
		}
	}
}

// TestLocalFlagMatrix checks each subcommand's local flags and shorthands. This
// guards the deliberately collision-avoiding letters (-t/-T, -u/-U, -F).
func TestLocalFlagMatrix(t *testing.T) {
	cases := map[string]map[string]string{
		"setup":    {"n": "dry-run"},
		"staff":    {"n": "dry-run"},
		"template": {"S": "source", "F": "force", "n": "dry-run"},
		"assign":   {"r": "roster", "g": "groups", "p": "public", "b": "branch-protection", "a": "all-branches", "U": "allow-unsquashed", "n": "dry-run"},
		"freeze":   {"u": "undo", "n": "dry-run"},
		"audit":    {"r": "roster", "g": "groups", "n": "dry-run"},
		"feedback": {"d": "dir", "r": "roster", "g": "groups", "F": "force", "n": "dry-run"},
		"status":   {"o": "out"},
		"collect":  {"o": "out", "r": "roster", "g": "groups", "s": "snapshot", "n": "dry-run"},
		"activity": {"s": "snapshot", "f": "from", "t": "to", "w": "rewrites", "o": "out"},
	}
	for name, want := range cases {
		cmd := subcommand(t, name)
		for short, long := range want {
			f := cmd.Flags().ShorthandLookup(short)
			if f == nil {
				t.Errorf("%s: shorthand -%s not defined", name, short)
				continue
			}
			if f.Name != long {
				t.Errorf("%s: -%s maps to %q, want %q", name, short, f.Name, long)
			}
		}
	}
}

// TestShorthandsMeanOneThing enforces the rule the flag set is built on: a
// shorthand letter names the same long flag everywhere it appears. Letters that
// flip meaning between commands are how a hand reaches for one command's -u and
// lands on another's, so the invariant is checked rather than trusted.
func TestShorthandsMeanOneThing(t *testing.T) {
	meaning := map[string]string{} // shorthand -> long name
	where := map[string]string{}   // shorthand -> the command that claimed it
	root := NewRootCmd()

	claim := func(cmdName string, f *pflag.Flag) {
		if f.Shorthand == "" {
			return
		}
		if prev, seen := meaning[f.Shorthand]; seen && prev != f.Name {
			t.Errorf("-%s is --%s in %s but --%s in %s; one letter must mean one thing",
				f.Shorthand, f.Name, cmdName, prev, where[f.Shorthand])
			return
		}
		meaning[f.Shorthand] = f.Name
		where[f.Shorthand] = cmdName
	}

	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { claim("cls", f) })
	for _, c := range root.Commands() {
		c.Flags().VisitAll(func(f *pflag.Flag) { claim(c.Name(), f) })
	}
}

func TestSetupRequiresConfig(t *testing.T) {
	// No config at all: setup fails fast asking for one.
	t.Setenv("GH_CLS_CONFIG", "")
	if _, err := execute("setup"); err == nil {
		t.Fatal("setup without a config should error")
	}
	// A config with an org hardens in dry-run without touching GitHub.
	withConfig(t, "org: cs101-spring26\nstaff_team: staff\n")
	if _, err := execute("setup", "--dry-run"); err != nil {
		t.Fatalf("setup --dry-run with a valid config should succeed, got %v", err)
	}
	// A config that omits org is rejected with guidance to add it.
	withConfig(t, "staff_team: staff\n")
	if _, err := execute("setup", "--dry-run"); err == nil || !strings.Contains(err.Error(), "org") {
		t.Fatalf("a config without org should error mentioning org, got %v", err)
	}
}

func TestAssignRequiresRoster(t *testing.T) {
	// The full run is covered in assign_test.go with config and a fake client;
	// here we only assert the required-flag enforcement.
	withConfig(t, "org: cs101-spring26\nstaff_team: staff\n")
	if _, err := execute("assign", "hw1"); err == nil {
		t.Fatal("assign without --roster should error")
	}
}

func TestAssignFeedbackEnum(t *testing.T) {
	// Invalid value is rejected in PreRunE, before any work.
	withConfig(t, "org: cs101-spring26\nstaff_team: staff\n")
	_, err := execute("assign", "hw1", "-r", "roster.csv", "--feedback", "bogus")
	if err == nil || !strings.Contains(err.Error(), "invalid --feedback") {
		t.Fatalf("invalid feedback mode should be rejected, got %v", err)
	}
	// Valid values pass validation.
	for _, mode := range []string{"", "pr", "issue"} {
		if err := (&assignOpts{feedback: mode}).validate(); err != nil {
			t.Errorf("feedback %q should validate, got %v", mode, err)
		}
	}
}

func TestTextOnlyCommandsNeedNoConfig(t *testing.T) {
	// Help and completion print text and touch nothing, so they must work on a
	// machine that has no course config: a shell evaluates the completion script
	// on every new session, and reading the help is how someone finds out a config
	// is needed at all.
	t.Setenv("GH_CLS_CONFIG", "")
	cases := map[string][]string{
		"help subcommand":   {"help", "assign"},
		"completion script": {"completion", "bash"},
		"shell completion":  {cobra.ShellCompRequestCmd, "assign", ""},
	}
	for name, args := range cases {
		out, err := execute(args...)
		if err != nil {
			t.Errorf("%s should not need a config, got %v", name, err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s produced no output", name)
		}
	}
	// The help subcommand's output is the assign help, not the root's.
	out, err := execute("help", "assign")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--roster") {
		t.Errorf("help assign should render assign's flags:\n%s", out)
	}
	// A command that does work still fails fast without a config.
	if _, err := execute("assign", "hw1", "-r", "roster.csv"); err == nil {
		t.Error("assign without a config should still error")
	}
}

func TestVersionFlag(t *testing.T) {
	out, err := execute("--version")
	if err != nil {
		t.Fatalf("--version should succeed, got %v", err)
	}
	if !strings.Contains(out, resolveVersion()) {
		t.Errorf("--version output %q should contain the resolved version %q", out, resolveVersion())
	}
}

func TestResolveVersionPrefersStamp(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v1.2.3"
	if got := resolveVersion(); got != "v1.2.3" {
		t.Errorf("stamped version should win, got %q", got)
	}
	// Without a stamp it must still yield a non-empty value (build info or "dev").
	version = "dev"
	if got := resolveVersion(); got == "" {
		t.Error("resolveVersion must never be empty")
	}
}

func TestConcurrencyDefault(t *testing.T) {
	j, err := NewRootCmd().PersistentFlags().GetInt("concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if j != defaultConcurrency {
		t.Errorf("default concurrency = %d, want %d", j, defaultConcurrency)
	}
}

func TestRequestLogAppendsAcrossRuns(t *testing.T) {
	// Two runs pointed at one path must accumulate. Truncating would destroy the
	// log of the run the instructor is comparing against, which on a diagnostic
	// run is the whole artifact.
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	g := &globalOpts{logPath: path}

	for _, line := range []string{"first run\n", "second run\n"} {
		if err := g.openRequestLog(); err != nil {
			t.Fatal(err)
		}
		if _, err := g.logFile.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := g.closeRequestLog(); err != nil {
			t.Fatal(err)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first run\nsecond run\n" {
		t.Errorf("log holds %q, want both runs", b)
	}
}

func TestRequestLogUnwritablePathFailsBeforeTheRun(t *testing.T) {
	// The log opens in the root's pre-run, before the command touches GitHub, so
	// a path that cannot be written costs nothing but the message.
	g := &globalOpts{logPath: filepath.Join(t.TempDir(), "no-such-dir", "requests.jsonl")}

	err := g.openRequestLog()
	if err == nil {
		t.Fatal("want an error for a path that cannot be opened")
	}
	t.Logf("message as the instructor sees it:\n  error: %v", err)
	if !strings.Contains(err.Error(), "--log-requests") {
		t.Errorf("the message should name the flag to fix, got: %v", err)
	}
	if g.logFile != nil {
		t.Error("a failed open must leave no file behind")
	}
}

func TestNoRequestLogUnlessAsked(t *testing.T) {
	g := &globalOpts{}
	if err := g.openRequestLog(); err != nil {
		t.Fatal(err)
	}
	if g.logFile != nil {
		t.Error("no log should be opened without --log-requests")
	}
	if err := g.closeRequestLog(); err != nil {
		t.Errorf("closing a log that was never opened: %v", err)
	}
}
