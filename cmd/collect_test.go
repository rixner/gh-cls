package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		// The default-branch tip, named the way fakeGit names a fresh clone's
		// commit, so an unpinned collect lands where it always has. A test that
		// needs a moving tip overrides this.
		GetRefFunc: func(_ context.Context, _, repo, _ string) (string, error) {
			return "sha-" + repo, nil
		},
		// An ordinary update: the new commit descends from the old one.
		CompareCommitsFunc: func(_ context.Context, _, _, _, _ string) (string, bool, error) {
			return "ahead", true, nil
		},
	}
}

// fakeClone is the in-memory state of one cloned repo.
type fakeClone struct {
	sha    string
	origin string
	modified  int  // tracked files changed
	untracked int  // files git would report as ??
	headHeld  bool // some branch, tag or remote-tracking ref contains HEAD
	// branch is the branch HEAD is on, empty when detached. A fresh clone lands
	// on one; collect is expected to detach and delete it.
	branch          string
	deletedBranches []string
	config          map[string]string
	// present are commits fetched into the clone beyond the one checked out.
	present []string
	// hasHistory stands in for a full clone: only then can the clone answer an
	// ancestry question at all. ancestors maps a commit to those behind it.
	hasHistory bool
	ancestors  map[string][]string
	tags       map[string]bool
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
	clones map[string]*fakeClone
	cloneErr    map[string]error // repo -> error returned by Clone
	checkoutErr map[string]error // dir -> error returned by Checkout
	fetchErr    map[string]error // ref -> error returned by Fetch
	emptyRepos  map[string]bool  // repo -> clones fine but has no commits
	// tips is the default-branch tip GetRef reports per repo, which is how a
	// test moves a student's branch now that the target is resolved before any
	// git runs. Unset means "sha-<repo>".
	tips    map[string]string
	fetched []string // refs fetched, for asserting the fetch rule held
	// compare is what GitHub's compare API reports for a repo. Unset means
	// "ahead": the new commit descends from the old one, an ordinary update.
	compare map[string]string
	// compareMissing marks a repo whose recorded commit GitHub no longer has,
	// which is what a force-push can leave behind.
	compareMissing map[string]bool
	cloned      []string         // dirs cloned, for asserting dry-run did nothing
	moved       []string         // dirs a finished clone was moved into place at
	// refFormatErr makes CheckRefFormat fail to run at all, which is a different
	// outcome from git rejecting the name.
	refFormatErr error
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		clones:  map[string]*fakeClone{},
		compare: map[string]string{},
		cloneErr:    map[string]error{},
		checkoutErr: map[string]error{},
		fetchErr:    map[string]error{},
		emptyRepos:  map[string]bool{},
		tips:           map[string]string{},
		compareMissing: map[string]bool{},
	}
}

// tip is the commit GetRef reports for a repo's default branch.
func (f *fakeGit) tip(repo string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tips[repo]; ok {
		return t
	}
	return "sha-" + repo
}

// comparison is what GitHub reports about a repo's previous commit and its new
// one, standing in for the compare API a shallow clone has to fall back on.
func (f *fakeGit) comparison(repo string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.compareMissing[repo] {
		return "", false, nil
	}
	if s, ok := f.compare[repo]; ok {
		return s, true, nil
	}
	return "ahead", true, nil
}

// seed registers an existing clone at dir, cloned from origin. clean false means
// a worktree with a modified tracked file, which is what it has always meant
// here; a test wanting untracked files or a stranded HEAD sets those fields on
// the returned clone directly.
func (f *fakeGit) seed(dir, origin, sha string, clean bool, tags ...string) {
	modified := 0
	if !clean {
		modified = 1
	}
	c := &fakeClone{sha: sha, origin: origin, modified: modified, headHeld: true,
		config: map[string]string{}, tags: map[string]bool{}, tagSHA: map[string]string{}}
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
	if f.emptyRepos[repo] {
		// An empty repository clones successfully and has no commit.
		f.clones[dir] = &fakeClone{origin: originURL(org, repo), config: map[string]string{},
			tags: map[string]bool{}, tagSHA: map[string]string{}}
		f.cloned = append(f.cloned, dir)
		return nil
	}
	// A fresh clone lands on the default branch, which is what collect has to
	// detach from and delete.
	f.clones[dir] = &fakeClone{sha: "sha-" + repo, origin: originURL(org, repo), headHeld: true, branch: "main",
		config: map[string]string{}, tags: map[string]bool{}, tagSHA: map[string]string{}}
	f.cloned = append(f.cloned, dir)
	return nil
}

