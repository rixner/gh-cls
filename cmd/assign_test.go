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
	teamID    int64
	hasIssues bool
	exists    map[string]bool
	branches  []gh.BranchCount
	generated []string
	collabs   []string
	teamRepos []string
	rulesets  map[string]int64 // repo -> staff team ID used in bypass
	refs      []string         // "repo:ref"
	prs       []string         // "repo:head->base"
	issues    []string         // repo
	enabled   []string         // repos where issues were enabled
}

func (f *fakeAssignClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeAssignClient) GetRepo(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists[owner+"/"+name] {
		return &gh.Repo{Name: name, DefaultBranch: "main", HasIssues: f.hasIssues}, true, nil
	}
	return nil, false, nil
}

func (f *fakeAssignClient) GetTeam(context.Context, string, string) (*gh.Team, bool, error) {
	return &gh.Team{Slug: "staff", ID: f.teamID}, true, nil
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

func (f *fakeAssignClient) ApplyRuleset(_ context.Context, _, repo string, staffTeamID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rulesets == nil {
		f.rulesets = map[string]int64{}
	}
	f.rulesets[repo] = staffTeamID
	return nil
}

func (f *fakeAssignClient) GetRef(_ context.Context, _, _, _ string) (string, error) {
	return "starter-sha", nil
}

func (f *fakeAssignClient) CreateRef(_ context.Context, _, repo, ref, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, repo+":"+ref)
	return nil
}

func (f *fakeAssignClient) BranchExists(_ context.Context, _, repo, branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return contains(f.refs, repo+":refs/heads/"+branch), nil
}

func (f *fakeAssignClient) CreatePR(_ context.Context, _, repo, _, head, base, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, repo+":"+head+"->"+base)
	return nil
}

func (f *fakeAssignClient) PRExists(_ context.Context, _, repo, base string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if strings.HasPrefix(p, repo+":") && strings.HasSuffix(p, "->"+base) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAssignClient) EnableIssues(_ context.Context, _, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = append(f.enabled, repo)
	return nil
}

func (f *fakeAssignClient) CreateIssue(_ context.Context, _, repo, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = append(f.issues, repo)
	return nil
}

func (f *fakeAssignClient) IssueExists(_ context.Context, _, repo, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return contains(f.issues, repo), nil
}

func newFakeAssign(role string) *fakeAssignClient {
	return &fakeAssignClient{
		role:     role,
		teamID:   42,
		exists:   map[string]bool{"cs101-spring26/hw1-template": true, "cs101-spring26/project-template": true},
		branches: []gh.BranchCount{{Name: "main", Commits: 1}},
	}
}

func boolp(b bool) *bool    { return &b }
func strp(s string) *string { return &s }

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
	return count(haystack, needle) > 0
}

func count(haystack []string, needle string) int {
	n := 0
	for _, h := range haystack {
		if h == needle {
			n++
		}
	}
	return n
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

func TestAssignBranchProtection(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{BranchProtection: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	if id, ok := fake.rulesets["hw1-ada"]; !ok || id != 42 {
		t.Errorf("ruleset not applied with resolved staff team ID: %v", fake.rulesets)
	}
	if len(fake.rulesets) != 3 {
		t.Errorf("expected a ruleset on each of 3 repos, got %d", len(fake.rulesets))
	}
}

func TestAssignFeedbackPR(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.refs, "hw1-ada:refs/heads/feedback") {
		t.Errorf("feedback branch not created: %v", fake.refs)
	}
	if !contains(fake.prs, "hw1-ada:main->feedback") {
		t.Errorf("feedback PR not opened with base feedback: %v", fake.prs)
	}
	if len(fake.issues) != 0 {
		t.Error("pr mode should not open issues")
	}
}

func TestAssignFeedbackIssueEnablesWhenNeeded(t *testing.T) {
	// Template has issues disabled: assign must enable them first.
	fake := newFakeAssign("admin")
	fake.hasIssues = false
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.enabled, "hw1-ada") {
		t.Errorf("issues should be enabled when the template has them off: %v", fake.enabled)
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Errorf("feedback issue not opened: %v", fake.issues)
	}
	if len(fake.prs) != 0 {
		t.Error("issue mode should not open PRs")
	}
}

func TestAssignFeedbackIssueSkipsEnableWhenOn(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.hasIssues = true
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if len(fake.enabled) != 0 {
		t.Errorf("issues already on: should not re-enable, got %v", fake.enabled)
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Error("feedback issue should still be opened")
	}
}

func TestAssignProtectionNewOnlyFeedbackReconciled(t *testing.T) {
	// An existing repo is reused: branch protection is not re-applied, but the
	// feedback artifact is reconciled (a missing one is created).
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	o := newAssignOpts(t, fake, assignRoster, "")

	ov := config.Overrides{BranchProtection: boolp(true), Feedback: strp("issue")}
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", ov); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.rulesets["hw1-ada"]; ok {
		t.Error("ruleset should not be applied to a reused repo")
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Error("feedback should be reconciled on a reused repo that lacks it")
	}
	// Brand-new repos in the same run get both protection and feedback.
	if _, ok := fake.rulesets["hw1-alan"]; !ok {
		t.Error("new repos should still get protection")
	}
	if !contains(fake.issues, "hw1-alan") {
		t.Error("new repos should get feedback")
	}
}

func TestAssignFeedbackPRIdempotent(t *testing.T) {
	// A reused repo already has its feedback branch and PR: neither is recreated,
	// so a closed PR is never reopened.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/feedback"}
	fake.prs = []string{"hw1-ada:main->feedback"}
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.refs, "hw1-ada:refs/heads/feedback") != 1 {
		t.Errorf("existing feedback branch should not be recreated: %v", fake.refs)
	}
	if count(fake.prs, "hw1-ada:main->feedback") != 1 {
		t.Errorf("existing feedback PR should not be reopened: %v", fake.prs)
	}
}

func TestAssignFeedbackPRRecoversMissingPR(t *testing.T) {
	// A prior run created the feedback branch but failed before opening the PR;
	// the re-run opens only the missing PR.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/feedback"} // branch present, no PR
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.refs, "hw1-ada:refs/heads/feedback") != 1 {
		t.Errorf("branch should not be recreated: %v", fake.refs)
	}
	if !contains(fake.prs, "hw1-ada:main->feedback") {
		t.Errorf("missing feedback PR should be opened on re-run: %v", fake.prs)
	}
}

func TestAssignFeedbackIssueIdempotent(t *testing.T) {
	// A reused repo already has its feedback issue: no duplicate is opened.
	fake := newFakeAssign("admin")
	fake.hasIssues = true
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.issues = []string{"hw1-ada"}
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.issues, "hw1-ada") != 1 {
		t.Errorf("existing feedback issue should not be duplicated: %v", fake.issues)
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
