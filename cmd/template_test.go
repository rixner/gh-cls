package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
)

type squashCall struct{ src, dst, msg string }

// fakeTemplateClient stands in for the GitHub operations template uses, keyed by
// "owner/name", recording mutations.
type fakeTemplateClient struct {
	role      string
	repos     map[string]*gh.Repo
	created   []string
	deleted   []string
	templated []string
}

func (f *fakeTemplateClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeTemplateClient) GetRepo(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
	r, ok := f.repos[owner+"/"+name]
	return r, ok, nil
}

func (f *fakeTemplateClient) CreateOrgRepo(_ context.Context, org, name string, _ bool) (*gh.Repo, error) {
	r := &gh.Repo{Name: name, CloneURL: "https://example/" + org + "/" + name + ".git"}
	if f.repos == nil {
		f.repos = map[string]*gh.Repo{}
	}
	f.repos[org+"/"+name] = r
	f.created = append(f.created, org+"/"+name)
	return r, nil
}

func (f *fakeTemplateClient) SetRepoTemplate(_ context.Context, org, name string) error {
	f.templated = append(f.templated, org+"/"+name)
	return nil
}

func (f *fakeTemplateClient) DeleteRepo(_ context.Context, org, name string) error {
	f.deleted = append(f.deleted, org+"/"+name)
	delete(f.repos, org+"/"+name)
	return nil
}

func newTemplateOpts(t *testing.T, fake *fakeTemplateClient, source string, force, dryRun bool, rec *squashCall) *templateOpts {
	t.Helper()
	t.Setenv("GH_CLS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	return &templateOpts{
		g:         &globalOpts{org: "cs101-spring26"},
		source:    source,
		message:   defaultSquashMessage,
		force:     force,
		dryRun:    dryRun,
		newClient: func(context.Context) (templateClient, error) { return fake, nil },
		squash: func(_ context.Context, src, dst, msg string) (string, error) {
			*rec = squashCall{src, dst, msg}
			return "main", nil
		},
	}
}

func withSource() map[string]*gh.Repo {
	return map[string]*gh.Repo{
		"cs101-templates/hw1-starter": {Name: "hw1-starter", CloneURL: "https://github.com/cs101-templates/hw1-starter.git"},
	}
}

func TestTemplateCreatesSquashed(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false, &rec)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 1 || fake.created[0] != "cs101-spring26/hw1-template" {
		t.Errorf("created = %v", fake.created)
	}
	if rec.src != "https://github.com/cs101-templates/hw1-starter.git" {
		t.Errorf("squash source = %q", rec.src)
	}
	if rec.dst != "https://example/cs101-spring26/hw1-template.git" || rec.msg != defaultSquashMessage {
		t.Errorf("squash dst/msg = %q / %q", rec.dst, rec.msg)
	}
	if len(fake.templated) != 1 {
		t.Errorf("derived repo should be marked a template, got %v", fake.templated)
	}
	if !strings.Contains(buf.String(), "Created cs101-spring26/hw1-template") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestTemplateAbortsIfExists(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false, &rec)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
	if len(fake.created) != 0 || rec.src != "" {
		t.Error("nothing should be created when the template exists and -F is absent")
	}
}

func TestTemplateForceOverwrites(t *testing.T) {
	repos := withSource()
	repos["cs101-spring26/hw1-template"] = &gh.Repo{Name: "hw1-template"}
	fake := &fakeTemplateClient{role: "admin", repos: repos}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", true, false, &rec)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "cs101-spring26/hw1-template" {
		t.Errorf("force should delete the existing template, deleted = %v", fake.deleted)
	}
	if len(fake.created) != 1 {
		t.Errorf("force should recreate the template, created = %v", fake.created)
	}
}

func TestTemplateSourceNotFound(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: map[string]*gh.Repo{}}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/missing", false, false, &rec)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want source-not-found error, got %v", err)
	}
}

func TestTemplateOwnerGuard(t *testing.T) {
	fake := &fakeTemplateClient{role: "member", repos: withSource()}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, false, &rec)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("no repo should be created when the owner guard fails")
	}
}

func TestTemplateDryRun(t *testing.T) {
	fake := &fakeTemplateClient{role: "admin", repos: withSource()}
	var rec squashCall
	o := newTemplateOpts(t, fake, "cs101-templates/hw1-starter", false, true, &rec)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.created) != 0 || rec.src != "" {
		t.Error("dry-run must not create or squash anything")
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should be labeled: %s", buf.String())
	}
}
