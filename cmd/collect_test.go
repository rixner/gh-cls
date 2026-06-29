package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
)

// fakeCollectClient returns a preset repo list, filtered by prefix.
type fakeCollectClient struct{ repos []gh.Repo }

func (f fakeCollectClient) ListOrgReposByPrefix(_ context.Context, _, prefix string) ([]gh.Repo, error) {
	var out []gh.Repo
	for _, r := range f.repos {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeClone is the in-memory state of one cloned repo.
type fakeClone struct {
	sha       string
	fetchHead string
	clean     bool
	tags      map[string]bool
}

// fakeGit is a concurrency-safe stand-in for the git/gh operations.
type fakeGit struct {
	mu        sync.Mutex
	clones    map[string]*fakeClone
	forced    map[string]bool   // dir -> next fetch reports a forced update
	remoteTip map[string]string // dir -> sha a fetch moves FETCH_HEAD to
	cloneErr  map[string]error  // repo -> error returned by Clone
	cloned    []string          // dirs cloned, for asserting dry-run did nothing
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		clones:    map[string]*fakeClone{},
		forced:    map[string]bool{},
		remoteTip: map[string]string{},
		cloneErr:  map[string]error{},
	}
}

// seed registers an existing clone at dir.
func (f *fakeGit) seed(dir, sha string, clean bool, tags ...string) {
	c := &fakeClone{sha: sha, clean: clean, tags: map[string]bool{}}
	for _, t := range tags {
		c.tags[t] = true
	}
	f.clones[dir] = c
}

func (f *fakeGit) CloneExists(dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir] != nil
}

func (f *fakeGit) Clone(_ context.Context, _, repo, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.cloneErr[repo]; e != nil {
		return e
	}
	f.clones[dir] = &fakeClone{sha: "sha-" + repo, clean: true, tags: map[string]bool{}}
	f.cloned = append(f.cloned, dir)
	return nil
}

func (f *fakeGit) WorktreeClean(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].clean, nil
}

func (f *fakeGit) Head(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].sha, nil
}

func (f *fakeGit) TagExists(_ context.Context, dir, tag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].tags[tag], nil
}

func (f *fakeGit) Fetch(_ context.Context, dir, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if tip, ok := f.remoteTip[dir]; ok {
		c.fetchHead = tip
	} else {
		c.fetchHead = ref
	}
	return f.forced[dir], nil
}

func (f *fakeGit) Checkout(_ context.Context, dir, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if ref == "FETCH_HEAD" {
		c.sha = c.fetchHead
	} else {
		c.sha = ref
	}
	return nil
}

func (f *fakeGit) CreateTag(_ context.Context, dir, tag, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones[dir].tags[tag] = true
	return nil
}

func newCollectOpts(t *testing.T, git gitRunner, repos []gh.Repo, rosterCSV, teamsYML, commitsYML string) *collectOpts {
	t.Helper()
	base := t.TempDir()
	o := &collectOpts{
		g:         assignGlobals(),
		out:       filepath.Join(base, "out"),
		label:     "test",
		now:       func() time.Time { return time.Date(2026, 6, 29, 14, 12, 33, 0, time.UTC) },
		newClient: func(context.Context) (collectClient, error) { return fakeCollectClient{repos}, nil },
		git:       git,
	}
	write := func(name, content string) string {
		p := filepath.Join(base, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if rosterCSV != "" {
		o.roster = write("roster.csv", rosterCSV)
	}
	if teamsYML != "" {
		o.teams = write("teams.yml", teamsYML)
	}
	if commitsYML != "" {
		o.commits = write("commits.yml", commitsYML)
	}
	return o
}

func hw1Repos() []gh.Repo {
	return []gh.Repo{
		{Name: "hw1-ada", DefaultBranch: "main"},
		{Name: "hw1-alan", DefaultBranch: "main"},
		{Name: "hw1-grace", DefaultBranch: "main"},
		{Name: "hw1-template", DefaultBranch: "main", IsTemplate: true}, // excluded
	}
}

func TestCollectFresh(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "3 collected, 0 updated, 0 up-to-date, 0 skipped, 0 failed") {
		t.Errorf("summary wrong:\n%s", buf.String())
	}
	for _, key := range []string{"ada", "alan", "grace"} {
		dir := filepath.Join(o.out, key)
		if !git.clones[dir].tags["gh-cls/collect/test"] {
			t.Errorf("%s not tagged", key)
		}
	}
	// Manifest: header + 3 rows.
	recs := readCSV(t, filepath.Join(o.out, "collected.csv"))
	if len(recs) != 4 {
		t.Fatalf("manifest should have header + 3 rows, got %v", recs)
	}
	ada := manifestRow(recs, "hw1-ada")
	if ada == nil || ada[0] != "test" || ada[3] != "sha-hw1-ada" || ada[4] != "main" {
		t.Errorf("hw1-ada manifest row wrong: %v", ada)
	}
}

// manifestRow finds the collect manifest row for a repo (column index 2).
func manifestRow(recs [][]string, repo string) []string {
	for _, r := range recs[1:] {
		if len(r) > 2 && r[2] == repo {
			return r
		}
	}
	return nil
}

func TestCollectIdempotentSameLabel(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	// Second run under the same label: everything already tagged, nothing redone.
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "0 collected, 0 updated, 3 up-to-date") {
		t.Errorf("a same-label re-run should be all up-to-date:\n%s", buf.String())
	}
}

