package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestSearchPrecedence(t *testing.T) {
	// Isolate the environment and working directory for each case.
	t.Run("env wins", func(t *testing.T) {
		t.Setenv(envVar, "/explicit/path.yml")
		if got := Search(); got != "/explicit/path.yml" {
			t.Errorf("Search() = %q, want the env path", got)
		}
	})

	t.Run("working dir file", func(t *testing.T) {
		t.Setenv(envVar, "")
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile(workingDirFile, []byte("org: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Search(); got != workingDirFile {
			t.Errorf("Search() = %q, want %q", got, workingDirFile)
		}
	})

	t.Run("xdg fallback", func(t *testing.T) {
		t.Setenv(envVar, "")
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Chdir(t.TempDir()) // no working-dir file here
		want := filepath.Join(xdg, "gh-cls", "config.yml")
		if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(want, []byte("org: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Search(); got != want {
			t.Errorf("Search() = %q, want %q", got, want)
		}
	})

	t.Run("none found", func(t *testing.T) {
		t.Setenv(envVar, "")
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Chdir(t.TempDir())
		if got := Search(); got != "" {
			t.Errorf("Search() = %q, want empty", got)
		}
	})
}

func TestLoadMissingIsNotError(t *testing.T) {
	t.Setenv(envVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	c, path, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if path != "" || c == nil || c.Org != "" {
		t.Errorf("Load() = (%+v, %q), want empty config and path", c, path)
	}
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("assignments:\n  hw1:\n    type: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile should reject an invalid assignment type")
	}
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
		p, err := c.Resolve("hw1", Overrides{Public: ptr(false), Template: "other/repo", Feedback: ptr(FeedbackPR)})
		if err != nil {
			t.Fatal(err)
		}
		if p.Public || p.Template != "other/repo" || p.Feedback != FeedbackPR {
			t.Errorf("override not applied: %+v", p)
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

	t.Run("missing template", func(t *testing.T) {
		bad := &Config{Assignments: map[string]Assignment{"x": {Type: TypeIndividual}}}
		if _, err := bad.Resolve("x", Overrides{}); err == nil {
			t.Error("want error when no template configured or overridden")
		}
	})
}

func TestWriteOrgPreservesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	original := `# course structure
org: cs101-fall25
staff_team: staff

assignments:
  hw1:
    type: individual
    template: cs101-templates/hw1-starter
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	prev, err := WriteOrg(path, "cs101-spring26")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "cs101-fall25" {
		t.Errorf("previous org = %q, want cs101-fall25", prev)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "org: cs101-spring26") {
		t.Errorf("new org not written:\n%s", text)
	}
	for _, want := range []string{"# course structure", "staff_team: staff", "template: cs101-templates/hw1-starter"} {
		if !strings.Contains(text, want) {
			t.Errorf("WriteOrg dropped %q:\n%s", want, text)
		}
	}

	// The rewritten file must still parse and reflect only the org change.
	c, _, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Org != "cs101-spring26" || c.StaffTeam != "staff" || len(c.Assignments) != 1 {
		t.Errorf("reloaded config wrong: %+v", c)
	}
}

func TestWriteOrgCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yml")
	prev, err := WriteOrg(path, "brand-new-org")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "" {
		t.Errorf("previous = %q, want empty for a new file", prev)
	}
	c, _, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Org != "brand-new-org" {
		t.Errorf("org = %q, want brand-new-org", c.Org)
	}
}
