package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
)

const assignConfig = `org: cs101-spring26
staff_team: staff
assignments:
  hw1:
    type: individual
    template: cs101-templates/hw1-starter
  project:
    type: group
    template: cs101-templates/project-starter
`

const assignRoster = `identifier,username
student-001,ada
student-002,alan
student-003,grace
`

const assignTeams = `team-alpha: [student-001, student-003]
team-beta: [student-002]
`

// fakeAssignClient is a concurrency-safe stand-in for the assign operations.
type fakeAssignClient struct {
	mu        sync.Mutex
	role      string
	exists    map[string]bool
	branches  []gh.BranchCount
	generated []string
	collabs   []string
	teamRepos []string
}

func (f *fakeAssignClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeAssignClient) GetRepo(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists[owner+"/"+name] {
		return &gh.Repo{Name: name}, true, nil
	}
	return nil, false, nil
}

func (f *fakeAssignClient) ListBranchesWithCommitCount(context.Context, string, string) ([]gh.BranchCount, error) {
	return f.branches, nil
}

func (f *fakeAssignClient) GenerateFromTemplate(_ context.Context, _, _, owner, name string, _, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists[owner+"/"+name] = true
	f.generated = append(f.generated, name)
	return nil
}

func (f *fakeAssignClient) AddCollaborator(_ context.Context, _, repo, username, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collabs = append(f.collabs, repo+":"+username)
	return nil
}

func (f *fakeAssignClient) AddTeamRepo(_ context.Context, _, _, _, repo, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teamRepos = append(f.teamRepos, repo)
	return nil
}

func newFakeAssign(role string) *fakeAssignClient {
	return &fakeAssignClient{
		role:     role,
		exists:   map[string]bool{"cs101-spring26/hw1-template": true, "cs101-spring26/project-template": true},
		branches: []gh.BranchCount{{Name: "main", Commits: 1}},
	}
}

// newAssignOpts wires assignOpts to a fake, isolating config to a temp file.
func newAssignOpts(t *testing.T, fake *fakeAssignClient, rosterCSV, teamsYML string) *assignOpts {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte(assignConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CLS_CONFIG", cfgPath)

	rosterPath := filepath.Join(dir, "roster.csv")
	if err := os.WriteFile(rosterPath, []byte(rosterCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	teamsPath := ""
	if teamsYML != "" {
		teamsPath = filepath.Join(dir, "teams.yml")
		if err := os.WriteFile(teamsPath, []byte(teamsYML), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return &assignOpts{
		g:         &globalOpts{concurrency: 4},
		roster:    rosterPath,
		teams:     teamsPath,
		newClient: func(context.Context) (assignClient, error) { return fake, nil },
		sleep:     func(time.Duration) {},
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestAssignIndividual(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"hw1-ada", "hw1-alan", "hw1-grace"} {
		if !contains(fake.generated, repo) {
			t.Errorf("missing generated repo %q (got %v)", repo, fake.generated)
		}
		if !contains(fake.teamRepos, repo) {
			t.Errorf("staff team not granted on %q", repo)
		}
	}
	if !contains(fake.collabs, "hw1-ada:ada") {
		t.Errorf("student push grant missing: %v", fake.collabs)
	}
	if !strings.Contains(buf.String(), "3 created") {
		t.Errorf("summary wrong: %s", buf.String())
	}
}

func TestAssignGroup(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, assignTeams)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.generated, "project-team-alpha") || !contains(fake.generated, "project-team-beta") {
		t.Errorf("group repos not generated: %v", fake.generated)
	}
	// team-alpha resolves student-001 and student-003 to ada and grace.
	if !contains(fake.collabs, "project-team-alpha:ada") || !contains(fake.collabs, "project-team-alpha:grace") {
		t.Errorf("team members not granted: %v", fake.collabs)
	}
}

func TestAssignGroupRequiresTeams(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "--teams is required") {
		t.Fatalf("group without teams should error, got %v", err)
	}
}

func TestAssignIndividualRejectsTeams(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, assignTeams)
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("individual with teams should error, got %v", err)
	}
}

func TestAssignTemplateMissing(t *testing.T) {
	fake := newFakeAssign("admin")
	delete(fake.exists, "cs101-spring26/hw1-template")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("missing template should error, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("no repos should be generated when the template is missing")
	}
}

func TestAssignUnsquashedAborts(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.branches = []gh.BranchCount{{Name: "main", Commits: 1}, {Name: "solution", Commits: 4}}
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "not fully squashed") {
		t.Fatalf("unsquashed template should abort, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("no repos should be generated when the template is unsquashed")
	}

	// With --allow-unsquashed it proceeds.
	o.allowUnsquashed = true
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{}); err != nil {
		t.Fatalf("--allow-unsquashed should proceed, got %v", err)
	}
	if len(fake.generated) == 0 {
		t.Error("repos should be generated when unsquashed is allowed")
	}
}

func TestAssignIdempotentSkip(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true // already created
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if contains(fake.generated, "hw1-ada") {
		t.Error("existing repo should be skipped for generation")
	}
	// Grants are still re-asserted on the skipped repo.
	if !contains(fake.collabs, "hw1-ada:ada") {
		t.Error("grants should be re-asserted on a skipped repo")
	}
	out := buf.String()
	if !strings.Contains(out, "1 skipped") || !strings.Contains(out, "re-asserted push") {
		t.Errorf("skip summary/warning missing: %s", out)
	}
}

func TestAssignUnknownTeamMember(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "team-x: [student-999]\n")
	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "student-999") {
		t.Fatalf("unknown team member should be a hard error, got %v", err)
	}
}

func TestAssignOwnerGuard(t *testing.T) {
	fake := newFakeAssign("member")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
}

func TestAssignDryRun(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")
	o.dryRun = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.generated) != 0 {
		t.Error("dry-run must not generate repos")
	}
	out := buf.String()
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "hw1-ada") {
		t.Errorf("dry-run plan missing: %s", out)
	}
}