func (f *fakeGit) RemoteURL(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].origin, nil
}

func (f *fakeGit) WorktreeState(_ context.Context, dir string) (worktreeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	return worktreeState{modified: c.modified, untracked: c.untracked}, nil
}

func (f *fakeGit) HeadHeldByRef(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].headHeld, nil
}

// Head fails when the clone holds no commit, which is how git behaves on a
// clone of an empty repository and on one a killed run left half-made.
func (f *fakeGit) Head(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if c == nil || c.sha == "" {
		return "", errors.New("fatal: ambiguous argument 'HEAD': unknown revision")
	}
	return c.sha, nil
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

func (f *fakeGit) Fetch(_ context.Context, dir, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.fetchErr[ref]; e != nil {
		return e
	}
	c := f.clones[dir]
	c.present = append(c.present, ref)
	f.fetched = append(f.fetched, ref)
	return nil
}

func (f *fakeGit) HasCommit(_ context.Context, dir, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if c == nil {
		return false, nil
	}
	return c.sha == sha || slices.Contains(c.present, sha), nil
}

// IsAncestor answers only when the test says the clone has the history for it,
// which stands in for a full clone; a shallow one cannot answer at all.
func (f *fakeGit) IsAncestor(_ context.Context, dir, a, b string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if c == nil || !c.hasHistory {
		return false, false, nil
	}
	return slices.Contains(c.ancestors[b], a), true, nil
}

func (f *fakeGit) Checkout(_ context.Context, dir, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.checkoutErr[dir]; e != nil {
		return e
	}
	c := f.clones[dir]
	// Every target is a SHA now, so a checkout names the commit outright.
	c.sha = ref
	c.branch = ""
	return nil
}

func (f *fakeGit) CreateTag(_ context.Context, dir, tag, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones[dir].tags[tag] = true
	f.clones[dir].tagSHA[tag] = sha
	return nil
}

func (f *fakeGit) MoveIntoPlace(_ context.Context, staging, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clones[dir] != nil {
		return errTargetExists
	}
	f.clones[dir] = f.clones[staging]
	delete(f.clones, staging)
	f.moved = append(f.moved, dir)
	return nil
}

func (f *fakeGit) CurrentBranch(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clones[dir].branch, nil
}

func (f *fakeGit) DeleteBranch(_ context.Context, dir, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clones[dir]
	if c.branch == branch {
		c.branch = ""
	}
	c.deletedBranches = append(c.deletedBranches, branch)
	return nil
}

func (f *fakeGit) SetConfig(_ context.Context, dir, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones[dir].config[key] = value
	return nil
}

func (f *fakeGit) CheckRefFormat(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refFormatErr != nil {
		return false, f.refFormatErr
	}
	return refNameLooksValid(ref), nil
}

// refNameLooksValid implements the part of git-check-ref-format(1) a collect
// label can plausibly violate. Git itself is the authority; the fake exists so
// the unit tests need no git, and TestCheckRefFormatMatchesGit holds the two to
// the same answers so this cannot drift into a comfortable fiction.
func refNameLooksValid(ref string) bool {
	if ref == "" || ref == "@" ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.Contains(ref, "@{") {
		return false
	}
	for _, r := range ref {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func newCollectOpts(t *testing.T, git *fakeGit, repos []gh.Repo, rosterCSV, groupsYML, snapshotYML string) *collectOpts {
	t.Helper()
	base := t.TempDir()
	client := fakeCollectClient(repos)
	// The tip comes from the runner's own table, so a test moves a student's
	// branch the same way it sets up everything else about that clone.
	client.GetRefFunc = func(_ context.Context, _, repo, _ string) (string, error) {
		return git.tip(repo), nil
	}
	client.CompareCommitsFunc = func(_ context.Context, _, repo, _, _ string) (string, bool, error) {
		return git.comparison(repo)
	}
	o := &collectOpts{
		g:         assignGlobals(),
		out:       filepath.Join(base, "out"),
		label:     "test",
		now:       func() time.Time { return time.Date(2026, 6, 29, 14, 12, 33, 0, time.UTC) },
		newClient: func(context.Context) (collectClient, error) { return client, nil },
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
	if snapshotYML != "" {
		o.snapshot = write("snapshot.yml", snapshotYML)
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
	if !strings.Contains(buf.String(), "3 collected, 0 updated, 0 up-to-date, 0 skipped, 0 refused, 0 failed") {
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

func TestCollectFetchesOnlyWhatTheCloneLacks(t *testing.T) {
	// P1, and the reason every target is resolved to a SHA before any git runs.
	// A depth-1 fetch naming a commit the clone already holds makes that commit
	// a shallow boundary: every tag whose history runs through it loses that
	// history, and the next gc deletes the commits. Knowing the target up front
	// is what lets collect skip the fetch entirely when the commit is there,
	// and name a commit rather than a branch when it is not.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	alanDir := filepath.Join(o.out, "alan")
	// ada already sits on the commit collect will resolve, carrying an earlier
	// label's tag whose history a boundary would destroy.
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-hw1-ada", true, "gh-cls/collect/midterm")
	// alan's student has pushed since.
	git.seed(alanDir, originURL("cs101-spring26", "hw1-alan"), "sha-old", true)
	git.tips["hw1-alan"] = "sha-moved"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	t.Log("\n" + buf.String())

	if slices.Contains(git.fetched, "sha-hw1-ada") {
		t.Errorf("a commit the clone already holds must never be fetched, fetched %v", git.fetched)
	}
	if !slices.Contains(git.fetched, "sha-moved") {
		t.Errorf("a commit the clone lacks has to be fetched, fetched %v", git.fetched)
	}
	// Never a branch: a depth-1 fetch by branch name lands on whatever the tip
	// is now, which may be a commit the clone already has.
	for _, ref := range git.fetched {
		if !strings.HasPrefix(ref, "sha-") {
			t.Errorf("every fetch should name a commit, got %q in %v", ref, git.fetched)
		}
	}
	if git.clones[adaDir].sha != "sha-hw1-ada" {
		t.Errorf("ada should be where it already was, got %q", git.clones[adaDir].sha)
	}
	if git.clones[alanDir].sha != "sha-moved" {
		t.Errorf("alan should have moved to the new commit, got %q", git.clones[alanDir].sha)
	}
}

func TestCollectTellsARewriteFromAnOrdinaryUpdate(t *testing.T) {
	// P5: the old warning read git's "(forced update)" text, which was wrong
	// both ways. It fired on every ordinary update of a shallow clone, because
	// the old tip's ancestry is not local so git cannot tell a fast-forward from
	// a rewrite, and it never fired on a pinned run, because fetching a SHA
	// updates no tracking ref. The question is now asked of the commits.
	for _, tc := range []struct {
		name, status string
		missing      bool
		wantNote     bool
	}{
		{name: "an ordinary update", status: "ahead"},
		{name: "a rewritten history", status: "diverged", wantNote: true},
		{name: "a recorded commit GitHub no longer has", missing: true, wantNote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := newFakeGit()
			o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
			adaDir := filepath.Join(o.out, "ada")
			git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-old", true)
			seedManifest(t, o.out, "midterm", "ada", "hw1-ada", "sha-old")
			git.tips["hw1-ada"] = "sha-new"
			if tc.status != "" {
				git.compare["hw1-ada"] = tc.status
			}
			git.compareMissing["hw1-ada"] = tc.missing

			var buf bytes.Buffer
			if err := o.run(context.Background(), &buf, "hw1"); err != nil {
				t.Fatalf("a rewrite is collected, not failed: %v\n%s", err, buf.String())
			}
			out := buf.String()
			t.Log("\n" + out)

			// Either way the new commit is collected and the old tag stands.
			if !strings.Contains(out, "updated hw1-ada") {
				t.Errorf("the new commit should still be collected:\n%s", out)
			}
			if git.clones[adaDir].sha != "sha-new" {
				t.Errorf("ada should be at the new commit, got %q", git.clones[adaDir].sha)
			}
			if got := strings.Contains(out, "rewritten"); got != tc.wantNote {
				t.Errorf("rewrite note = %v, want %v:\n%s", got, tc.wantNote, out)
			}
			if tc.wantNote && !strings.Contains(out, "midterm") {
				t.Errorf("the note should name the label that still holds the old commit:\n%s", out)
			}
		})
	}
}

// Snapshot SHAs are full-length: collect rejects an abbreviation rather than
// resolving it, so these fixtures spell whole object names.
const (
	adaSHA  = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	alanSHA = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
)

func TestCollectPinned(t *testing.T) {
	git := newFakeGit()
	// grace has no SHA, so it is skipped.
	commits := "ada: " + adaSHA + "\nalan: " + alanSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if git.clones[filepath.Join(o.out, "ada")].sha != adaSHA {
		t.Errorf("ada should be at the pinned SHA, got %q", git.clones[filepath.Join(o.out, "ada")].sha)
	}
	if !strings.Contains(buf.String(), "skipped (not in the snapshot) hw1-grace") {
		t.Errorf("a unit absent from the snapshot should be skipped:\n%s", buf.String())
	}
	ada := manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada")
	if ada == nil || ada[3] != adaSHA || ada[4] != "(pinned)" {
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
	if len(git.fetched) != 0 {
		t.Errorf("dry-run must fetch nothing, fetched %v", git.fetched)
	}
	if !strings.Contains(buf.String(), "DRY RUN") || !strings.Contains(buf.String(), "would collect hw1-ada") {
		t.Errorf("dry-run output wrong:\n%s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(o.out, "collected.csv")); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry-run must not write a manifest")
	}
}

func TestCollectDryRunInspectsEachClone(t *testing.T) {
	// P15: the dry run listed every repository as "would collect" without
	// opening a single clone, so it could not say what was already up to date,
	// what was dirty, or what a real run would refuse. That is the only thing
	// worth knowing before committing a class-sized run to the network.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	o.dryRun = true
	tag := "gh-cls/collect/test"
	// Already collected under this label.
	git.seed(filepath.Join(o.out, "ada"), originURL("cs101-spring26", "hw1-ada"), "sha-hw1-ada", true, tag)
	// A grader is part way through something.
	git.seed(filepath.Join(o.out, "alan"), originURL("cs101-spring26", "hw1-alan"), "sha-old", false)
	// grace has no clone yet, so it would be a new one.

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)

	// Each repository gets the outcome a real run would give it, not one label.
	for _, want := range []string{
		"up-to-date hw1-ada",
		"skipped (local changes) hw1-alan",
		"would collect hw1-grace",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan should report %q:\n%s", want, out)
		}
	}
	// And it says what the run would cost, which is the reason to look first.
	if !strings.Contains(out, "Plan for test:") || !strings.Contains(out, "paced git operation(s)") {
		t.Errorf("the plan should state what a real run would spend:\n%s", out)
	}
	// One new clone is the only thing needing the network here.
	if !strings.Contains(out, "1 paced git operation(s)") {
		t.Errorf("only the new clone costs a request:\n%s", out)
	}
	// The repo a real run would pass over is named again at the end.
	if !strings.Contains(out, "Would not be collected under test (1)") {
		t.Errorf("the plan should account for what it would skip:\n%s", out)
	}
	if len(git.cloned) != 0 || len(git.fetched) != 0 {
		t.Errorf("a dry run touches the network for nothing: cloned %v fetched %v", git.cloned, git.fetched)
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

func TestCollectAccountsForEveryRepoAtTheEnd(t *testing.T) {
	// P13: dirty, refused and skipped repositories keep their previous commit
	// checked out, each reported in one line that has scrolled away by the end
	// of a class-sized run. A grader who opens the directory sees code either
	// way and cannot tell the label passed it over, so they grade the old work.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	alanDir := filepath.Join(o.out, "alan")
	graceDir := filepath.Join(o.out, "grace")
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-old", false) // modified tracked files
	git.seed(alanDir, originURL("cs101-spring26", "hw1-alan"), "sha-alan", true)
	git.clones[alanDir].headHeld = false // a grader committed here
	git.seed(graceDir, originURL("cs101-spring26", "hw1-grace"), "sha-grace", true)
	git.clones[graceDir].untracked = 2
	git.tips["hw1-grace"] = "sha-grace-new"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)

	tail := out[strings.Index(out, "0 collected"):]
	if !strings.Contains(tail, "Not collected under test (2)") {
		t.Errorf("the end of the run should account for both uncollected repos:\n%s", tail)
	}
	// Each is named again, with why and what to do, after the streaming lines.
	for _, want := range []string{"hw1-ada", "tracked file(s) modified", "hw1-alan", "no branch or tag holds"} {
		if !strings.Contains(tail, want) {
			t.Errorf("the end-of-run list should mention %q:\n%s", want, tail)
		}
	}
	// Collected, but with the grader's files still there.
	if !strings.Contains(tail, "Collected, with something to note (1)") ||
		!strings.Contains(tail, "hw1-grace") {
		t.Errorf("a repo collected over untracked files should be noted:\n%s", tail)
	}
	// A clean collection is not dragged into either list.
	if strings.Count(tail, "hw1-grace") != 1 {
		t.Errorf("hw1-grace should appear once at the end:\n%s", tail)
	}
}

func TestCollectRefusesASecondRunInTheSameDirectory(t *testing.T) {
	// P12: two runs into one --out race on the clones and the tags, and both
	// read the manifest before appending, so rows can be duplicated.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := acquireRunLock(o.out)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	runErr := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if runErr == nil {
		t.Fatal("a second run in the same directory should refuse")
	}
	t.Log("\n" + runErr.Error())
	for _, want := range []string{"already running", "host:", "pid:", "delete"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, runErr)
		}
	}
	if len(git.cloned) != 0 {
		t.Errorf("a refused run must clone nothing, cloned %v", git.cloned)
	}

	// Once the first run is done the directory is free again.
	if err := held.release(); err != nil {
		t.Fatal(err)
	}
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatalf("the directory should be usable once released: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.out, gitCLSDir, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a finished run should leave no lock behind")
	}
}

func TestCollectDryRunTakesNoLock(t *testing.T) {
	// A dry run writes nothing, the lock included, so it can be run against a
	// directory a real collection is working in.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	o.dryRun = true
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.out, gitCLSDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run should create nothing under --out")
	}
}

func TestCollectRefusesAKeyThatCollidesWithItsOwnDirectory(t *testing.T) {
	// <out>/.gh-cls holds the lock and the staging area. A repository whose key
	// landed there would fight collect for the same directory.
	repos := []gh.Repo{{Name: "hw1-.gh-cls", DefaultBranch: "main"}}
	git := newFakeGit()
	o := newCollectOpts(t, git, repos, assignRoster, "", "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	out := buf.String()
	t.Log("\n" + out)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a key colliding with collect's own directory should be refused, got %v", err)
	}
	if !strings.Contains(out, "collect uses for its own directory") {
		t.Errorf("the refusal should say why:\n%s", out)
	}
	if len(git.cloned) != 0 {
		t.Errorf("nothing should be cloned for it, cloned %v", git.cloned)
	}
}

func TestCollectLeavesNothingBehindWhenAFirstCollectionFails(t *testing.T) {
	// P7: collect cloned the tip into <out>/<key> and then fetched the pinned
	// SHA. When that fetch failed the tip stayed on disk, untagged and often
	// past the deadline, and a grader browsing the directory saw what looked
	// like a collection.
	git := newFakeGit()
	commits := "ada: " + adaSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)
	git.fetchErr[adaSHA] = errors.New("could not find remote ref " + adaSHA)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	t.Log("\n" + buf.String())
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a failed pinned clone should fail that repo, got %v", err)
	}

	adaDir := filepath.Join(o.out, "ada")
	if _, statErr := os.Stat(adaDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("%s must not exist after a failed first collection", adaDir)
	}
	if git.clones[adaDir] != nil {
		t.Error("no clone may be left in place after a failed first collection")
	}
}

