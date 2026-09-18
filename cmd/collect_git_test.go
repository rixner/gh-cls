package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests run the real execGit against local bare repositories standing in
// for student remotes. Nothing here reaches the network.
//
// They exist because the fake cannot show what they check. collect's fake git
// answers questions about its own state; whether the flags collect passes make
// git behave as the design claims is a fact about git, and a fake asked the same
// question would only repeat the assumption back.

// requireGit skips a test when git is not installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// runGit runs a git command in dir, failing the test on error. It is not named
// git, because the unit tests use that as a variable for the fake runner.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"LC_ALL=C", "LANG=C",
		"GIT_AUTHOR_NAME=student", "GIT_AUTHOR_EMAIL=s@example.com",
		"GIT_COMMITTER_NAME=student", "GIT_COMMITTER_EMAIL=s@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOrigin builds a bare repository standing in for a student's remote, with n
// commits on main. It returns the origin's file:// URL and the worktree it was
// pushed from, so a test can add commits and push them.
func newOrigin(t *testing.T, n int) (url, work string) {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	work = filepath.Join(base, "w")
	runGit(t, base, "init", "-q", "--bare", bare)
	runGit(t, base, "clone", "-q", bare, work)
	for i := 1; i <= n; i++ {
		studentCommit(t, work, "c"+strconv.Itoa(i))
	}
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return "file://" + bare, work
}

// studentCommit adds one commit to a worktree and returns its SHA.
func studentCommit(t *testing.T, work, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "f-"+name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", name)
	return runGit(t, work, "rev-parse", "HEAD")
}

func TestHeadHeldByRefSeesAGraderCommitAsUnheld(t *testing.T) {
	// V10/P2. Emptiness is the answer, not the exit status: for-each-ref
	// succeeds either way, so a check written against its exit code would call
	// every clone held and strand the commit it was meant to protect.
	requireGit(t)
	origin, _ := newOrigin(t, 3)
	base := t.TempDir()
	clone := filepath.Join(base, "c")
	runGit(t, base, "clone", "-q", "--depth", "1", "--no-tags", origin, clone)
	runGit(t, clone, "checkout", "-q", "--detach", "HEAD")
	runGit(t, clone, "tag", "gh-cls/collect/final", "HEAD")
	runGit(t, clone, "branch", "-q", "-D", "main")

	held, err := (execGit{}).HeadHeldByRef(context.Background(), clone)
	if err != nil {
		t.Fatalf("HeadHeldByRef: %v", err)
	}
	if !held {
		t.Error("the collect tag holds HEAD, so it should read as held")
	}

	// The grader commits a fix so the tests will run.
	runGit(t, clone, "commit", "-q", "--allow-empty", "-m", "grader patch")
	held, err = (execGit{}).HeadHeldByRef(context.Background(), clone)
	if err != nil {
		t.Fatalf("HeadHeldByRef: %v", err)
	}
	if held {
		t.Error("a commit made on a detached HEAD is held by no ref, and collecting over it would strand it")
	}
}

