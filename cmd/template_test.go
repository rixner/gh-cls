package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// generateCall records one GenerateFromTemplate invocation.
type generateCall struct {
	tmpl, dst string
	private   bool
}

// fakeTemplateState configures a ghtest.Fake for template tests, keyed by
// "owner/name", and captures its mutations.
type fakeTemplateState struct {
	role         string
	repos        map[string]*gh.Repo
	markWontTake bool            // SetRepoTemplate "succeeds" but the is_template flag never sticks
	noContent    map[string]bool // "owner/name" repos whose branches do not resolve (empty repo)
	forcePublic  bool            // generation produces a public repo regardless of the request

	generated []generateCall
	deleted   []string
	templated []string
}

func (s *fakeTemplateState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.GetRepoFunc = func(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
		r, ok := s.repos[owner+"/"+name]
		return r, ok, nil
	}
	fk.SetRepoTemplateFunc = func(_ context.Context, owner, name string) error {
		key := owner + "/" + name
		s.templated = append(s.templated, key)
		if r, ok := s.repos[key]; ok && !s.markWontTake {
			r.IsTemplate = true
		}
		return nil
	}
	fk.GenerateFromTemplateFunc = func(_ context.Context, tmplOwner, tmplRepo, owner, name string, private, _ bool) error {
		if s.repos == nil {
			s.repos = map[string]*gh.Repo{}
		}
		key := owner + "/" + name
		s.repos[key] = &gh.Repo{Name: name, Private: private && !s.forcePublic, DefaultBranch: "main"}
		s.generated = append(s.generated, generateCall{tmpl: tmplOwner + "/" + tmplRepo, dst: key, private: private})
		return nil
	}
	fk.BranchExistsFunc = func(_ context.Context, owner, name, branch string) (bool, error) {
		key := owner + "/" + name
		if s.noContent[key] {
			return false, nil
		}
		r, ok := s.repos[key]
		return ok && r.DefaultBranch == branch, nil
	}
	fk.DeleteRepoFunc = func(_ context.Context, org, name string) error {
		s.deleted = append(s.deleted, org+"/"+name)
		delete(s.repos, org+"/"+name)
		return nil
	}
	return fk
}

func newTemplateOpts(t *testing.T, state *fakeTemplateState, source string, force, dryRun bool) *templateOpts {
	t.Helper()
	fk := state.fake()
	return &templateOpts{
		g:         &globalOpts{org: "cs101-spring26"},
		source:    source,
		force:     force,
		dryRun:    dryRun,
		newClient: func(context.Context) (templateClient, error) { return fk, nil },
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
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if len(state.generated) != 1 {
		t.Fatalf("generated = %v", state.generated)
	}
	g := state.generated[0]
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
	if contains(state.templated, "cs101-templates/hw1-starter") {
		t.Errorf("an already-template source must not be re-marked: %v", state.templated)
	}
	if !contains(state.templated, "cs101-spring26/hw1-template") {
		t.Errorf("the output repo should be marked a template: %v", state.templated)
	}
	if !strings.Contains(buf.String(), "Created cs101-spring26/hw1-template") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestTemplateBareOutputDefaultsToOrg(t *testing.T) {
	// A bare <repo> argument is created in the configured org.
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if state.generated[0].dst != "cs101-spring26/hw1-template" {
		t.Errorf("bare output should be created in the org, got %q", state.generated[0].dst)
	}
}

func TestTemplateRejectsDestinationSameAsSource(t *testing.T) {
	// With --force, an existing destination is deleted before generating from the
	// source. If destination and source name the same repository, that delete
	// would destroy the source. This must be caught before anything is deleted.
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", true /*force*/, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "cs101-templates/hw1-starter")
	if err == nil || !strings.Contains(err.Error(), "same repository") {
		t.Fatalf("destination == source should be rejected, got %v", err)
	}
	if len(state.deleted) != 0 {
		t.Errorf("the source must never be deleted, deleted = %v", state.deleted)
	}
	if len(state.generated) != 0 {
		t.Errorf("nothing should be generated when destination == source: %v", state.generated)
	}
}

func TestTemplateRejectsDestinationSameAsSourceDifferentCase(t *testing.T) {
	// GitHub owner and repo names are case-insensitive, so a case variation of the
	// same repository must be rejected too.
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "CS101-Templates/HW1-Starter", true /*force*/, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "cs101-templates/hw1-starter")
	if err == nil || !strings.Contains(err.Error(), "same repository") {
		t.Fatalf("a case-variant destination == source should be rejected, got %v", err)
	}
	if len(state.deleted) != 0 {
		t.Errorf("the source must never be deleted, deleted = %v", state.deleted)
	}
}

func TestTemplateRejectsBareDestinationSameAsSource(t *testing.T) {
	// A bare <repo> argument is qualified with the configured org before the
	// comparison, so it must also be caught when --source names the same repo in
	// that org.
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-spring26/hw1", true /*force*/, false)
	state.repos["cs101-spring26/hw1"] = &gh.Repo{Name: "hw1", DefaultBranch: "main", IsTemplate: true}

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "same repository") {
		t.Fatalf("a bare destination resolving to the source should be rejected, got %v", err)
	}
	if len(state.deleted) != 0 {
		t.Errorf("the source must never be deleted, deleted = %v", state.deleted)
	}
}

