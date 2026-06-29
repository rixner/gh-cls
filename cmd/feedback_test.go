package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
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

// fakeFeedbackClient is a concurrency-safe stand-in for the feedback operations.
// Each repo carries a feedback issue and PR number and a list of comment bodies.
type fakeFeedbackClient struct {
	mu       sync.Mutex
	role     string
	repos    map[string]bool     // repo name -> exists
	issueNum map[string]int      // repo -> feedback issue number (absent = none)
	prNum    map[string]int      // repo -> feedback PR number (absent = none)
	comments map[string][]string // repo -> comment bodies
	posts    []string            // repo names AddComment was called on, for assertions
}

func newFakeFeedback(role string, repos ...string) *fakeFeedbackClient {
	f := &fakeFeedbackClient{
		role:     role,
		repos:    map[string]bool{},
		issueNum: map[string]int{},
		prNum:    map[string]int{},
		comments: map[string][]string{},
	}
	for i, r := range repos {
		f.repos[r] = true
		f.issueNum[r] = i + 1
		f.prNum[r] = 100 + i
	}
	return f
}

func (f *fakeFeedbackClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeFeedbackClient) GetRepo(_ context.Context, _, name string) (*gh.Repo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repos[name] {
		return &gh.Repo{Name: name, DefaultBranch: "main"}, true, nil
	}
	return nil, false, nil
}

func (f *fakeFeedbackClient) FindIssueByTitle(_ context.Context, _, repo, _ string) (int, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.issueNum[repo]
	return n, "open", ok, nil
}

func (f *fakeFeedbackClient) FindPRByBase(_ context.Context, _, repo, _ string) (int, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.prNum[repo]
	return n, "open", ok, nil
}

func (f *fakeFeedbackClient) ListIssueComments(_ context.Context, _, repo string, _ int) ([]gh.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gh.Comment
	for _, b := range f.comments[repo] {
		out = append(out, gh.Comment{Body: b})
	}
	return out, nil
}

func (f *fakeFeedbackClient) AddComment(_ context.Context, _, repo string, _ int, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[repo] = append(f.comments[repo], body)
	f.posts = append(f.posts, repo)
	return "https://github.com/cs101-spring26/" + repo + "#issuecomment-1", nil
}

// newFeedbackOpts wires feedbackOpts to a fake; the feedback dir, roster, and
// (optional) teams files live in a temp dir. It returns the opts and the dir so
// a test can rewrite a file to simulate a re-grade.
func newFeedbackOpts(t *testing.T, fake *fakeFeedbackClient, files map[string]string, rosterCSV, teamsYML string) (*feedbackOpts, string) {
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
	teamsPath := ""
	if teamsYML != "" {
		teamsPath = filepath.Join(base, "teams.yml")
		if err := os.WriteFile(teamsPath, []byte(teamsYML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &feedbackOpts{
		g:         feedbackGlobals(),
		dir:       fbdir,
		roster:    rosterPath,
		teams:     teamsPath,
		newClient: func(context.Context) (feedbackClient, error) { return fake, nil },
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
	fake := newFakeFeedback("admin", "proj-team-alpha", "proj-team-beta")
	files := map[string]string{"team-alpha.md": "team a feedback", "team-beta.md": "team b feedback"}

	t.Run("posts to each team's PR", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, files, assignRoster, assignTeams)
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "proj"); err != nil {
			t.Fatalf("run: %v\n%s", err, buf.String())
		}
		if len(fake.comments["proj-team-alpha"]) != 1 || len(fake.comments["proj-team-beta"]) != 1 {
			t.Errorf("each team PR should get one comment, got %v", fake.comments)
		}
	})

	t.Run("group assignment requires --teams", func(t *testing.T) {
		o, _ := newFeedbackOpts(t, fake, files, assignRoster, "")
		o.teams = ""
		var buf bytes.Buffer
		if err := o.run(context.Background(), &buf, "proj"); err == nil || !strings.Contains(err.Error(), "--teams is required") {
			t.Fatalf("want a --teams error, got %v", err)
		}
	})
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
		if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "same student/team") {
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
