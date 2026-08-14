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
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeCollectClient returns a preset repo list, filtered by prefix.
func fakeCollectClient(repos []gh.Repo) *ghtest.Fake {
	return &ghtest.Fake{
		ListOrgReposByPrefixFunc: func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
			var out []gh.Repo
			for _, r := range repos {
				if strings.HasPrefix(r.Name, prefix) {
					out = append(out, r)
				}
			}
			return out, nil
		},
	}
}

// fakeClone is the in-memory state of one cloned repo.
type fakeClone struct {
	sha       string
	fetchHead string
	origin    string
	clean     bool
	tags      map[string]bool
	// tagSHA is the commit each tag names, which is not necessarily the clone's
	// current sha: a grading checkout can move HEAD after a collection.
	tagSHA map[string]string
}

// originURL is the remote a clone of org/repo carries, as gh writes it.
func originURL(org, repo string) string {
	return "https://github.com/" + org + "/" + repo + ".git"
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

// seed registers an existing clone at dir, cloned from origin.
func (f *fakeGit) seed(dir, origin, sha string, clean bool, tags ...string) {
	c := &fakeClone{sha: sha, origin: origin, clean: clean, tags: map[string]bool{}, tagSHA: map[string]string{}}
	for _, t := range tags {
		c.tags[t] = true
		c.tagSHA[t] = sha
	}
	f.clones[dir] = c
}

func (f *fakeGit) CloneExists(dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir] != nil
}

func (f *fakeGit) Clone(_ context.Context, org, repo, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.cloneErr[repo]; e != nil {
		return e
	}
	f.clones[dir] = &fakeClone{sha: "sha-" + repo, origin: originURL(org, repo), clean: true, tags: map[string]bool{}, tagSHA: map[string]string{}}
	f.cloned = append(f.cloned, dir)
	return nil
}

func (f *fakeGit) RemoteURL(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].origin, nil
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

