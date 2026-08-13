package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// feedbackGlobals is the loaded-config state the feedback tests run against: an
// individual assignment with an issue artifact, a group one with a PR artifact,
// and one with no feedback configured.
func feedbackGlobals() *globalOpts {
	cfg := &config.Config{
		Org:       "cs101-spring26",
		StaffTeam: "staff",
		Assignments: map[string]config.Assignment{
			"hw1":  {Type: config.TypeIndividual, Template: "hw1-template", Feedback: config.FeedbackIssue},
			"proj": {Type: config.TypeGroup, Template: "proj-template", Feedback: config.FeedbackPR},
			"hw0":  {Type: config.TypeIndividual, Template: "hw0-template"}, // no feedback
		},
	}
	return &globalOpts{cfg: cfg, org: cfg.Org, staffTeam: cfg.StaffTeam, concurrency: 4}
}

const fbRosterSolo = "identifier,username\nstudent-001,ada\n"

// fakeFeedbackState configures a ghtest.Fake for feedback tests and captures
// what it observed. Each repo carries a feedback issue and PR number and a
// list of comment bodies.
type fakeFeedbackState struct {
	role     string
	repos    map[string]bool     // repo name -> exists
	issueNum map[string]int      // repo -> feedback issue number (absent = none)
	prNum    map[string]int      // repo -> feedback PR number (absent = none)
	comments map[string][]string // repo -> comment bodies
	posts    []string            // repo names AddComment was called on, for assertions
}

func newFakeFeedback(role string, repos ...string) *fakeFeedbackState {
	s := &fakeFeedbackState{
		role:     role,
		repos:    map[string]bool{},
		issueNum: map[string]int{},
		prNum:    map[string]int{},
		comments: map[string][]string{},
	}
	for i, r := range repos {
		s.repos[r] = true
		s.issueNum[r] = i + 1
		s.prNum[r] = 100 + i
	}
	return s
}

func (s *fakeFeedbackState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.GetRepoFunc = func(_ context.Context, _, name string) (*gh.Repo, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.repos[name] {
			return &gh.Repo{Name: name, DefaultBranch: "main"}, true, nil
		}
		return nil, false, nil
	}
	fk.FindIssueByTitleFunc = func(_ context.Context, _, repo, _ string) (int, string, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		n, ok := s.issueNum[repo]
		return n, "open", ok, nil
	}
	fk.FindPRByBaseFunc = func(_ context.Context, _, repo, _ string) (int, string, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		n, ok := s.prNum[repo]
		return n, "open", ok, nil
	}
	fk.ListIssueCommentsFunc = func(_ context.Context, _, repo string, _ int) ([]gh.Comment, error) {
		fk.Lock()
		defer fk.Unlock()
		var out []gh.Comment
		for _, b := range s.comments[repo] {
			out = append(out, gh.Comment{Body: b})
		}
		return out, nil
	}
	fk.AddCommentFunc = func(_ context.Context, _, repo string, _ int, body string) (string, error) {
		fk.Lock()
		defer fk.Unlock()
		s.comments[repo] = append(s.comments[repo], body)
		s.posts = append(s.posts, repo)
		return "https://github.com/cs101-spring26/" + repo + "#issuecomment-1", nil
	}
	return fk
}

