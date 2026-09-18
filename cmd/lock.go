package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runLock is one run's exclusive claim on an --out directory.
//
// Two runs collecting into the same directory race on the clones and the tags,
// and both read the manifest before appending to it, so rows can be duplicated.
// The claim is a file created exclusively: the filesystem decides the winner, so
// there is no window between asking whether a run is in progress and becoming
// that run.
type runLock struct{ path string }

// acquireRunLock claims <out>/.gh-cls/lock for this run. The error names who
// holds it and how to clear it, since the common case for a refusal is a lock a
// killed run left behind rather than a run genuinely in progress.
func acquireRunLock(out string) (*runLock, error) {
	dir := filepath.Join(out, gitCLSDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("another collect is already running in %s:\n%s\n"+
			"wait for it to finish. If no such run is going (the machine restarted, or it was killed), "+
			"delete %s and re-run", out, indent(readLockHolder(path)), path)
	}
	if err != nil {
		return nil, fmt.Errorf("claiming %s: %w", path, err)
	}

	host, hostErr := os.Hostname()
	if hostErr != nil {
		host = "unknown host"
	}
	_, writeErr := fmt.Fprintf(f, "host: %s\npid: %d\nstarted: %s\n",
		host, os.Getpid(), time.Now().Format(time.RFC3339))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		// The claim is held but says nothing about who holds it, which would
		// leave the next run unable to report anything useful. Give it up.
		_ = os.Remove(path)
		return nil, fmt.Errorf("recording this run in %s: %w", path, errors.Join(writeErr, closeErr))
	}
	return &runLock{path: path}, nil
}

// release gives up the claim. A run that dies without releasing leaves the file,
// which is what the refusal explains how to clear.
func (l *runLock) release() error {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("releasing %s: %w", l.path, err)
	}
	return nil
}

// readLockHolder returns what the lock file says, for a refusal to quote. A file
// that cannot be read still refuses the run: it exists, so someone claimed it.
func readLockHolder(path string) string {
	body, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(body)) == "" {
		return "(the lock file says nothing about who holds it)"
	}
	return strings.TrimRight(string(body), "\n")
}

// indent offsets a quoted block so it reads as one inside a message.
func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