func TestTemplateDryRunRejectsDestinationSameAsSource(t *testing.T) {
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, true /*dryRun*/)

	err := o.run(context.Background(), &bytes.Buffer{}, "cs101-templates/hw1-starter")
	if err == nil || !strings.Contains(err.Error(), "same repository") {
		t.Fatalf("dry-run with destination == source should still be rejected, got %v", err)
	}
}

func TestTemplateSourceRequiresOwner(t *testing.T) {
	// --source must be a full owner/name; a bare name is rejected so the source org
	// is always explicit.
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "hw1-starter" /*bare source*/, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("a bare --source should be rejected, got %v", err)
	}
}

func TestTemplateSourceMustBeTemplate(t *testing.T) {
	// The source is not a template repository and --mark-source was not given:
	// fail with guidance rather than silently flipping someone's repo.
	repos := map[string]*gh.Repo{"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main"}}
	state := &fakeTemplateState{role: "admin", repos: repos}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "--mark-source") {
		t.Fatalf("a non-template source should fail pointing at --mark-source, got %v", err)
	}
	if len(state.generated) != 0 {
		t.Error("nothing should be generated when the source is not a template")
	}
}

func TestTemplateMarkSource(t *testing.T) {
	// --mark-source opts into marking the source a template repository, then proceeds.
	repos := map[string]*gh.Repo{"cs101-templates/hw1-starter": {Name: "hw1-starter", DefaultBranch: "main"}}
	state := &fakeTemplateState{role: "admin", repos: repos}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)
	o.markSource = true

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if !contains(state.templated, "cs101-templates/hw1-starter") {
		t.Errorf("--mark-source should mark the source a template: %v", state.templated)
	}
	if len(state.generated) != 1 {
		t.Errorf("generation should proceed after marking the source: %v", state.generated)
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
	state := &fakeTemplateState{role: "admin", repos: repos}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
	if len(state.generated) != 0 {
		t.Error("nothing should be generated when the output exists and -F is absent")
	}
}

func TestTemplateForceOverwrites(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	state := &fakeTemplateState{role: "admin", repos: repos}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", true, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if len(state.deleted) != 1 || state.deleted[0] != "cs101-spring26/hw1-template" {
		t.Errorf("force should delete the existing output, deleted = %v", state.deleted)
	}
	if len(state.generated) != 1 {
		t.Errorf("force should regenerate the output, generated = %v", state.generated)
	}
}

func TestTemplateSourceNotFound(t *testing.T) {
	state := &fakeTemplateState{role: "admin", repos: map[string]*gh.Repo{}}
	o := newTemplateOpts(t, state, "cs101-templates/missing", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want source-not-found error, got %v", err)
	}
}

func TestTemplateOwnerGuard(t *testing.T) {
	state := &fakeTemplateState{role: "member", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if len(state.generated) != 0 {
		t.Error("nothing should be generated when the owner guard fails")
	}
}

func TestTemplateDryRun(t *testing.T) {
	state := &fakeTemplateState{role: "admin", repos: withSource()}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if len(state.generated) != 0 || len(state.templated) != 0 {
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
	state := &fakeTemplateState{
		role:      "admin",
		repos:     repos,
		noContent: map[string]bool{"cs101-templates/hw1-starter": true},
	}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", true /*force*/, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("empty source should be rejected, got %v", err)
	}
	if len(state.deleted) != 0 {
		t.Errorf("the existing output must not be deleted when the source is invalid: %v", state.deleted)
	}
	if len(state.generated) != 0 {
		t.Errorf("nothing should be generated from an empty source: %v", state.generated)
	}
}

func TestTemplateRollsBackWhenNotPrivate(t *testing.T) {
	// Generation yields a public repo despite the private request. Starter code
	// must not be world-readable, so the command must catch it and roll back.
	state := &fakeTemplateState{role: "admin", repos: withSource(), forcePublic: true}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "must be private") {
		t.Fatalf("a public output should be rejected, got %v", err)
	}
	if !contains(state.deleted, "cs101-spring26/hw1-template") {
		t.Errorf("the public output should be rolled back: %v", state.deleted)
	}
}

func TestTemplateRollsBackWhenMarkFails(t *testing.T) {
	// The repo generates, but marking it a template never takes effect. The
	// command must verify that post-condition, fail, and roll back the unusable
	// repo so no broken template is left for assign to generate from.
	state := &fakeTemplateState{role: "admin", repos: withSource(), markWontTake: true}
	o := newTemplateOpts(t, state, "cs101-templates/hw1-starter", false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1-template")
	if err == nil || !strings.Contains(err.Error(), "not marked a template") {
		t.Fatalf("want a post-condition failure about the template flag, got %v", err)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should report the rollback: %v", err)
	}
	if !contains(state.deleted, "cs101-spring26/hw1-template") {
		t.Errorf("the unusable repo should be rolled back (deleted), deleted = %v", state.deleted)
	}
	if _, ok := state.repos["cs101-spring26/hw1-template"]; ok {
		t.Error("no broken template repo should remain after rollback")
	}
}