func (f *fakeGit) TagSHA(_ context.Context, dir, tag string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if !c.tags[tag] {
		return "", errors.New("unknown revision " + tag)
	}
	return c.tagSHA[tag], nil
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

func (f *fakeGit) CreateTag(_ context.Context, dir, tag, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones[dir].tags[tag] = true
	f.clones[dir].tagSHA[tag] = sha
	return nil
}

func newCollectOpts(t *testing.T, git gitRunner, repos []gh.Repo, rosterCSV, groupsYML, commitsYML string) *collectOpts {
	t.Helper()
	base := t.TempDir()
	o := &collectOpts{
		g:         assignGlobals(),
		out:       filepath.Join(base, "out"),
		label:     "test",
		now:       func() time.Time { return time.Date(2026, 6, 29, 14, 12, 33, 0, time.UTC) },
		newClient: func(context.Context) (collectClient, error) { return fakeCollectClient(repos), nil },
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
	if groupsYML != "" {
		o.groups = write("groups.yml", groupsYML)
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
	git.seed(filepath.Join(o.out, "ada"), originURL("cs101-spring26", "hw1-ada"), "sha-old", false)
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
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-old", true) // clean, untagged
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

func TestCollectGroupNeedsGroups(t *testing.T) {
	git := newFakeGit()
	o := newCollectOpts(t, git, nil, "", "group-alpha: [student-001]\n", "")
	// project is a group assignment; passing --groups (no roster) is correct.
	if err := o.run(context.Background(), &bytes.Buffer{}, "project"); err != nil {
		t.Fatalf("group with --groups should be accepted, got %v", err)
	}

	// A roster on a group assignment is rejected.
	o2 := newCollectOpts(t, newFakeGit(), nil, assignRoster, "group-alpha: [student-001]\n", "")
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

func TestCollectFillsManifestHolesLeftByAnInterruptedRun(t *testing.T) {
	// Tags are written per repo as the run proceeds, the manifest only at the end.
	// A run that dies at 80/100 leaves those 80 tagged and unrecorded, and on the
	// re-run they are "up-to-date": they used to be filtered out of the manifest,
	// so their SHAs never entered the record of what was graded and no later run
	// could ever put them there.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	tag := "gh-cls/collect/test"
	// ada and alan were collected and tagged before the interruption; the manifest
	// was never written. grace was never reached.
	git.seed(filepath.Join(o.out, "ada"), originURL("cs101-spring26", "hw1-ada"), "sha-ada-collected", true, tag)
	git.seed(filepath.Join(o.out, "alan"), originURL("cs101-spring26", "hw1-alan"), "sha-alan-collected", true, tag)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "1 collected, 0 updated, 2 up-to-date") {
		t.Errorf("the tagged repos should read as up-to-date:\n%s", buf.String())
	}
	recs := readCSV(t, filepath.Join(o.out, "collected.csv"))
	if len(recs) != 4 {
		t.Fatalf("every repo belongs in the manifest, got %v", recs)
	}
	// Each recovered row carries the SHA its tag names, which is what was graded.
	for repo, sha := range map[string]string{
		"hw1-ada":   "sha-ada-collected",
		"hw1-alan":  "sha-alan-collected",
		"hw1-grace": "sha-hw1-grace",
	} {
		row := manifestRow(recs, repo)
		if row == nil {
			t.Errorf("%s is missing from the manifest: %v", repo, recs)
			continue
		}
		if row[3] != sha {
			t.Errorf("%s recorded at %q, want the tagged commit %q", repo, row[3], sha)
		}
	}
}

func TestCollectNeverWritesAManifestRowTwice(t *testing.T) {
	// The repair reads the manifest to decide what is missing, so it has to be
	// safe to run again: a third, fourth, fifth run must not keep appending the
	// same rows.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	for i := range 3 {
		if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	recs := readCSV(t, filepath.Join(o.out, "collected.csv"))
	if len(recs) != 4 {
		t.Errorf("three runs should leave header + 3 rows, got %v", recs)
	}
}

func TestCollectUpToDateReadsTheTagNotHead(t *testing.T) {
	// A grading checkout can move HEAD after a collection. The manifest records
	// what was collected, so the recovered row must come from the tag.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	tag := "gh-cls/collect/test"
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-collected", true, tag)
	git.clones[adaDir].sha = "sha-moved-by-grader" // HEAD moved since; the tag did not

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	row := manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada")
	if row == nil || row[3] != "sha-collected" {
		t.Errorf("the manifest should record the tagged commit, got %v", row)
	}
}

func TestCollectRejectsClonesOfAnotherRepo(t *testing.T) {
	// Reusing one --out directory across assignments (COLLECT.md's own example uses
	// a generic ./submissions) leaves hw0's clones where hw1's belong. Fetching
	// into them would grade hw0's code and record it in the manifest under hw1's
	// repo name, with nothing in the output to show it.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, originURL("cs101-spring26", "hw0-ada"), "sha-hw0", true)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "1 repo(s) failed") {
		t.Fatalf("a clone of the wrong repo should fail that repo, got %v", err)
	}
	out := buf.String()
	for _, want := range []string{"FAILED hw1-ada", "hw0-ada", "cs101-spring26/hw1-ada", "different --out", adaDir} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure should mention %q:\n%s", want, out)
		}
	}
	// Never fetched, never tagged: the wrong clone is left exactly as it was.
	if c := git.clones[adaDir]; c.sha != "sha-hw0" || len(c.tags) != 0 {
		t.Errorf("the mismatched clone must be left untouched, got %+v", c)
	}
	if manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada") != nil {
		t.Error("a repo that was never collected must not appear in the manifest")
	}
}

func TestOriginNames(t *testing.T) {
	// Every form a git remote takes for the same repository, and the near misses
	// that must not pass.
	cases := map[string]bool{
		"https://github.com/cs101-spring26/hw1-ada.git": true,
		"https://github.com/cs101-spring26/hw1-ada":     true,
		"https://github.com/cs101-spring26/hw1-ada/":    true,
		"git@github.com:cs101-spring26/hw1-ada.git":     true,
		"ssh://git@github.com/cs101-spring26/hw1-ada":   true,
		"https://github.com/CS101-Spring26/HW1-Ada.git": true, // GitHub names are case-insensitive
		"https://github.com/cs101-spring26/hw1-adam":    false,
		"https://github.com/cs101-spring26/hw0-ada.git": false,
		"https://github.com/other-org/hw1-ada.git":      false,
		"": false,
	}
	for origin, want := range cases {
		if got := originNames(origin, "cs101-spring26", "hw1-ada"); got != want {
			t.Errorf("originNames(%q) = %v, want %v", origin, got, want)
		}
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
