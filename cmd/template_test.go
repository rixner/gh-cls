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
	markWontTake bool            // SetRepoTemplate "succeeds" but the is_template flag never sticks
	noContent    map[string]bool // "owner/name" repos whose branches do not resolve (empty repo)
	forcePublic  bool            // generation produces a public repo regardless of the request
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
	f.repos[key] = &gh.Repo{Name: name, Private: private && !f.forcePublic, DefaultBranch: "main"}
	f.generated = append(f.generated, generateCall{tmpl: tmplOwner + "/" + tmplRepo, dst: key, private: private})
	return nil
}

func (f *fakeTemplateClient) BranchExists(_ context.Context, owner, name, branch string) (bool, error) {
	key := owner + "/" + name
	if f.noContent[key] {
		return false, nil
	}
	r, ok := f.repos[key]
	return ok && r.DefaultBranch == branch, nil
}

func (f *fakeTemplateClient) DeleteRepo(_ context.Context, org, name string) error {
	f.deleted = append(f.deleted, org+"/"+name)
	delete(f.repos, org+"/"+name)
	return nil
}

func newTemplateOpts(t *testing.T, fake *fakeTemplateClient, source string, force, dryRun bool) *templateOpts {
	t.Helper()
	return &templateOpts{
		g:         &globalOpts{org: "cs101-spring26"},
		source:    source,
		force:     force,
		dryRun:    dryRun,
		newClient: func(context.Context) (templateClient, error) { return fake, nil },
		sleep:     func(time.Duration) {},
	}
}

// withSource returns a source repo that is already a template repository — the
// normal, pre-requisite-satisfied state.
func withSource() map[string]*gh.Repo {
	return map[string]*gh.Repo{
		"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main", IsTemplate: true},
	}
}

func TestTemplateGeneratesFromSource(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1-template"); err != nil {
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
		t.Error("template should be generated private")
	}
	// The source is already a template; only the new output repo gets marked.
	if contains(fake.templated, "cs101-templates/hw1-starter") {
		t.Errorf("an already-template source must not be re-marked: %v", fake.templated)
	}
	if !contains(fake.templated, "cs101-spring26/hw1-template") {
		t.Errorf("the output repo should be marked a template: %v", fake.templated)
	}
	if !strings.Contains(buf.String(), "Created cs101-spring26/hw1-template") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestTemplateBareOutputDefaultsToOrg(t *testing.T) {
	// A bare <repo> argument is created in the configured org.
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if fake.generated[0].dst != "cs101-spring26/hw1-template" {
		t.Errorf("bare output should be created in the org, got %q", fake.generated[0].dst)
	}
}

func TestTemplateSourceRequiresOwner(t *testing.T) {
	// --source must be a full owner/name; a bare name is rejected so the source org
	// is always explicit.
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, fake, "hw1-starter" /*bare source*/, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("a bare --source should be rejected, got %v", err)
	}
}

func TestTemplateSourceMustBeTemplate(t *testing.T) {
	// The source is not a template repository and --mark-source was not given:
	// fail with guidance rather than silently flipping someone's repo.
	repos := map[string]*gh.Repo{"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main"}}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "--mark-source") {
		t.Fatalf("a non-template source should fail pointing at --mark-source, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("nothing should be generated when the source is not a template")
	}
}

func TestTemplateMarkSource(t *testing.T) {
	// --mark-source opts into marking the source a template repository, then proceeds.
	repos := map[string]*gh.Repo{"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main"}}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)
	o.markSource = true

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.templated, "cs101-templates/hw1-starter") {
		t.Errorf("--mark-source should mark the source a template: %v", fake.templated)
	}
	if len(fake.generated) != 1 {
		t.Errorf("generation should proceed after marking the source: %v", fake.generated)
	}
}

func TestQualifyTemplate(t *testing.T) {
	if got := qualifyTemplate("hw1-template", "cs101"); got != "cs101/hw1-template" {
		t.Errorf("bare name = %q, want cs101/hw1-template", got)
	}
	if got := qualifyTemplate("other-org/hw1", "cs101"); got != "other-org/hw1" {
		t.Errorf("owner-qualified name = %q, want it unchanged", got)
	}
}

func TestTemplateAbortsIfExists(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("nothing should be generated when the output exists and -F is absent")
	}
}

func TestTemplateForceOverwrites(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", true, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "cs101-spring26/hw1-template" {
		t.Errorf("force should delete the existing output, deleted = %v", fake.deleted)
	}
	if len(fake.generated) != 1 {
		t.Errorf("force should regenerate the output, generated = %v", fake.generated)
	}
}

func TestTemplateSourceNotFound(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: map[string]*gh.Repo{}}
	o := newTemplateOpts(t, fake, "cs101-templates/missing", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want source-not-found error, got %v", err)
	}
}

func TestTemplateOwnerGuard(t *testing.T) {
	fake := &fakeTemplateClient{role: "member", repos: withSource()}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "owner") {
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
	if err := o.run(context.Background(), &buf, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if len(fake.generated) != 0 || len(fake.templated) != 0 {
		t.Error("dry-run must not generate or mark anything")
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should be labeled: %s", buf.String())
	}
}

func TestTemplateRejectsEmptySource(t *testing.T) {
	// The source repo exists but has no commits (its default branch does not
	// resolve). This must be caught up front, before the existing output is
	// deleted on --force, so a bad run never destroys a good template.
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{
		role:      "admin",
		repos:     repos,
		noContent: map[string]bool{"cs101-templates/hw1-starter": true},
	}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", true /*force*/, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("empty source should be rejected, got %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("the existing output must not be deleted when the source is invalid: %v", fake.deleted)
	}
	if len(fake.generated) != 0 {
		t.Errorf("nothing should be generated from an empty source: %v", fake.generated)
	}
}

func TestTemplateRollsBackWhenNotPrivate(t *testing.T) {
	// Generation yields a public repo despite the private request. Starter code
	// must not be world-readable, so the command must catch it and roll back.
	fake := &fakeTemplateClient{role: "admin", repos: withSource(), forcePublic: true}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "must be private") {
		t.Fatalf("a public output should be rejected, got %v", err)
	}
	if !contains(fake.deleted, "cs101-spring26/hw1-template") {
		t.Errorf("the public output should be rolled back: %v", fake.deleted)
	}
}

func TestTemplateRollsBackWhenMarkFails(t *testing.T) {
	// The repo generates, but marking it a template never takes effect. The
	// command must verify that post-condition, fail, and roll back the unusable
	// repo so no broken template is left for assign to generate from.
	fake := &fakeTemplateClient{role: "admin", repos: withSource(), markWontTake: true}
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
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