func TestNewClonesKeepNoBranchAndWillNotGuessOne(t *testing.T) {
	// P8: a new clone was left on a local branch sitting at the tip from clone
	// time, so `git checkout main`, or a grading script running `git pull`,
	// silently handed back code other than what was collected. Deleting the
	// branch is not enough on its own: git recreates it from origin/main unless
	// guessing is turned off.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}

	c := git.clones[filepath.Join(o.out, "ada")]
	if c == nil {
		t.Fatal("ada should have been collected")
	}
	if c.branch != "" {
		t.Errorf("a new clone should end detached, on branch %q", c.branch)
	}
	if !slices.Contains(c.deletedBranches, "main") {
		t.Errorf("the branch the clone created should be deleted, deleted %v", c.deletedBranches)
	}
	if got := c.config["checkout.guess"]; got != "false" {
		t.Errorf("checkout.guess should be false so the branch cannot be guessed back, got %q", got)
	}
}

func TestCollectSkipsAnEmptyRepositoryAndCreatesNoDirectory(t *testing.T) {
	// P14: the clone succeeded, reading HEAD failed, and an empty clone was left
	// behind that failed on every later run.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	git.emptyRepos["hw1-ada"] = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("an empty repository is a skip, not a failure: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	if !strings.Contains(out, "skipped (empty repository)") || !strings.Contains(out, "no commits yet") {
		t.Errorf("an empty repository should be reported as such:\n%s", out)
	}
	adaDir := filepath.Join(o.out, "ada")
	if _, statErr := os.Stat(adaDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("%s must not be created for a repository with no commits", adaDir)
	}
	if manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada") != nil {
		t.Error("nothing was collected, so nothing belongs in the manifest")
	}
}

