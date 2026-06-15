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
