package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
)

// fakeReady is a minimal repoReady stand-in for the readiness poll.
type fakeReady struct {
	repo      *gh.Repo
	exists    bool
	getErr    error
	branchOK  bool
	branchErr error
	getCalls  int
}

func (f *fakeReady) GetRepo(context.Context, string, string) (*gh.Repo, bool, error) {
	f.getCalls++
	return f.repo, f.exists, f.getErr
}

func (f *fakeReady) BranchExists(context.Context, string, string, string) (bool, error) {
	return f.branchOK, f.branchErr
}

func TestWaitRepoReadySucceeds(t *testing.T) {
	f := &fakeReady{repo: &gh.Repo{Name: "hw1-ada", DefaultBranch: "main"}, exists: true, branchOK: true}
	r, err := waitRepoReady(context.Background(), f, func(time.Duration) {}, "org", "hw1-ada")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "hw1-ada" {
		t.Errorf("got %+v", r)
	}
}

func TestWaitRepoReadySurfacesGetError(t *testing.T) {
	// A real error from GetRepo must be reported, not hidden behind the generic
	// "did not become ready" timeout after exhausting the poll attempts.
	f := &fakeReady{getErr: errors.New("403 forbidden")}
	_, err := waitRepoReady(context.Background(), f, func(time.Duration) {}, "org", "hw1-ada")
	if err == nil || !strings.Contains(err.Error(), "403 forbidden") {
		t.Fatalf("the underlying error should surface, got %v", err)
	}
	if f.getCalls != 1 {
		t.Errorf("a hard error should abort polling immediately, got %d GetRepo calls", f.getCalls)
	}
}

func TestWaitRepoReadySurfacesBranchError(t *testing.T) {
	f := &fakeReady{repo: &gh.Repo{DefaultBranch: "main"}, exists: true, branchErr: errors.New("boom")}
	_, err := waitRepoReady(context.Background(), f, func(time.Duration) {}, "org", "hw1-ada")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a branch-check error should surface, got %v", err)
	}
}

func TestWaitRepoReadyTimesOutWhenUnpopulated(t *testing.T) {
	// No error, but the default branch never lands: this is the genuine timeout.
	f := &fakeReady{repo: &gh.Repo{DefaultBranch: "main"}, exists: true, branchOK: false}
	_, err := waitRepoReady(context.Background(), f, func(time.Duration) {}, "org", "hw1-ada")
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("an unpopulated repo should time out, got %v", err)
	}
	if f.getCalls != readyAttempts {
		t.Errorf("should poll all %d attempts, got %d", readyAttempts, f.getCalls)
	}
}

func TestWaitRepoReadyLooksAfterWaiting(t *testing.T) {
	// Generation is never instant: across real runs the immediate check failed
	// every time and the next one succeeded, so looking first only ever spent a
	// repo read and a ref read to learn nothing.
	f := &fakeReady{repo: &gh.Repo{Name: "hw1-ada", DefaultBranch: "main"}, exists: true, branchOK: true}
	var waited []time.Duration
	if _, err := waitRepoReady(context.Background(), f, func(d time.Duration) { waited = append(waited, d) }, "org", "hw1-ada"); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != readyDelay {
		t.Errorf("waits = %v, want one wait before the first look", waited)
	}
	if f.getCalls != 1 {
		t.Errorf("read the repo %d times, want once now that the wait comes first", f.getCalls)
	}
}