// newFeedbackOpts wires feedbackOpts to a fake; the feedback dir, roster, and
// (optional) groups files live in a temp dir. It returns the opts and the dir so
// a test can rewrite a file to simulate a re-grade.
func newFeedbackOpts(t *testing.T, fake *fakeFeedbackState, files map[string]string, rosterCSV, groupsYML string) (*feedbackOpts, string) {
	t.Helper()
	base := t.TempDir()
	fbdir := filepath.Join(base, "fb")
	if err := os.Mkdir(fbdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(fbdir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rosterPath := filepath.Join(base, "roster.csv")
	if err := os.WriteFile(rosterPath, []byte(rosterCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	groupsPath := ""
	if groupsYML != "" {
		groupsPath = filepath.Join(base, "groups.yml")
		if err := os.WriteFile(groupsPath, []byte(groupsYML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fk := fake.fake()
	return &feedbackOpts{
		g:         feedbackGlobals(),
		dir:       fbdir,
		roster:    rosterPath,
		groups:    groupsPath,
		newClient: func(context.Context) (feedbackClient, error) { return fk, nil },
	}, fbdir
}

func TestFeedbackPostsToEveryUnit(t *testing.T) {
	fake := newFakeFeedback("admin", "hw1-ada", "hw1-alan", "hw1-grace")
	files := map[string]string{"ada.md": "nice work ada", "alan.md": "see me", "grace.txt": "well done"}
	o, _ := newFeedbackOpts(t, fake, files, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for repo, body := range map[string]string{"hw1-ada": "nice work ada", "hw1-alan": "see me", "hw1-grace": "well done"} {
		if len(fake.comments[repo]) != 1 {
			t.Fatalf("%s got %d comments, want 1", repo, len(fake.comments[repo]))
		}
		c := fake.comments[repo][0]
		if !strings.Contains(c, body) {
			t.Errorf("%s comment missing body: %q", repo, c)
		}
		if !strings.Contains(c, feedbackMarkerPrefix) {
			t.Errorf("%s comment missing idempotency marker: %q", repo, c)
		}
	}
	if !strings.Contains(buf.String(), "3 posted, 0 up-to-date, 0 failed") {
		t.Errorf("summary wrong:\n%s", buf.String())
	}
}

func TestFeedbackGroupMode(t *testing.T) {
	fake := newFakeFeedback("admin", "proj-group-alpha", "proj-group-beta")
	files := map[string]string{"group-alpha.md": "group a feedback", "group-beta.md": "group b feedback"}

	t.Run("posts to each group's PR", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, files, assignRoster, assignGroups)
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "proj"); err != nil {
			t.Fatalf("run: %v\n%s", err, buf.String())
		}
		if len(fake.comments["proj-group-alpha"]) != 1 || len(fake.comments["proj-group-beta"]) != 1 {
			t.Errorf("each group PR should get one comment, got %v", fake.comments)
		}
	})

	t.Run("group assignment requires --groups", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, files, assignRoster, "")
		o.groups = ""
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "proj"); err == nil || !strings.Contains(err.Error(), "--groups is required") {
			t.Fatalf("want a --groups error, got %v", err)
		}
	})
}

func TestFeedbackWarnsOnMultiGroupStudent(t *testing.T) {
	// Regression: feedback used to print the unassigned-student warning but
	// silently drop the multi-group one, unlike audit. loadUnits/printUnitWarnings
	// now share the same warning logic, so feedback reports it too.
	fake := newFakeFeedback("admin", "proj-group-alpha", "proj-group-beta")
	files := map[string]string{"group-alpha.md": "group a feedback", "group-beta.md": "group b feedback"}
	groupsMultiGroup := "group-alpha: [student-001, student-003]\ngroup-beta: [student-002, student-001]\n"
	o, _ := newFeedbackOpts(t, fake, files, assignRoster, groupsMultiGroup)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "proj"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "student-001 is in more than one group: group-alpha, group-beta") {
		t.Errorf("multi-group warning missing:\n%s", buf.String())
	}
}

func TestFeedbackIdempotent(t *testing.T) {
	fake := newFakeFeedback("admin", "hw1-ada")
	o, dir := newFeedbackOpts(t, fake, map[string]string{"ada.md": "first round"}, fbRosterSolo, "")

	// First run posts.
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run 1: %v\n%s", err, buf.String())
	}
	if len(fake.comments["hw1-ada"]) != 1 {
		t.Fatalf("run 1 should post one comment, got %d", len(fake.comments["hw1-ada"]))
	}

	// Re-run unchanged: the marker is already present, so nothing is posted.
	buf.Reset()
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run 2: %v\n%s", err, buf.String())
	}
	if len(fake.comments["hw1-ada"]) != 1 {
		t.Errorf("re-run must not repost identical feedback, got %d comments", len(fake.comments["hw1-ada"]))
	}
	if !strings.Contains(buf.String(), "0 posted, 1 up-to-date, 0 failed") {
		t.Errorf("re-run summary wrong:\n%s", buf.String())
	}

	// Edit the file (a re-grade): the new hash mismatches, so a fresh comment posts.
	if err := os.WriteFile(filepath.Join(dir, "ada.md"), []byte("second round"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run 3: %v\n%s", err, buf.String())
	}
	if len(fake.comments["hw1-ada"]) != 2 {
		t.Errorf("edited feedback should post a new comment, got %d comments", len(fake.comments["hw1-ada"]))
	}
}

func TestFeedbackNoFeedbackConfigured(t *testing.T) {
	fake := newFakeFeedback("admin", "hw0-ada")
	o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, fbRosterSolo, "")
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw0")
	if err == nil || !strings.Contains(err.Error(), "no feedback artifact") {
		t.Fatalf("an assignment with no feedback should abort, got %v", err)
	}
	if len(fake.posts) != 0 {
		t.Error("nothing should be posted when feedback is not configured")
	}
}

func TestFeedbackMissingArtifact(t *testing.T) {
	// The repo exists but has no feedback issue (assign never created it).
	fake := newFakeFeedback("admin", "hw1-ada")
	delete(fake.issueNum, "hw1-ada")
	o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, fbRosterSolo, "")
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(buf.String(), "no feedback issue") {
		t.Fatalf("a missing feedback issue should fail that repo, got %v\n%s", err, buf.String())
	}
}

func TestFeedbackMissingRepo(t *testing.T) {
	// No repos exist at all; the unit's repo is reported not found.
	fake := newFakeFeedback("admin")
	o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, fbRosterSolo, "")
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(buf.String(), "not found") {
		t.Fatalf("a missing repo should be reported not found, got err=%v\n%s", err, buf.String())
	}
}