func TestCheckoutRefusesRatherThanOverwriteAGradersFiles(t *testing.T) {
	// V14 and V15 together, and the reason --no-overwrite-ignore is passed: git
	// does not list an ignored file in `status --porcelain`, so nothing upstream
	// of the checkout can see it, and a plain checkout replaces it in silence.
	requireGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "w")
	runGit(t, base, "init", "-q", "--bare", bare)
	runGit(t, base, "clone", "-q", bare, work)

	// The first commit ignores out/. The second tracks a file inside it, plus an
	// ordinary untracked-to-be file, which is the situation P18 describes.
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "f1"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "ignore")
	runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	first := runGit(t, work, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(work, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{"out/result.txt": "student\n", "notes.txt": "student\n"} {
		if err := os.WriteFile(filepath.Join(work, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, work, "add", "-f", "out/result.txt", "notes.txt")
	runGit(t, work, "commit", "-qm", "result")
	runGit(t, work, "push", "-q", "origin", "HEAD:main")

	clone := filepath.Join(base, "c")
	runGit(t, base, "clone", "-q", "--no-tags", "file://"+bare, clone)
	runGit(t, clone, "checkout", "-q", "--detach", first)

	// The grader's own output lands at both paths.
	if err := os.MkdirAll(filepath.Join(clone, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"out/result.txt", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(clone, path), []byte("grader\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Only the untracked one is visible: the ignored file is invisible to every
	// check collect could make before the checkout.
	st, err := (execGit{}).WorktreeState(context.Background(), clone)
	if err != nil {
		t.Fatalf("WorktreeState: %v", err)
	}
	if st.modified != 0 || st.untracked != 1 {
		t.Errorf("expected no modified and one untracked file, got %+v", st)
	}

	err = (execGit{}).Checkout(context.Background(), clone, "origin/main")
	if err == nil {
		t.Fatal("the checkout must refuse rather than overwrite the grader's files")
	}
	if !filesInTheWay(err) {
		t.Errorf("the refusal should be recognised as files in the way, got: %v", err)
	}
	// Nothing moved, and both of the grader's files are as they were.
	if head := runGit(t, clone, "rev-parse", "HEAD"); head != first {
		t.Errorf("HEAD moved despite the refusal: %s", head)
	}
	for _, path := range []string{"out/result.txt", "notes.txt"} {
		body, readErr := os.ReadFile(filepath.Join(clone, path))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(body) != "grader\n" {
			t.Errorf("%s was overwritten with %q", path, body)
		}
	}
}

func TestFetchNeverImportsAStudentsTag(t *testing.T) {
	// P6: the tag prefix was documented as one that "never collides with a
	// student's own tags", which is not true of a namespace anyone can push to.
	// A student who pushes gh-cls/collect/final at a commit of their choosing
	// used to have it imported, found by the next run, reported up-to-date, and
	// written into the manifest as the commit that was graded.
	requireGit(t)
	origin, work := newOrigin(t, 2)
	base := t.TempDir()

	// Two clones made the way older versions of collect made them, before the
	// student pushes any tag. These stand in for the clones already on an
	// instructor's disk: they carry no tag settings, so the flag on the fetch is
	// the only thing protecting them.
	legacyHole := filepath.Join(base, "legacy-hole")
	legacyFixed := filepath.Join(base, "legacy-fixed")
	runGit(t, base, "clone", "-q", "--depth", "1", origin, legacyHole)
	runGit(t, base, "clone", "-q", "--depth", "1", origin, legacyFixed)

	// The student tags the tip in collect's namespace. The tip is what matters:
	// a depth-1 clone follows only the tags pointing into the history it
	// actually fetched, so a tag further back is not imported and forges
	// nothing.
	runGit(t, work, "tag", "gh-cls/collect/final", "HEAD")
	runGit(t, work, "push", "-q", "origin", "gh-cls/collect/final")

	// A clone that takes tags picks it straight up. This is the hole, and it has
	// to be demonstrated or the fix below proves nothing.
	taken := filepath.Join(base, "taken")
	runGit(t, base, "clone", "-q", "--depth", "1", origin, taken)
	if tags := runGit(t, taken, "tag", "-l"); tags != "gh-cls/collect/final" {
		t.Fatalf("a plain clone should have imported the student tag, got %q", tags)
	}

	// The clone collect makes must not.
	kept := filepath.Join(base, "kept")
	runGit(t, base, "clone", "-q", "--depth", "1", "--no-tags", origin, kept)
	if tags := runGit(t, kept, "tag", "-l"); tags != "" {
		t.Errorf("a --no-tags clone must import no tags, got %q", tags)
	}
	// The flag also sticks: clone --no-tags records remote.origin.tagOpt, and a
	// later plain fetch in that clone still takes no tags. A clone collect makes
	// is protected twice over; the legacy clones below have only the flag on the
	// fetch, which is why that is tested separately.
	if opt := runGit(t, kept, "config", "--get", "remote.origin.tagOpt"); opt != "--no-tags" {
		t.Errorf("a --no-tags clone should record tagOpt for later fetches, got %q", opt)
	}

	// Now the fetch path, on the two legacy clones. The student moves the tag
	// onto a new tip, which a fetch does download, so a tag would be followed
	// here if anything were going to follow one.
	studentCommit(t, work, "c3")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "tag", "-f", "gh-cls/collect/final", "HEAD")
	runGit(t, work, "push", "-qf", "origin", "gh-cls/collect/final")

	// A fetch that follows tags takes it, which is what shows the tag is
	// reachable in this setup at all and the assertion below is not vacuous.
	runGit(t, legacyHole, "fetch", "-q", "--tags", "--depth", "1", "origin", "main")
	if tags := runGit(t, legacyHole, "tag", "-l"); tags != "gh-cls/collect/final" {
		t.Fatalf("a tag-following fetch should have taken the student tag, got %q", tags)
	}

	// Collect's fetch does not. Note that naming a ref on the command line
	// already suppresses tag auto-following, so this held before --no-tags was
	// passed too: on the fetch the flag is defence in depth, and states the
	// guarantee rather than leaving it resting on that incidental behaviour.
	// The hole P6 describes is on the clone above, where it was real.
	if _, err := (execGit{}).Fetch(context.Background(), legacyFixed, "main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if tags := runGit(t, legacyFixed, "tag", "-l"); tags != "" {
		t.Errorf("collect's fetch must import no tags into an older clone, got %q", tags)
	}
}