func TestCollectDirtySkipped(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	// ada is an existing clone with local changes and no tag yet.
	git.seed(filepath.Join(o.out, "ada"), "sha-old", false)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "skipped (local changes) hw1-ada") {
		t.Errorf("a dirty clone should be skipped, not clobbered:\n%s", buf.String())
	}
	// ada keeps its old sha (untouched); the other two are collected.
	if git.clones[filepath.Join(o.out, "ada")].sha != "sha-old" {
		t.Error("a dirty clone must not be moved")
	}
}

func TestCollectNonFFWarns(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, "sha-old", true) // clean, untagged
	git.forced[adaDir] = true
	git.remoteTip[adaDir] = "sha-new"
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "updated hw1-ada") || !strings.Contains(buf.String(), "rewritten") {
		t.Errorf("a forced update should be reported with a warning:\n%s", buf.String())
	}
	if git.clones[adaDir].sha != "sha-new" {
		t.Errorf("ada should be at the new tip, got %q", git.clones[adaDir].sha)
	}
}

func TestCollectPinned(t *testing.T) {
	git := newFakeGit()
	// grace has no SHA, so it is skipped.
	commits := "ada: aaaa1111\nalan: bbbb2222\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if git.clones[filepath.Join(o.out, "ada")].sha != "aaaa1111" {
		t.Errorf("ada should be at the pinned SHA, got %q", git.clones[filepath.Join(o.out, "ada")].sha)
	}
	if !strings.Contains(buf.String(), "skipped (no pinned SHA) hw1-grace") {
		t.Errorf("a unit with no pinned SHA should be skipped:\n%s", buf.String())
	}
	ada := manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada")
	if ada == nil || ada[3] != "aaaa1111" || ada[4] != "(pinned)" {
		t.Errorf("pinned manifest row wrong: %v", ada)
	}
}

func TestCollectReconcile(t *testing.T) {
	git := newFakeGit()
	// Roster has ada, alan, grace; repos have ada, alan, and an unexpected zzz (no grace).
	repos := []gh.Repo{
		{Name: "hw1-ada", DefaultBranch: "main"},
		{Name: "hw1-alan", DefaultBranch: "main"},
		{Name: "hw1-zzz", DefaultBranch: "main"},
	}
	o := newCollectOpts(t, git, repos, assignRoster, "", "")
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "missing") || !strings.Contains(out, "grace") {
		t.Errorf("a student with no repo should be reported missing:\n%s", out)
	}
	if !strings.Contains(out, "unexpected") || !strings.Contains(out, "hw1-zzz") {
		t.Errorf("an unexpected repo should be reported:\n%s", out)
	}
	// The unexpected repo is still collected.
	if !git.clones[filepath.Join(o.out, "zzz")].tags["gh-cls/collect/test"] {
		t.Error("an unexpected repo should still be collected")
	}
}

func TestCollectGroupNeedsTeams(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, nil, "", "team-alpha: [student-001]\n", "")
	// project is a group assignment; passing --teams (no roster) is correct.
	if err := o.run(context.Background(), &bytes.Buffer{}, "project"); err != nil {
		t.Fatalf("group with --teams should be accepted, got %v", err)
	}

	// A roster on a group assignment is rejected.
	o2 := newCollectOpts(t, newFakeGit(), nil, assignRoster, "team-alpha: [student-001]\n", "")
	if err := o2.run(context.Background(), &bytes.Buffer{}, "project"); err == nil || !strings.Contains(err.Error(), "--roster is not allowed") {
		t.Fatalf("a roster on a group assignment should be rejected, got %v", err)
	}

	// An individual assignment with no roster is rejected.
	o3 := newCollectOpts(t, newFakeGit(), nil, "", "", "")
	if err := o3.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "--roster is required") {
		t.Fatalf("an individual assignment without a roster should be rejected, got %v", err)
	}
}

func TestCollectDryRun(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	o.dryRun = true
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(git.cloned) != 0 {
		t.Errorf("dry-run must clone nothing, cloned %v", git.cloned)
	}
	if !strings.Contains(buf.String(), "DRY RUN") || !strings.Contains(buf.String(), "would collect hw1-ada") {
		t.Errorf("dry-run output wrong:\n%s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(o.out, "collected.csv")); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry-run must not write a manifest")
	}
}

func TestCollectCloneFailureReported(t *testing.T) {
	git := newFakeGit()
	git.cloneErr["hw1-alan"] = errors.New("boom")
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "1 repo(s) failed") {
		t.Fatalf("a clone failure should surface, got %v", err)
	}
	if !strings.Contains(buf.String(), "FAILED hw1-alan") {
		t.Errorf("the failed repo should be named:\n%s", buf.String())
	}
	// The other repos still collected (one failure does not abort the rest).
	if !strings.Contains(buf.String(), "2 collected") {
		t.Errorf("other repos should still be collected:\n%s", buf.String())
	}
}