func TestCollectRefusesADirectoryItDidNotMake(t *testing.T) {
	// Collect never moves or deletes anything under --out. A directory that is
	// not a clone it can use is refused, empty or not: deciding one is
	// disposable is the instructor's call.
	for _, tc := range []struct{ name, file string }{
		{"holding a file", "notes.txt"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := newFakeGit()
			o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
			adaDir := filepath.Join(o.out, "ada")
			if err := os.MkdirAll(adaDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(adaDir, tc.file), []byte("grader\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var buf bytes.Buffer
			err := o.run(context.Background(), &buf, "hw1")
			out := buf.String()
			t.Log("\n" + out)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("a directory collect cannot use should be refused, got %v", err)
			}
			if !strings.Contains(out, "is not a clone collect can use") {
				t.Errorf("the refusal should say why:\n%s", out)
			}
			if tc.file != "" {
				body, readErr := os.ReadFile(filepath.Join(adaDir, tc.file))
				if readErr != nil || string(body) != "grader\n" {
					t.Errorf("the directory's contents must be untouched, got %q %v", body, readErr)
				}
			}
		})
	}
}

func TestCollectLeavesNoStagingBehind(t *testing.T) {
	// Staging is collect's own scratch space. A finished run should hold nothing
	// there, so a later run is not deciding what an old directory was for.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	staging := filepath.Join(o.out, gitCLSDir, "staging")
	entries, err := os.ReadDir(staging)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging should be empty after a run, holds %v", entries)
	}
}

