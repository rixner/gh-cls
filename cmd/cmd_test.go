package cmd

import (
	"bytes"
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
// shorthands on the root command.
func TestPersistentFlagMatrix(t *testing.T) {
	pf := NewRootCmd().PersistentFlags()
	for short, long := range map[string]string{"o": "org", "s": "staff-team", "j": "concurrency"} {
		f := pf.ShorthandLookup(short)
		if f == nil {
			t.Fatalf("persistent shorthand -%s not defined", short)
		}
		if f.Name != long {
			t.Errorf("persistent -%s maps to %q, want %q", short, f.Name, long)
		}
	}
}

// TestLocalFlagMatrix checks each subcommand's local flags and shorthands. This
// guards the deliberately collision-avoiding letters (-t/-T, -u/-U, -F).
func TestLocalFlagMatrix(t *testing.T) {
	cases := map[string]map[string]string{
		"setup":    {"n": "dry-run"},
		"template": {"t": "template", "m": "message", "F": "force", "n": "dry-run"},
		"assign":   {"r": "roster", "T": "teams", "p": "public", "b": "branch-protection", "a": "all-branches", "f": "feedback", "U": "allow-unsquashed", "n": "dry-run"},
		"freeze":   {"u": "undo", "n": "dry-run"},
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

func TestSetupRequiresOrg(t *testing.T) {
	// Isolate config discovery so dry-run reads nothing from the environment.
	t.Setenv("GH_CLS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	if _, err := execute("setup"); err == nil {
		t.Fatal("setup without --org should error")
	}
	// The success path with hardening is covered in setup_test.go with an
	// injected fake client; here --dry-run keeps the flag check offline.
	if _, err := execute("setup", "--org", "cs101-spring26", "--dry-run"); err != nil {
		t.Fatalf("setup --org --dry-run should succeed, got %v", err)
	}
}

func TestAssignRequiresRoster(t *testing.T) {
	// The full run is covered in assign_test.go with config and a fake client;
	// here we only assert the required-flag enforcement.
	if _, err := execute("assign", "hw1"); err == nil {
		t.Fatal("assign without --roster should error")
	}
}

func TestAssignFeedbackEnum(t *testing.T) {
	// Invalid value is rejected in PreRunE, before any work.
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

func TestConcurrencyDefault(t *testing.T) {
	j, err := NewRootCmd().PersistentFlags().GetInt("concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if j != defaultConcurrency {
		t.Errorf("default concurrency = %d, want %d", j, defaultConcurrency)
	}
}
