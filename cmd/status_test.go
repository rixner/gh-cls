package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
)

// fakeStatusClient is a stand-in for the read-only status operations. repos is a
// flat list filtered by prefix, mirroring the real ListOrgReposByPrefix. The
// collaborators and feedback maps drive the --detail scan, keyed by repo name.
type fakeStatusClient struct {
	teamMissing   bool
	members       []string
	repos         []gh.Repo
	listErr       error
	collaborators map[string][]gh.Collaborator
	issueState    map[string]string // repo -> state; absent means not found
	prState       map[string]string
}

func (f *fakeStatusClient) GetTeam(context.Context, string, string) (*gh.Team, bool, error) {
	if f.teamMissing {
		return nil, false, nil
	}
	return &gh.Team{ID: 1}, true, nil
}

func (f *fakeStatusClient) ListTeamMembers(context.Context, string, string) ([]string, error) {
	return f.members, nil
}

func (f *fakeStatusClient) ListOrgReposByPrefix(_ context.Context, _, prefix string) ([]gh.Repo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []gh.Repo
	for _, r := range f.repos {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStatusClient) ListDirectCollaborators(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
	return f.collaborators[repo], nil
}

func (f *fakeStatusClient) FindIssueByTitle(_ context.Context, _, repo, _ string) (int, string, bool, error) {
	s, ok := f.issueState[repo]
	if !ok {
		return 0, "", false, nil
	}
	return 1, s, true, nil
}

func (f *fakeStatusClient) FindPRByBase(_ context.Context, _, repo, _ string) (int, string, bool, error) {
	s, ok := f.prState[repo]
	if !ok {
		return 0, "", false, nil
	}
	return 2, s, true, nil
}

// fixedClock is the deterministic timestamp the --detail auto filename uses in
// tests so the generated CSV name is predictable.
func fixedClock() time.Time { return time.Date(2026, 6, 29, 14, 12, 33, 0, time.UTC) }

func newStatusOpts(fake *fakeStatusClient) *statusOpts {
	return newStatusOptsG(assignGlobals(), fake)
}

func newStatusOptsG(g *globalOpts, fake *fakeStatusClient) *statusOpts {
	return &statusOpts{
		g:         g,
		now:       fixedClock,
		newClient: func(context.Context) (statusClient, error) { return fake, nil },
	}
}

func TestStatusWholeCourse(t *testing.T) {
	fake := &fakeStatusClient{
		members: []string{"ta1", "ta2", "ta3"},
		repos: []gh.Repo{
			{Name: "hw1-ada", Private: true},
			{Name: "hw1-alan", Private: true},
			{Name: "hw1-template", Private: true, IsTemplate: true}, // excluded
			{Name: "project-team-alpha", Private: true},
		},
	}
	var buf bytes.Buffer
	if err := newStatusOpts(fake).run(context.Background(), &buf, ""); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "Org: cs101-spring26") {
		t.Errorf("missing org header:\n%s", out)
	}
	if !strings.Contains(out, "Staff team: staff (3 members)") {
		t.Errorf("staff line wrong:\n%s", out)
	}
	// hw1 has 2 student repos (the template is excluded); project has 1.
	for _, want := range []string{"hw1", "individual", "project", "group"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	hw1Line := lineContaining(t, out, "hw1")
	if !strings.Contains(hw1Line, "2") {
		t.Errorf("hw1 should count 2 student repos (template excluded): %q", hw1Line)
	}
	// Assignments are reported in sorted order: hw1 before project.
	if strings.Index(out, "hw1") > strings.Index(out, "project") {
		t.Errorf("assignments should be sorted (hw1 before project):\n%s", out)
	}
}

func TestStatusSingleAssignment(t *testing.T) {
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos: []gh.Repo{
			{Name: "hw1-ada", Private: true},
			{Name: "project-team-alpha", Private: true},
		},
	}
	var buf bytes.Buffer
	if err := newStatusOpts(fake).run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "hw1") {
		t.Errorf("hw1 should be reported:\n%s", out)
	}
	if strings.Contains(out, "project") {
		t.Errorf("only hw1 was requested, project should not appear:\n%s", out)
	}
	// A single member is rendered without the plural "s".
	if !strings.Contains(out, "Staff team: staff (1 member)") {
		t.Errorf("a one-member team should read '1 member':\n%s", out)
	}
}

func TestStatusUnknownAssignment(t *testing.T) {
	fake := &fakeStatusClient{members: []string{"ta1"}}
	err := newStatusOpts(fake).run(context.Background(), &bytes.Buffer{}, "bogus")
	if err == nil || !strings.Contains(err.Error(), "not found in config") {
		t.Fatalf("an unknown assignment should error, got %v", err)
	}
}

func TestStatusVisibilityMismatch(t *testing.T) {
	// hw1's policy is private (assignGlobals sets no public flag); a public repo
	// is drift that must be flagged.
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos: []gh.Repo{
			{Name: "hw1-ada", Private: true},
			{Name: "hw1-alan", Private: false}, // public under a private policy
		},
	}
	var buf bytes.Buffer
	if err := newStatusOpts(fake).run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 public") || !strings.Contains(out, "[policy: private]") {
		t.Errorf("visibility drift should be flagged:\n%s", out)
	}
}