func TestCollectSkipsAGraderCommitNoRefHolds(t *testing.T) {
	// P2: a clone detached after its first update, where a grader committed a fix
	// so the tests would run. The next label checked the new commit out from
	// under them, leaving it held only by the reflog, which expires; gc then
	// deletes it. COLLECT.md promises grading edits survive.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-grader-fix", true)
	git.clones[adaDir].headHeld = false
	git.tips["hw1-ada"] = "sha-new-tip"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("a stranded HEAD is a skip, not a failure: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{"HEAD held by no ref", shortSHA("sha-grader-fix"), "tag <name>"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skip should mention %q:\n%s", want, out)
		}
	}
	// The grader's commit is still checked out, and nothing was tagged over it.
	if c := git.clones[adaDir]; c.sha != "sha-grader-fix" || len(c.tags) != 0 {
		t.Errorf("the clone must be left exactly as it was, got %+v", c)
	}
}

func TestCollectSkipsWhenAFileIsInTheWayOfTheCheckout(t *testing.T) {
	// P18: a grader's output at a path the new commit tracks. Git refuses the
	// checkout and changes nothing, so this is a repository to come back to once
	// the file is moved, not a failed run.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-old", true)
	git.checkoutErr[adaDir] = errors.New(
		"git checkout origin/main: exit status 1: error: The following untracked working tree files would be overwritten by checkout:\n\tout/result.txt")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("a refused checkout is a skip, not a failure: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{"skipped (files in the way)", "out/result.txt", "move it aside"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skip should mention %q:\n%s", want, out)
		}
	}
	if c := git.clones[adaDir]; c.sha != "sha-old" || len(c.tags) != 0 {
		t.Errorf("nothing may be tagged when the checkout was refused, got %+v", c)
	}
}

