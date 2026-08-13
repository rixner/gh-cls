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

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeStatusClient configures a ghtest.Fake for the read-only status
// operations. repos is a flat list filtered by prefix, mirroring the real
// ListOrgReposByPrefix. The collaborators and feedback maps drive the
// --detail scan, keyed by repo name.
type fakeStatusClient struct {
	teamMissing   bool
	members       []string
	repos         []gh.Repo
	listErr       error
	collaborators map[string][]gh.Collaborator
	invitations   map[string][]gh.Invitation
	frozen        map[string]freezeState // recorded freeze state per repo
	noProperty    bool                   // org never ran setup: no freeze record readable
	issueState    map[string]string      // repo -> state; absent means not found
	prState       map[string]string

	listCalls int // count of ListOrgReposByPrefix calls, to assert it lists at most once
}

func (s *fakeStatusClient) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.GetTeamFunc = func(context.Context, string, string) (*gh.Team, bool, error) {
		if s.teamMissing {
			return nil, false, nil
		}
		return &gh.Team{ID: 1}, true, nil
	}
	fk.ListTeamMembersFunc = func(context.Context, string, string) ([]string, error) {
		return s.members, nil
	}
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		s.listCalls++
		if s.listErr != nil {
			return nil, s.listErr
		}
		var out []gh.Repo
		for _, r := range s.repos {
			if strings.HasPrefix(r.Name, prefix) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	fk.ListDirectCollaboratorsFunc = func(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
		return s.collaborators[repo], nil
	}
	fk.ListRepoInvitationsFunc = func(_ context.Context, _, repo string) ([]gh.Invitation, error) {
		return s.invitations[repo], nil
	}
	fk.GetPropertyDefinitionFunc = func(_ context.Context, _, name string) (*gh.PropertyDefinition, bool, error) {
		if s.noProperty {
			return nil, false, nil
		}
		return &gh.PropertyDefinition{
			PropertyName:     name,
			ValueType:        gh.PropertyTypeTrueFalse,
			ValuesEditableBy: gh.PropertyEditableByOrg,
		}, true, nil
	}
	fk.ListRepoPropertyValuesFunc = func(context.Context, string) (map[string]map[string]string, error) {
		out := make(map[string]map[string]string, len(s.frozen))
		for repo, state := range s.frozen {
			out[repo] = map[string]string{frozenProperty: string(state)}
		}
		return out, nil
	}
	fk.FindIssueByTitleFunc = func(_ context.Context, _, repo, _ string) (int, string, bool, error) {
		state, ok := s.issueState[repo]
		if !ok {
			return 0, "", false, nil
		}
		return 1, state, true, nil
	}
	fk.FindPRByBaseFunc = func(_ context.Context, _, repo, _ string) (int, string, bool, error) {
		state, ok := s.prState[repo]
		if !ok {
			return 0, "", false, nil
		}
		return 2, state, true, nil
	}
	return fk
}

// fixedClock is the deterministic timestamp the --detail auto filename uses in
// tests so the generated CSV name is predictable.
func fixedClock() time.Time { return time.Date(2026, 6, 29, 14, 12, 33, 0, time.UTC) }

func newStatusOpts(fake *fakeStatusClient) *statusOpts {
	return newStatusOptsG(assignGlobals(), fake)
}

func newStatusOptsG(g *globalOpts, fake *fakeStatusClient) *statusOpts {
	fk := fake.fake()
	return &statusOpts{
		g:         g,
		now:       fixedClock,
		newClient: func(context.Context) (statusClient, error) { return fk, nil },
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
	// A whole-course run must list the org once and partition locally, not
	// once per assignment.
	if fake.listCalls != 1 {
		t.Errorf("whole-course status should list the org exactly once, got %d calls", fake.listCalls)
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

func TestStatusWholeCourseDetailListsOrgOnce(t *testing.T) {
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos: []gh.Repo{
			{Name: "hw1-ada", Private: true},
			{Name: "project-team-alpha", Private: true},
		},
		collaborators: map[string][]gh.Collaborator{
			"hw1-ada":            {collab("ada", "push")},
			"project-team-alpha": {collab("alpha", "push")},
		},
	}
	o := newStatusOptsG(assignGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "whole.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, ""); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if fake.listCalls != 1 {
		t.Errorf("whole-course --detail should list the org exactly once, got %d calls", fake.listCalls)
	}
}

func TestStatusExcludesLongerOverlappingAssignmentRepos(t *testing.T) {
	// "proj" and "proj-final" both configured, with proj-final-y also matching
	// proj's "proj-" prefix. Partitioning the single whole-course listing
	// locally must still exclude it from "proj", as filterAssignmentRepos did
	// when each assignment listed separately.
	g := &globalOpts{
		org: "cs101-spring26",
		cfg: &config.Config{
			StaffTeam: "staff",
			Assignments: map[string]config.Assignment{
				"proj":       {Type: config.TypeIndividual},
				"proj-final": {Type: config.TypeIndividual},
			},
		},
		staffTeam:   "staff",
		concurrency: 4,
	}
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos:   []gh.Repo{{Name: "proj-x", Private: true}, {Name: "proj-final-y", Private: true}},
	}
	o := newStatusOptsG(g, fake)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, ""); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if fake.listCalls != 1 {
		t.Errorf("should still list the org exactly once, got %d calls", fake.listCalls)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[0] {
		case "proj":
			if fields[2] != "1" {
				t.Errorf("proj should count 1 repo (proj-final's repo excluded): %q", line)
			}
		case "proj-final":
			if fields[2] != "1" {
				t.Errorf("proj-final should count 1 repo (proj's repo excluded): %q", line)
			}
		}
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

func TestStatusRepoCountIsNotRepeated(t *testing.T) {
	// The count and the visibility share one column. When every repo has the same
	// visibility the total must appear exactly once on the line, not as a bare
	// count and again inside the breakdown ("2   2 private").
	fake := &fakeStatusClient{
		members: []string{"ta1"},
		repos:   []gh.Repo{{Name: "hw1-ada", Private: true}, {Name: "hw1-alan", Private: true}},
	}
	var buf bytes.Buffer
	if err := newStatusOpts(fake).run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "VISIBILITY") {
		t.Errorf("the visibility column should be folded into REPOS:\n%s", out)
	}
	line := lineContaining(t, out, "hw1")
	if got := strings.Count(line, "2"); got != 1 {
		t.Errorf("the repo count should appear once on the line, got %d in %q", got, line)
	}
	if !strings.Contains(line, "2 private") {
		t.Errorf("the line should read as a count with its visibility: %q", line)
	}
}

func TestStatusDetailCountsWriteInvitationAsUnfrozen(t *testing.T) {
	// The same loophole freeze closes: hw1-ada's only collaborator is read-only,
	// but a pending invitation still confers write, so accepting it would reopen
	// the repo. Reporting that as "frozen" would tell the instructor the deadline
	// held when it did not.
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw1-ada": {collab("ada", "pull")}},
		invitations:   map[string][]gh.Invitation{"hw1-ada": {invite(1, "alan", gh.InvitationWrite)}},
		issueState:    map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "inv.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "1 mixed") {
		t.Errorf("a read collaborator plus a write invitation is a partial freeze:\n%s", buf.String())
	}
}