func TestFeedbackCompletenessMissingFile(t *testing.T) {
	// Roster has ada and alan; only ada has a feedback file.
	roster := "identifier,username\nstudent-001,ada\nstudent-002,alan\n"

	t.Run("aborts without --force, naming the gap", func(t *testing.T) {
		fake := newFakeFeedback("admin", "hw1-ada", "hw1-alan")
		o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, roster, "")
		var buf bytes.Buffer
		err := o.run(context.Background(), &buf, "hw1")
		if err == nil {
			t.Fatal("incomplete coverage should abort without --force")
		}
		if !strings.Contains(buf.String(), "alan") {
			t.Errorf("the missing student should be named:\n%s", buf.String())
		}
		if len(fake.posts) != 0 {
			t.Error("nothing should be posted on an aborted run")
		}
	})

	t.Run("--force posts the matching subset and reports the skip", func(t *testing.T) {
		fake := newFakeFeedback("admin", "hw1-ada", "hw1-alan")
		o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, roster, "")
		o.force = true
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "hw1"); err != nil {
			t.Fatalf("--force should proceed, got %v\n%s", err, buf.String())
		}
		if len(fake.posts) != 1 || fake.posts[0] != "hw1-ada" {
			t.Errorf("only ada should be posted, got %v", fake.posts)
		}
		if !strings.Contains(buf.String(), "1 posted") || !strings.Contains(buf.String(), "alan") {
			t.Errorf("report should post one and still name the skipped student:\n%s", buf.String())
		}
	})
}

func TestFeedbackUnmatchedFile(t *testing.T) {
	// ada matches; bogus matches no student.
	fake := newFakeFeedback("admin", "hw1-ada")
	files := map[string]string{"ada.md": "x", "bogus.md": "y"}

	t.Run("aborts without --force, naming the file", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, files, fbRosterSolo, "")
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "hw1"); err == nil {
			t.Fatal("an unmatched file should abort without --force")
		}
		if !strings.Contains(buf.String(), "bogus.md") {
			t.Errorf("the unmatched file should be named:\n%s", buf.String())
		}
	})

	t.Run("--force posts the matched file, skips the unmatched one", func(t *testing.T) {
		fake := newFakeFeedback("admin", "hw1-ada")
		o, _ := newFeedbackOpts(t, fake, files, fbRosterSolo, "")
		o.force = true
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "hw1"); err != nil {
			t.Fatalf("--force should proceed, got %v", err)
		}
		if len(fake.posts) != 1 || fake.posts[0] != "hw1-ada" {
			t.Errorf("only the matched file should post, got %v", fake.posts)
		}
	})
}

func TestFeedbackFileValidation(t *testing.T) {
	fake := newFakeFeedback("admin", "hw1-ada")

	t.Run("whitespace-only file is rejected", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "   \n\t"}, fbRosterSolo, "")
		if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("a blank file should be rejected, got %v", err)
		}
	})

	t.Run("two files for the same key are rejected", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x", "ada.txt": "y"}, fbRosterSolo, "")
		if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "same student/group") {
			t.Fatalf("a duplicate key should be rejected, got %v", err)
		}
	})

	t.Run("non-.md/.txt files are ignored", func(t *testing.T) {
		fake := newFakeFeedback("admin", "hw1-ada")
		o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x", "notes.pdf": "binary"}, fbRosterSolo, "")
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "hw1"); err != nil {
			t.Fatalf("an ignored file should not break the run, got %v\n%s", err, buf.String())
		}
		if len(fake.posts) != 1 {
			t.Errorf("only the .md file should post, got %v", fake.posts)
		}
		if !strings.Contains(buf.String(), "notes.pdf") {
			t.Errorf("an ignored file should be reported:\n%s", buf.String())
		}
	})
}

func TestFeedbackDryRun(t *testing.T) {
	fake := newFakeFeedback("admin", "hw1-ada", "hw1-alan", "hw1-grace")
	files := map[string]string{"ada.md": "x", "alan.md": "y", "grace.md": "z"}
	o, _ := newFeedbackOpts(t, fake, files, assignRoster, "")
	o.dryRun = true
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("dry-run: %v\n%s", err, buf.String())
	}
	if len(fake.posts) != 0 {
		t.Errorf("dry-run must not post, got %v", fake.posts)
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output missing banner:\n%s", buf.String())
	}
}

func TestFeedbackRequiresOwner(t *testing.T) {
	fake := newFakeFeedback("member", "hw1-ada")
	o, _ := newFeedbackOpts(t, fake, map[string]string{"ada.md": "x"}, fbRosterSolo, "")
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("a non-owner should be rejected, got %v", err)
	}
	if len(fake.posts) != 0 {
		t.Error("nothing should be posted when the owner check fails")
	}
}