func TestCollectTakesACloneWithUntrackedFilesAndSaysSo(t *testing.T) {
	// A behaviour change: any untracked file used to make the worktree "dirty",
	// so a grader's own build output stopped the collection entirely. Untracked
	// files are not a change to the student's code, so they no longer block it;
	// they are reported, since stale output now sits beside the new commit.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-old", true)
	git.clones[adaDir].untracked = 3
	git.tips["hw1-ada"] = "sha-new"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	if !strings.Contains(out, "updated hw1-ada") {
		t.Errorf("untracked files must not stop a collection:\n%s", out)
	}
	if !strings.Contains(out, "3 untracked file(s)") {
		t.Errorf("the untracked files should be reported:\n%s", out)
	}
	if git.clones[adaDir].sha != "sha-new" {
		t.Errorf("the clone should have moved to the new tip, got %q", git.clones[adaDir].sha)
	}
}

// seedManifest writes a collected.csv recording one commit for a repo under a
// label, standing in for what an earlier run left behind.
func seedManifest(t *testing.T, out, label, key, repo, sha string) {
	t.Helper()
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := "label,key,repo,sha,ref,time\n" +
		strings.Join([]string{label, key, repo, sha, "main", "2026-06-29T14:12:33Z"}, ",") + "\n"
	if err := os.WriteFile(filepath.Join(out, "collected.csv"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRefusesALabelAskedForASecondCommit(t *testing.T) {
	// P3: COLLECT.md suggests editing the snapshot to give a student a later
	// commit. Re-running the same label after that edit hit the tag-exists check
	// before anything was compared, so the old commit was silently kept, the run
	// said up-to-date, and the instructor believed the new commit was collected.
	git := newFakeGit()
	commits := "ada: " + adaSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The instructor edits the snapshot to a later commit and re-runs the label.
	if err := os.WriteFile(o.snapshot, []byte("ada: "+alanSHA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	out := buf.String()
	t.Log("\n" + out)

	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a label asked for a second commit should be refused and exit non-zero, got %v", err)
	}
	if strings.Contains(out, "up-to-date hw1-ada") {
		t.Error("the old commit must not be reported as up to date")
	}
	for _, want := range []string{"refused", shortSHA(adaSHA), shortSHA(alanSHA), "new label"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal should mention %q:\n%s", want, out)
		}
	}
	// Nothing moved: the tag still holds what it collected.
	if got := git.clones[filepath.Join(o.out, "ada")].tagSHA["gh-cls/collect/test"]; got != adaSHA {
		t.Errorf("the tag must not move, got %q", got)
	}
}

func TestCollectRefusesWhenTheTagDisagreesWithTheTarget(t *testing.T) {
	// The same refusal reached through the tag rather than the manifest: a run
	// died before writing collected.csv, so the clone is tagged and unrecorded.
	git := newFakeGit()
	tag := "gh-cls/collect/test"
	commits := "ada: " + adaSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)
	git.seed(filepath.Join(o.out, "ada"), originURL("cs101-spring26", "hw1-ada"), alanSHA, true, tag)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	out := buf.String()
	t.Log("\n" + out)

	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a tag disagreeing with the snapshot should be refused, got %v", err)
	}
	for _, want := range []string{"label test holds", shortSHA(alanSHA), "the snapshot says", shortSHA(adaSHA)} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal should mention %q:\n%s", want, out)
		}
	}
}

