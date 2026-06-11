// Package git prepares a squashed template by flattening a source repository's
// working tree into a single commit, using the system git binary.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// commitEnv pins the single starter commit's author and committer to a neutral
// identity. This keeps the instructor's git identity out of student repositories
// and makes the squash reproducible regardless of the host's git config.
var commitEnv = []string{
	"GIT_AUTHOR_NAME=gh cls",
	"GIT_AUTHOR_EMAIL=gh-cls@users.noreply.github.com",
	"GIT_COMMITTER_NAME=gh cls",
	"GIT_COMMITTER_EMAIL=gh-cls@users.noreply.github.com",
}

// Squash shallow-clones srcURL, discards its history, builds a single commit of
// the working tree carrying message, and pushes that commit to pushURL. It
// returns the branch name pushed (the source's default branch).
//
// Authentication for clone and push is left to the user's git credential helper
// (gh configures one via `gh auth setup-git`), so this code never handles a
// token.
func Squash(ctx context.Context, srcURL, pushURL, message string) (string, error) {
	work, err := os.MkdirTemp("", "gh-cls-template-*")
	if err != nil {
		return "", fmt.Errorf("creating work directory: %w", err)
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")

	if _, err := git(ctx, "", nil, "clone", "--depth", "1", srcURL, src); err != nil {
		return "", fmt.Errorf("shallow-cloning source template: %w", err)
	}

	branch, err := git(ctx, src, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading source default branch: %w", err)
	}
	branch = strings.TrimSpace(branch)

	// Drop the cloned history and rebuild a single-commit repository from the
	// working tree, so none of the source template's commits are exposed.
	if err := os.RemoveAll(filepath.Join(src, ".git")); err != nil {
		return "", fmt.Errorf("removing source history: %w", err)
	}
	steps := [][]string{
		{"init", "-q", "-b", branch},
		{"add", "-A"},
		{"commit", "-q", "-m", message},
		{"remote", "add", "origin", pushURL},
		{"push", "-q", "origin", branch},
	}
	for _, args := range steps {
		if _, err := git(ctx, src, commitEnv, args...); err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return branch, nil
}

// git runs the git binary in dir (the current directory if dir is empty) with
// extra environment, returning trimmed stdout. On failure the error includes
// git's stderr.
func git(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if extraEnv != nil {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
