package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeArchiveState configures a ghtest.Fake for archive tests and captures what
// it observed.
type fakeArchiveState struct {
	role      string
	repos     map[string]*gh.Repo
	listErr   map[string]bool // prefixes whose listing fails
	dontApply bool            // accept the archive call but leave the repo unarchived
	archived  []string
	calls     int
}

func newFakeArchive() *fakeArchiveState {
	s := &fakeArchiveState{role: "admin", repos: map[string]*gh.Repo{}, listErr: map[string]bool{}}
	for _, n := range []string{"hw1-ada", "hw1-alan", "project-group-alpha"} {
		s.repos[n] = &gh.Repo{Name: n}
	}
	s.repos["hw1-template"] = &gh.Repo{Name: "hw1-template", IsTemplate: true}
	// A template the config does not name, excluded only by GitHub's flag.
	s.repos["hw1-starter"] = &gh.Repo{Name: "hw1-starter", IsTemplate: true}
	return s
}

func (s *fakeArchiveState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) {
		fk.Lock()
		defer fk.Unlock()
		s.calls++
		return s.role, nil
	}
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.listErr[prefix] {
			return nil, fmt.Errorf("listing failed for %s", prefix)
		}
		var out []gh.Repo
		for name, r := range s.repos {
			if strings.HasPrefix(name, prefix) {
				out = append(out, *r)
			}
		}
		return out, nil
	}
	fk.ArchiveRepoFunc = func(_ context.Context, _, name string) error {
		fk.Lock()
		defer fk.Unlock()
		s.archived = append(s.archived, name)
		if !s.dontApply {
			s.repos[name].Archived = true
		}
		return nil
	}
	fk.GetRepoFunc = func(_ context.Context, _, name string) (*gh.Repo, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		r, ok := s.repos[name]
		if !ok {
			return nil, false, nil
		}
		cp := *r
		return &cp, true, nil
	}
	return fk
}

func newArchiveOpts(fake *fakeArchiveState, all, dryRun bool) *archiveOpts {
	fk := fake.fake()
	return &archiveOpts{
		g:         assignGlobals(),
		all:       all,
		dryRun:    dryRun,
		newClient: func(context.Context) (archiveClient, error) { return fk, nil },
	}
}

func TestArchiveAllArchivesEveryStudentRepo(t *testing.T) {
	fake := newFakeArchive()
	o := newArchiveOpts(fake, true, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, want := range []string{"hw1-ada", "hw1-alan", "project-group-alpha"} {
		if !contains(fake.archived, want) {
			t.Errorf("%s not archived: %v", want, fake.archived)
		}
	}
	if contains(fake.archived, "hw1-template") || contains(fake.archived, "hw1-starter") {
		t.Errorf("a template repository must not be archived: %v", fake.archived)
	}
	if !strings.Contains(buf.String(), "archived 3 repo(s), 0 failed") {
		t.Errorf("summary wrong:\n%s", buf.String())
	}
}

func TestArchiveNamedAssignmentOnly(t *testing.T) {
	fake := newFakeArchive()
	o := newArchiveOpts(fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, []string{"hw1"}); err != nil {
		t.Fatal(err)
	}
	if contains(fake.archived, "project-group-alpha") || len(fake.archived) != 2 {
		t.Errorf("only hw1's repos should be archived: %v", fake.archived)
	}
}

// TestArchiveRequiresNamesOrAll checks the whole course is never archived by
// default, and naming assignments alongside --all is refused, both before any
// request.
func TestArchiveRequiresNamesOrAll(t *testing.T) {
	for name, tc := range map[string]struct {
		all   bool
		names []string
		want  string
	}{
		"neither": {false, nil, "--all"},
		"both":    {true, []string{"hw1"}, "not both"},
	} {
		fake := newFakeArchive()
		o := newArchiveOpts(fake, tc.all, false)
		err := o.run(context.Background(), &bytes.Buffer{}, tc.names)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, tc.want, err)
		}
		if fake.calls != 0 {
			t.Errorf("%s: no request should be made", name)
		}
	}
}