func TestCollectRecollectsADeletedTagAtTheRecordedCommit(t *testing.T) {
	// P4: deleting a clone or a tag and re-running the same label used to take
	// the current tip under the old label, while the manifest kept the old SHA,
	// so tag and manifest silently disagreed about what had been graded.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	adaDir := filepath.Join(o.out, "ada")
	// The clone is clean and untagged, the manifest still records the commit,
	// and the student has pushed since.
	git.seed(adaDir, originURL("cs101-spring26", "hw1-ada"), "sha-whatever", true)
	git.tips["hw1-ada"] = "sha-pushed-since"
	seedManifest(t, o.out, "test", "ada", "hw1-ada", adaSHA)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	t.Log("\n" + buf.String())

	if got := git.clones[adaDir].sha; got != adaSHA {
		t.Errorf("the recorded commit should be recovered, got %q (the tip is sha-pushed-since)", got)
	}
	if got := git.clones[adaDir].tagSHA["gh-cls/collect/test"]; got != adaSHA {
		t.Errorf("the tag should be recreated at the recorded commit, got %q", got)
	}
	// And the manifest still says the one thing it always said.
	row := manifestRow(readCSV(t, filepath.Join(o.out, "collected.csv")), "hw1-ada")
	if row == nil || row[3] != adaSHA {
		t.Errorf("the manifest must keep recording the collected commit, got %v", row)
	}
}

func TestCollectRefusesALabelGitCannotTag(t *testing.T) {
	// A label git cannot spell as a tag used to fail at the tagging step, by
	// which point every worktree had moved and nothing had been recorded. It is
	// refused while a refusal still costs nothing.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")
	o.label = "fall 2026"

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("a label with a space should be refused:\n%s", buf.String())
	}
	t.Log("\n" + err.Error())
	for _, want := range []string{"fall 2026", "cannot be used in a tag name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
	if len(git.cloned) != 0 {
		t.Errorf("nothing should be cloned before the label is checked, cloned %v", git.cloned)
	}
	if _, err := os.Stat(filepath.Join(o.out, "collected.csv")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused run must leave no manifest")
	}
}

func TestCollectReportsAnUnaskableGitSeparatelyFromABadLabel(t *testing.T) {
	// git failing to run at all is not the instructor's label being wrong, and
	// telling them to fix their label would send them after the wrong problem.
	git := newFakeGit()
	git.refFormatErr = errors.New("exec: \"git\": executable file not found in $PATH")
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil {
		t.Fatal("a git that cannot be run should fail the run")
	}
	t.Log("\n" + err.Error())
	if strings.Contains(err.Error(), "cannot be used in a tag name") {
		t.Errorf("a git failure must not be reported as a bad label: %v", err)
	}
	if !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("the underlying failure should survive: %v", err)
	}
}

