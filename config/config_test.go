package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestResolvePath(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envVar, "/from/env.yml")
		got, err := ResolvePath("/from/flag.yml")
		if err != nil || got != "/from/flag.yml" {
			t.Fatalf("ResolvePath = (%q, %v), want the flag path", got, err)
		}
	})
	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv(envVar, "/from/env.yml")
		got, err := ResolvePath("")
		if err != nil || got != "/from/env.yml" {
			t.Fatalf("ResolvePath = (%q, %v), want the env path", got, err)
		}
	})
	t.Run("error when neither is set", func(t *testing.T) {
		t.Setenv(envVar, "")
		if _, err := ResolvePath(""); err == nil {
			t.Fatal("ResolvePath should error when neither -c nor the env var is set")
		}
	})
}

func TestLoad(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("valid", func(t *testing.T) {
		c, err := Load(write(t, "org: cs101-spring26\nstaff_team: staff\nassignments:\n  hw1:\n    type: individual\n    template: o/t\n"))
		if err != nil {
			t.Fatal(err)
		}
		if c.Org != "cs101-spring26" || c.StaffTeam != "staff" || len(c.Assignments) != 1 {
			t.Errorf("parsed config wrong: %+v", c)
		}
	})

	t.Run("missing org is rejected", func(t *testing.T) {
		_, err := Load(write(t, "staff_team: staff\n"))
		if err == nil || !strings.Contains(err.Error(), "org") {
			t.Fatalf("a config without org should error mentioning org, got %v", err)
		}
	})

	t.Run("missing staff_team is rejected", func(t *testing.T) {
		_, err := Load(write(t, "org: cs101-spring26\n"))
		if err == nil || !strings.Contains(err.Error(), "staff_team") {
			t.Fatalf("a config without staff_team should error mentioning staff_team, got %v", err)
		}
	})

	t.Run("invalid assignment type is rejected", func(t *testing.T) {
		if _, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  hw1:\n    type: bogus\n")); err == nil {
			t.Fatal("Load should reject an invalid assignment type")
		}
	})

	t.Run("a misspelled course key is rejected", func(t *testing.T) {
		// Dropped silently, this is a whole class of repositories created without the
		// feedback artifact the config asked for, and nothing in the run says so.
		_, err := Load(write(t, "org: x\nstaff_team: staff\nfeedbck: pr\n"))
		if err == nil {
			t.Fatal("Load should reject an unknown top-level key")
		}
		if !strings.Contains(err.Error(), "feedbck") || !strings.Contains(err.Error(), "line 3") {
			t.Fatalf("the error should name the key and its line, got %v", err)
		}
	})

	t.Run("a misspelled assignment key is rejected", func(t *testing.T) {
		_, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  hw1:\n    type: individual\n    branch_protecton: true\n"))
		if err == nil {
			t.Fatal("Load should reject an unknown per-assignment key")
		}
		if !strings.Contains(err.Error(), "branch_protecton") || !strings.Contains(err.Error(), "line 6") {
			t.Fatalf("the error should name the key and its line, got %v", err)
		}
		// The message is the user's, so it must not leak the Go type it came from.
		if strings.Contains(err.Error(), "config.Assignment") {
			t.Errorf("the error should not name a Go type, got %v", err)
		}
	})

	t.Run("an empty config still reports the required keys", func(t *testing.T) {
		// An empty file decodes as EOF; the useful answer is Validate's, not "EOF".
		_, err := Load(write(t, ""))
		if err == nil || !strings.Contains(err.Error(), "org") {
			t.Fatalf("an empty config should ask for org, got %v", err)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
			t.Fatal("Load should error on a missing file")
		}
	})

	t.Run("non-overlapping assignment names are accepted", func(t *testing.T) {
		_, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  hw1:\n    type: individual\n  hw2:\n    type: individual\n"))
		if err != nil {
			t.Fatalf("hw1/hw2 should not be rejected as overlapping, got %v", err)
		}
	})

	t.Run("dash-prefix-overlapping assignment names are rejected", func(t *testing.T) {
		_, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  proj:\n    type: individual\n  proj-final:\n    type: individual\n"))
		if err == nil {
			t.Fatal("Load should reject proj/proj-final: proj-final's repos would match proj's <name>-* prefix")
		}
		if !strings.Contains(err.Error(), "proj") || !strings.Contains(err.Error(), "proj-final") {
			t.Fatalf("overlap error should name both assignments, got %v", err)
		}
		// The rejection is correct but the way out is not obvious from "rename one",
		// so the message must point at a separator that works.
		if !strings.Contains(err.Error(), "_") {
			t.Errorf("overlap error should suggest a non-dash separator, got %v", err)
		}
	})

	t.Run("an assignment name that is not repo-name-safe is rejected", func(t *testing.T) {
		// The name is the prefix of every repository the assignment creates. GitHub
		// rewrites a name it cannot use, so the repositories that appear are not the
		// ones assign then waits for and grants access to.
		for _, name := range []string{"hw 1", "hw/1", "hwé"} {
			_, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  \""+name+"\":\n    type: individual\n"))
			if err == nil {
				t.Errorf("assignment %q should be rejected", name)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the error should name the assignment, got %v", err)
			}
		}
		// The characters GitHub keeps verbatim are accepted.
		if _, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  hw1.2_a-b:\n    type: individual\n")); err != nil {
			t.Errorf("a repo-name-safe assignment name should be accepted, got %v", err)
		}
	})

	t.Run("a variant separated by something other than a dash is accepted", func(t *testing.T) {
		// The documented workaround for paired assignments (an in-class exercise and
		// its makeup): only "-" starts a repo prefix, so "_" keeps them distinct.
		_, err := Load(write(t, "org: x\nstaff_team: staff\nassignments:\n  hw1:\n    type: individual\n  hw1_makeup:\n    type: individual\n"))
		if err != nil {
			t.Fatalf("hw1/hw1_makeup should be accepted, got %v", err)
		}
	})
}