func TestArchiveUnknownAssignmentAborts(t *testing.T) {
	fake := newFakeArchive()
	o := newArchiveOpts(fake, false, false)
	err := o.run(context.Background(), &bytes.Buffer{}, []string{"hw1", "hw9"})
	if err == nil || !strings.Contains(err.Error(), "hw9") {
		t.Fatalf("want an error naming hw9, got %v", err)
	}
	if len(fake.archived) != 0 {
		t.Errorf("nothing should be archived: %v", fake.archived)
	}
}

// TestArchiveNamedAssignmentWithNoReposAborts checks a named assignment with no
// repos stops the run before anything is archived, even for the other names.
func TestArchiveNamedAssignmentWithNoReposAborts(t *testing.T) {
	fake := newFakeArchive()
	delete(fake.repos, "project-group-alpha")
	o := newArchiveOpts(fake, false, false)
	err := o.run(context.Background(), &bytes.Buffer{}, []string{"hw1", "project"})
	if err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("want an error naming project, got %v", err)
	}
	if len(fake.archived) != 0 {
		t.Errorf("nothing should be archived: %v", fake.archived)
	}
}

// TestArchiveAllToleratesAnAssignmentWithNoRepos checks --all does not fail on
// an assignment that was never handed out.
func TestArchiveAllToleratesAnAssignmentWithNoRepos(t *testing.T) {
	fake := newFakeArchive()
	delete(fake.repos, "project-group-alpha")
	o := newArchiveOpts(fake, true, false)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "no repos for project") {
		t.Errorf("the empty assignment should be noted:\n%s", buf.String())
	}
}

// TestArchiveListingFailureAborts checks every listing is read before anything
// is archived, so one failure leaves every repo as it was.
func TestArchiveListingFailureAborts(t *testing.T) {
	fake := newFakeArchive()
	fake.listErr["project-"] = true
	o := newArchiveOpts(fake, true, false)
	err := o.run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was archived") {
		t.Fatalf("want a listing error, got %v", err)
	}
	if len(fake.archived) != 0 {
		t.Errorf("nothing should be archived: %v", fake.archived)
	}
}

func TestArchiveSkipsAlreadyArchived(t *testing.T) {
	fake := newFakeArchive()
	fake.repos["hw1-ada"].Archived = true
	o := newArchiveOpts(fake, false, false)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, []string{"hw1"}); err != nil {
		t.Fatal(err)
	}
	if contains(fake.archived, "hw1-ada") {
		t.Errorf("an archived repo should be left alone: %v", fake.archived)
	}
	if !strings.Contains(buf.String(), "1 repo(s) already archived") {
		t.Errorf("the skip should be reported:\n%s", buf.String())
	}
}

func TestArchiveVerifiesItTookEffect(t *testing.T) {
	fake := newFakeArchive()
	fake.dontApply = true
	o := newArchiveOpts(fake, false, false)
	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, []string{"hw1"})
	if err == nil {
		t.Fatalf("an archive that did not take should fail:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "did not take") {
		t.Errorf("the failure should say it did not take:\n%s", buf.String())
	}
}

func TestArchiveDryRunChangesNothing(t *testing.T) {
	fake := newFakeArchive()
	o := newArchiveOpts(fake, true, true)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.archived) != 0 {
		t.Errorf("a dry run archived %v", fake.archived)
	}
	if !strings.Contains(buf.String(), "[dry-run] Archiving 3 repo(s)") || !strings.Contains(buf.String(), "would archive 3 repo(s)") {
		t.Errorf("dry run output wrong:\n%s", buf.String())
	}
}

func TestArchiveOwnerGuard(t *testing.T) {
	fake := newFakeArchive()
	fake.role = "member"
	o := newArchiveOpts(fake, true, false)
	if err := o.run(context.Background(), &bytes.Buffer{}, nil); err == nil {
		t.Fatal("a non-owner should be refused")
	}
	if len(fake.archived) != 0 {
		t.Errorf("nothing should be archived: %v", fake.archived)
	}
}