func TestCollectRefusesAnAbbreviatedSnapshotSHA(t *testing.T) {
	// Only an empty SHA used to be rejected. An abbreviation reaches the fetch,
	// where it fails per repo, after new clones exist.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "ada: aaaa1111\n")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("an abbreviated SHA should be refused:\n%s", buf.String())
	}
	t.Log("\n" + err.Error())
	for _, want := range []string{"aaaa1111", "full commit SHA", "activity --snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
	if len(git.cloned) != 0 {
		t.Errorf("nothing should be cloned, cloned %v", git.cloned)
	}
}

func TestCollectRefusesASnapshotThatMatchesNoRepository(t *testing.T) {
	// A snapshot file for another assignment matches nothing here. Every repo
	// would be skipped as "not in the snapshot" and the run would collect nobody
	// while reading as though that were the answer.
	git := newFakeGit()
	commits := "zeta: " + adaSHA + "\nomega: " + alanSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("a snapshot matching nothing should be refused:\n%s", buf.String())
	}
	t.Log("\n" + err.Error())
	for _, want := range []string{"none of which match", "zeta", "omega", "another assignment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
	if len(git.cloned) != 0 {
		t.Errorf("nothing should be cloned, cloned %v", git.cloned)
	}
}

func TestCollectNotesSnapshotKeysThatMatchNoRepository(t *testing.T) {
	// A mistyped key silently pins nothing for that student. The run still goes
	// ahead, since the other keys are good, but the typo is named.
	git := newFakeGit()
	commits := "ada: " + adaSHA + "\nadaa: " + alanSHA + "\n"
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", commits)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)
	if !strings.Contains(out, "adaa") || !strings.Contains(out, "match no repository") {
		t.Errorf("a snapshot key matching nothing should be named:\n%s", out)
	}
}

func TestCheckRefFormatMatchesGit(t *testing.T) {
	// The fake stands in for git so the unit tests need none, which is only safe
	// while the two agree. This holds them to the same answers, so a drifted fake
	// (or a git release that changes the rules) fails here rather than during a
	// term's collection.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	const p = "refs/tags/gh-cls/collect/"
	refs := []string{
		p + "final", p + "20260629-141233", p + "ok-dash_under.dot",
		p + "fall 2026", p + "fall..2026", p + "final.lock",
		p + "ca~ret", p + "co:lon", p + "quest?ion", p + "star*",
		p + "brack[et", p + "back\\slash", p + "at@{brace",
		p + "trailing.", p + ".leading", p, p + "/double",
	}
	for _, ref := range refs {
		want, err := execGit{}.CheckRefFormat(context.Background(), ref)
		if err != nil {
			t.Fatalf("asking git about %q: %v", ref, err)
		}
		if got := refNameLooksValid(ref); got != want {
			t.Errorf("refNameLooksValid(%q) = %v, but git says %v", ref, got, want)
		}
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

func TestCollectSkipsAConfiguredTemplateWhoseFlagWasCleared(t *testing.T) {
	// With the template flag cleared, hw1-template would otherwise be cloned as a
	// submission under the key "template". It is the template hw1's config names,
	// so collect excludes it without consulting the flag.
	repos := hw1Repos()
	repos[3] = gh.Repo{Name: "hw1-template", DefaultBranch: "main"} // IsTemplate cleared
	git := newFakeGit()
	o := newCollectOpts(t, git, repos, assignRoster, "", "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "template") {
		t.Errorf("the template repo must not be collected or reported:\n%s", buf.String())
	}
	if _, cloned := git.clones[filepath.Join(o.out, "template")]; cloned {
		t.Error("the template repo must not be cloned as a submission")
	}
}

func TestCollectStreamsEachRepoBeforeTheSummary(t *testing.T) {
	// Cloning a full class takes minutes. The per-repo lines used to print only
	// once every clone had finished, so the run looked stuck the whole time; they
	// are now streamed, numbered, as each repo lands.
	git := newFakeGit()
	o := newCollectOpts(t, git, hw1Repos(), assignRoster, "", "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	t.Log("\n" + out)

	for _, key := range []string{"ada", "alan", "grace"} {
		if !strings.Contains(out, "collected hw1-"+key) {
			t.Errorf("missing a line for %s:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "[3/3]") {
		t.Errorf("lines should be numbered against the total:\n%s", out)
	}
	if strings.Index(out, "[1/3]") > strings.Index(out, "3 collected") {
		t.Errorf("the per-repo lines must precede the summary:\n%s", out)
	}
	// The summary counts rather than re-listing, so no repo appears twice.
	if n := strings.Count(out, "hw1-ada"); n != 1 {
		t.Errorf("hw1-ada should appear once, got %d:\n%s", n, out)
	}
}