func TestStatusMissingStaffTeam(t *testing.T) {
	fake := &fakeStatusClient{teamMissing: true}
	var buf bytes.Buffer
	if err := newStatusOpts(fake).run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("a missing staff team should be reported:\n%s", buf.String())
	}
}

func TestStatusListError(t *testing.T) {
	fake := &fakeStatusClient{members: []string{"ta1"}, listErr: errors.New("boom")}
	var buf bytes.Buffer
	err := newStatusOpts(fake).run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a list failure should surface as an error, got %v", err)
	}
	if !strings.Contains(buf.String(), "FAILED hw1") {
		t.Errorf("the failed assignment should be named:\n%s", buf.String())
	}
}

func TestStatusDetail(t *testing.T) {
	// feedbackGlobals' hw1 is individual with an issue feedback artifact.
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos: []gh.Repo{
			{Name: "hw1-ada", Private: true},
			{Name: "hw1-bob", Private: true},
			{Name: "hw1-cy", Private: true},
			{Name: "hw1-template", Private: true, IsTemplate: true}, // excluded
		},
		collaborators: map[string][]gh.Collaborator{
			"hw1-ada": {collab("ada", "push")}, // writable
			"hw1-bob": {collab("bob", "pull")}, // frozen
			"hw1-cy":  {collab("cy", "push")},  // writable
		},
		issueState: map[string]string{
			"hw1-ada": "open",
			"hw1-bob": "closed",
			// hw1-cy absent -> missing
		},
	}
	csvPath := filepath.Join(t.TempDir(), "detail.csv")
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = csvPath

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"hw1 (individual)", "1 frozen", "2 writable", "feedback: 1 open, 1 closed, 1 missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}

	recs := readCSV(t, csvPath)
	wantHeader := []string{"assignment", "repo", "key", "visibility", "expected_visibility", "frozen", "feedback"}
	if len(recs) != 4 || !equalRow(recs[0], wantHeader) {
		t.Fatalf("CSV header/row count wrong: %v", recs)
	}
	bob := findRow(recs, "hw1-bob")
	if bob == nil {
		t.Fatalf("no hw1-bob row in CSV: %v", recs)
	}
	if want := []string{"hw1", "hw1-bob", "bob", "private", "private", "frozen", "closed"}; !equalRow(bob, want) {
		t.Errorf("hw1-bob row = %v, want %v", bob, want)
	}
}

func TestStatusDetailMixed(t *testing.T) {
	// One repo where a non-admin has push and another has only pull is a partial
	// freeze: it must read as "mixed" in the summary.
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos:   []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{
			"hw1-ada": {collab("ada", "push"), collab("ta", "pull")},
		},
		issueState: map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "m.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "1 mixed") {
		t.Errorf("a partial freeze should read as mixed:\n%s", buf.String())
	}
}

func TestStatusDetailNoFeedbackConfigured(t *testing.T) {
	// feedbackGlobals' hw0 has no feedback policy.
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw0-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw0-ada": {collab("ada", "push")}},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "f.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw0"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "feedback: not configured") {
		t.Errorf("an assignment with no feedback policy should say so:\n%s", buf.String())
	}
}

func TestStatusDetailNeverOverwritesExplicitOut(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "taken.csv")
	if err := os.WriteFile(existing, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStatusClient{members: []string{"ta1"}, repos: []gh.Repo{{Name: "hw1-ada", Private: true}}}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = existing
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("an existing --out must not be overwritten, got %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Errorf("the existing file was modified: %q", b)
	}
}

func TestStatusDetailAutoNameRollsOnCollision(t *testing.T) {
	t.Chdir(t.TempDir()) // auto CSV lands in the (temp) working directory

	// The fixed clock yields this base name; pre-create it so the run must roll.
	taken := "gh-cls-status-hw1-20260629-141233.csv"
	if err := os.WriteFile(taken, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw1-ada": {collab("ada", "push")}},
		issueState:    map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.detail = true // no --out: use the auto name

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	rolled := "gh-cls-status-hw1-20260629-141233-2.csv"
	if _, err := os.Stat(rolled); err != nil {
		t.Errorf("a name collision should roll to %s, but it is absent (%v)\noutput:\n%s", rolled, err, buf.String())
	}
	if b, _ := os.ReadFile(taken); string(b) != "sentinel" {
		t.Errorf("the pre-existing file must not be overwritten, got %q", b)
	}
	if !strings.Contains(buf.String(), rolled) {
		t.Errorf("output should name the file actually written (%s):\n%s", rolled, buf.String())
	}
}

// readCSV reads every record from a CSV file.
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// findRow returns the first CSV record whose repo column (index 1) is repo.
func findRow(recs [][]string, repo string) []string {
	for _, r := range recs {
		if len(r) > 1 && r[1] == repo {
			return r
		}
	}
	return nil
}

func equalRow(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lineContaining returns the first line of s that contains sub, failing if none.
func lineContaining(t *testing.T, s, sub string) string {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", sub, s)
	return ""
}
