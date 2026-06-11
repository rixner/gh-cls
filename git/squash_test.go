package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs git for test setup with a fixed identity so commits succeed
// regardless of host config.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), commitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSquashFlattensHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "dest.git")

	// A source repo with multiple commits and a couple of files.
	runGit(t, "", "init", "-q", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-q", "-m", "first")
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-q", "-m", "second")

	// A bare repo to receive the push.
	runGit(t, "", "init", "-q", "--bare", "-b", "main", dst)

	branch, err := Squash(context.Background(), src, dst, "Provided starter code")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}

	// The destination must have exactly one commit.
	if count := runGit(t, "", "--git-dir", dst, "rev-list", "--count", "main"); count != "1" {
		t.Errorf("commit count = %s, want 1 (history not squashed)", count)
	}
	if msg := runGit(t, "", "--git-dir", dst, "log", "-1", "--format=%s", "main"); msg != "Provided starter code" {
		t.Errorf("commit message = %q", msg)
	}
	// Both files from the working tree must be present.
	tree := runGit(t, "", "--git-dir", dst, "ls-tree", "-r", "--name-only", "main")
	for _, f := range []string{"a.txt", "b.txt"} {
		if !strings.Contains(tree, f) {
			t.Errorf("file %q missing from squashed tree:\n%s", f, tree)
		}
	}
}
