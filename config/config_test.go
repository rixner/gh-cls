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

	t.Run("invalid assignment type is rejected", func(t *testing.T) {
		if _, err := Load(write(t, "org: x\nassignments:\n  hw1:\n    type: bogus\n")); err == nil {
			t.Fatal("Load should reject an invalid assignment type")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
			t.Fatal("Load should error on a missing file")
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
