package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
)

// generateCall records one GenerateFromTemplate invocation.
type generateCall struct {
	tmpl, dst string
	private   bool
}

// fakeTemplateClient stands in for the GitHub operations template uses, keyed by
// "owner/name", recording mutations.
type fakeTemplateClient struct {
	role         string
	repos        map[string]*gh.Repo
	generated    []generateCall
	deleted      []string
	templated    []string
	markWontTake bool // SetRepoTemplate "succeeds" but the is_template flag never sticks
}

func (f *fakeTemplateClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeTemplateClient) GetRepo(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
	r, ok := f.repos[owner+"/"+name]
	return r, ok, nil
}

func (f *fakeTemplateClient) SetRepoTemplate(_ context.Context, owner, name string) error {
	key := owner + "/" + name
	f.templated = append(f.templated, key)
	if r, ok := f.repos[key]; ok && !f.markWontTake {
		r.IsTemplate = true
	}
	return nil
}

func (f *fakeTemplateClient) GenerateFromTemplate(_ context.Context, tmplOwner, tmplRepo, owner, name string, private, _ bool) error {
	if f.repos == nil {
		f.repos = map[string]*gh.Repo{}
	}
	key := owner + "/" + name
	f.repos[key] = &gh.Repo{Name: name, Private: private, DefaultBranch: "main"}
	f.generated = append(f.generated, generateCall{tmpl: tmplOwner + "/" + tmplRepo, dst: key, private: private})
	return nil
}

func (f *fakeTemplateClient) BranchExists(_ context.Context, owner, name, branch string) (bool, error) {
	r, ok := f.repos[owner+"/"+name]
	return ok && r.DefaultBranch == branch, nil
}

func (f *fakeTemplateClient) DeleteRepo(_ context.Context, org, name string) error {
	f.deleted = append(f.deleted, org+"/"+name)
	delete(f.repos, org+"/"+name)
	return nil
}

func newTemplateOpts(t *testing.T, fake *fakeTemplateClient, source string, force, dryRun bool) *templateOpts {
	t.Helper()
	t.Setenv("GH_CLS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	return &templateOpts{
		g:         &globalOpts{org: "cs101-spring26"},
		source:    source,
		force:     force,
		dryRun:    dryRun,
		newClient: func(context.Context) (templateClient, error) { return fake, nil },
		sleep:     func(time.Duration) {},
	}
}

func withSource() map[string]*gh.Repo {
	return map[string]*gh.Repo{
		"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main"},
	}
}

func TestTemplateGeneratesFromSource(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.generated) != 1 {
		t.Fatalf("generated = %v", fake.generated)
	}
	g := fake.generated[0]
	if g.tmpl != "cs101-templates/hw1-starter" {
		t.Errorf("generate source = %q, want cs101-templates/hw1-starter", g.tmpl)
	}
	if g.dst != "cs101-spring26/hw1-template" {
		t.Errorf("generate dst = %q, want cs101-spring26/hw1-template", g.dst)
	}
	if !g.private {
		t.Error("derived template should be generated private")
	}
	// The source must be marked a template (required to generate from it), and so
	// must the derived repo (so assign can generate student repos from it).
	if !contains(fake.templated, "cs101-templates/hw1-starter") {
		t.Errorf("source should be marked a template, templated = %v", fake.templated)
	}
	if !contains(fake.templated, "cs101-spring26/hw1-template") {
		t.Errorf("derived repo should be marked a template, templated = %v", fake.templated)
	}
	if !strings.Contains(buf.String(), "Created cs101-spring26/hw1-template") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

// A source already marked a template needs no re-marking.
func TestTemplateSkipsMarkingTemplateSource(t *testing.T) {
	repos := map[string]*gh.Repo{
		"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main", IsTemplate: true},
	}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	if contains(fake.templated, "cs101-templates/hw1-starter") {
		t.Errorf("an already-template source should not be re-marked, templated = %v", fake.templated)
	}
}

func TestTemplateAbortsIfExists(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("nothing should be generated when the template exists and -F is absent")
	}
}

func TestTemplateForceOverwrites(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", true, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "cs101-spring26/hw1-template" {
		t.Errorf("force should delete the existing template, deleted = %v", fake.deleted)
	}
	if len(fake.generated) != 1 {
		t.Errorf("force should regenerate the template, generated = %v", fake.generated)
	}
}

func TestTemplateSourceNotFound(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: map[string]*gh.Repo{}}
	o := newTemplateOpts(t, fake, "cs101-templates/missing", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want source-not-found error, got %v", err)
	}
}

func TestTemplateOwnerGuard(t *testing.T) {
	fake := &fakeTemplateClient{role: "member", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("nothing should be generated when the owner guard fails")
	}
}

func TestTemplateDryRun(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.generated) != 0 || len(fake.templated) != 0 {
		t.Error("dry-run must not generate or mark anything")
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should be labeled: %s", buf.String())
	}
}

func TestTemplateRollsBackWhenMarkFails(t *testing.T) {
	// The repo generates, but marking it a template never takes effect. The
	// command must verify that post-condition, fail, and roll back the unusable
	// repo so no broken <name>-template is left for assign to generate from.
	fake := &fakeTemplateClient{role: "admin", repos: withSource(), markWontTake: true}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "not marked a template") {
		t.Fatalf("want a post-condition failure about the template flag, got %v", err)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should report the rollback: %v", err)
	}
	if !contains(fake.deleted, "cs101-spring26/hw1-template") {
		t.Errorf("the unusable repo should be rolled back (deleted), deleted = %v", fake.deleted)
	}
	if _, ok := fake.repos["cs101-spring26/hw1-template"]; ok {
		t.Error("no broken template repo should remain after rollback")
	}
}