func TestStatusDetailIgnoresSettledInvitations(t *testing.T) {
	// An expired invitation cannot be accepted and a read invitation confers no
	// write, so neither should keep a genuinely frozen repo from reading as frozen.
	expired := invite(1, "alan", gh.InvitationWrite)
	expired.Expired = true
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw1-ada": {collab("ada", "pull")}},
		invitations:   map[string][]gh.Invitation{"hw1-ada": {expired, invite(2, "bob", gh.InvitationRead)}},
		issueState:    map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "settled.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "1 frozen") {
		t.Errorf("expired and read invitations should not unfreeze the repo:\n%s", buf.String())
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
	wantHeader := []string{"assignment", "repo", "key", "visibility", "expected_visibility", "frozen", "recorded", "feedback"}
	if len(recs) != 4 || !equalRow(recs[0], wantHeader) {
		t.Fatalf("CSV header/row count wrong: %v", recs)
	}
	bob := findRow(recs, "hw1-bob")
	if bob == nil {
		t.Fatalf("no hw1-bob row in CSV: %v", recs)
	}
	// bob's access is read-only but this fake records no freeze state, so the row
	// shows the access and the record disagreeing in the benign direction: nothing
	// was recorded, which is what a repo frozen before the record existed looks like.
	if want := []string{"hw1", "hw1-bob", "bob", "private", "private", "frozen", "not recorded", "closed"}; !equalRow(bob, want) {
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

func TestStatusDetailFlagsDriftFromTheRecord(t *testing.T) {
	// The check the record makes possible: hw1-ada is recorded frozen but its
	// student still has push, so the freeze did not fully take. Without the record
	// this is invisible, since a writable repo looks like any unfrozen one.
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}, {Name: "hw1-bob", Private: true}},
		collaborators: map[string][]gh.Collaborator{
			"hw1-ada": {collab("ada", "push")}, // contradicts the record
			"hw1-bob": {collab("bob", "pull")}, // agrees with it
		},
		frozen:     map[string]freezeState{"hw1-ada": freezeFrozen, "hw1-bob": freezeFrozen},
		issueState: map[string]string{"hw1-ada": "open", "hw1-bob": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "drift.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "DRIFT hw1-ada") {
		t.Errorf("a repo recorded frozen but still writable is drift:\n%s", out)
	}
	if strings.Contains(out, "DRIFT hw1-bob") {
		t.Errorf("a repo whose access matches its record is not drift:\n%s", out)
	}
}

func TestStatusDetailFlagsAThawedRepoThatIsStillFrozen(t *testing.T) {
	// The other direction: an extension was recorded but the access was never
	// restored, so the student cannot work during their extension.
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw1-ada": {collab("ada", "pull")}},
		frozen:        map[string]freezeState{"hw1-ada": freezeThawed},
		issueState:    map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "thaw.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "DRIFT hw1-ada") || !strings.Contains(out, "--undo") {
		t.Errorf("a repo recorded thawed but still frozen should be flagged with the fix:\n%s", out)
	}
}

func TestStatusStillWorksWithoutTheFreezeProperty(t *testing.T) {
	// status reads only and must stay usable on an org that never ran setup: it
	// reports every repo as not recorded and says why, rather than failing.
	fake := &fakeStatusClient{
		members:       []string{"ta1"},
		repos:         []gh.Repo{{Name: "hw1-ada", Private: true}},
		collaborators: map[string][]gh.Collaborator{"hw1-ada": {collab("ada", "pull")}},
		noProperty:    true,
		issueState:    map[string]string{"hw1-ada": "open"},
	}
	o := newStatusOptsG(feedbackGlobals(), fake)
	o.out = filepath.Join(t.TempDir(), "noprop.csv")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("a missing freeze property must not fail a read-only status: %v", err)
	}
	if !strings.Contains(buf.String(), "no freeze record is readable") {
		t.Errorf("the blank record column should be explained:\n%s", buf.String())
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
