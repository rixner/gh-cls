package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
// only from the config file: no command — not even setup — accepts them as
// flags, so a stray -o/--org can never target an unconfigured org.
func TestOrgIsConfigOnly(t *testing.T) {
	if NewRootCmd().PersistentFlags().Lookup("org") != nil {
		t.Error("--org must not be a flag")
	}
	for _, name := range []string{"setup", "staff", "template", "assign", "freeze", "audit"} {
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
		"staff":    {"t": "tas", "n": "dry-run"},
		"template": {"s": "source", "F": "force", "n": "dry-run"},
		"assign":   {"r": "roster", "T": "teams", "p": "public", "b": "branch-protection", "a": "all-branches", "f": "feedback", "U": "allow-unsquashed", "n": "dry-run"},
		"freeze":   {"u": "undo", "n": "dry-run"},
		"audit":    {"r": "roster", "T": "teams", "n": "dry-run"},
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
	_, err := execute("assign", "hw1", "-r", "roster.csv", "-f", "bogus")
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