func TestResolvePrecedence(t *testing.T) {
	c := &Config{Assignments: map[string]Assignment{
		"hw1":  {Type: TypeIndividual, Template: "org/hw1", Public: ptr(true), Feedback: FeedbackIssue},
		"bare": {Type: TypeGroup, Template: "org/bare"},
	}}

	t.Run("config values when no override", func(t *testing.T) {
		p, err := c.Resolve("hw1", Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if !p.Public || p.Feedback != FeedbackIssue || p.BranchProtection {
			t.Errorf("got %+v, want public+issue, no protection", p)
		}
	})

	t.Run("flag overrides config", func(t *testing.T) {
		p, err := c.Resolve("hw1", Overrides{Public: ptr(false), Feedback: ptr(FeedbackPR)})
		if err != nil {
			t.Fatal(err)
		}
		if p.Public || p.Feedback != FeedbackPR {
			t.Errorf("override not applied: %+v", p)
		}
		// Template is read from config, never overridden.
		if p.Template != "org/hw1" {
			t.Errorf("template = %q, want the config value org/hw1", p.Template)
		}
	})

	t.Run("defaults when unset everywhere", func(t *testing.T) {
		p, err := c.Resolve("bare", Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Public || p.BranchProtection || p.Feedback != FeedbackNone {
			t.Errorf("defaults wrong: %+v", p)
		}
	})

	t.Run("unknown assignment", func(t *testing.T) {
		if _, err := c.Resolve("nope", Overrides{}); err == nil {
			t.Error("want error for unknown assignment")
		}
	})

	t.Run("missing template is allowed", func(t *testing.T) {
		// Resolve no longer requires a template; only assign does, and it checks
		// for itself. This keeps audit/freeze working on a templateless entry.
		bare := &Config{Assignments: map[string]Assignment{"x": {Type: TypeIndividual}}}
		if _, err := bare.Resolve("x", Overrides{}); err != nil {
			t.Errorf("Resolve should not require a template, got %v", err)
		}
	})
}
