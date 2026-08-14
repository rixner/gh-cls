package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// act builds one activity record.
func act(atype, branch, before, after, actor string, ts time.Time) gh.Activity {
	a := gh.Activity{
		ActivityType: atype,
		Ref:          "refs/heads/" + branch,
		Before:       before,
		After:        after,
		Timestamp:    ts,
	}
	a.Actor.Login = actor
	return a
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeActivityState configures a ghtest.Fake for activity tests.
type fakeActivityState struct {
	repos []gh.Repo
	// events per repo name, in any order (the command sorts).
	events map[string][]gh.Activity
	// tips per repo name: the branch's current commit.
	tips map[string]string
	// gone lists SHAs CommitExists reports as unretrievable.
	gone map[string]bool

	verified []string // SHAs passed to CommitExists
}

func (s *fakeActivityState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		var out []gh.Repo
		for _, r := range s.repos {
			if strings.HasPrefix(r.Name, prefix) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	fk.ListRepoActivityFunc = func(_ context.Context, _, repo, _ string) ([]gh.Activity, error) {
		fk.Lock()
		defer fk.Unlock()
		return append([]gh.Activity(nil), s.events[repo]...), nil
	}
	fk.GetRefFunc = func(_ context.Context, _, repo, _ string) (string, error) {
		fk.Lock()
		defer fk.Unlock()
		return s.tips[repo], nil
	}
	fk.CommitExistsFunc = func(_ context.Context, _, _, sha string) (bool, error) {
		fk.Lock()
		defer fk.Unlock()
		s.verified = append(s.verified, sha)
		return !s.gone[sha], nil
	}
	return fk
}

func newActivityOpts(fake *fakeActivityState) *activityOpts {
	fk := fake.fake()
	return &activityOpts{
		g:         assignGlobals(),
		now:       func() time.Time { return at("2026-04-01T00:00:00Z") },
		newClient: func(context.Context) (activityClient, error) { return fk, nil },
	}
}

// oneRepo is a single-repo fixture whose main branch was pushed twice before a
// deadline and once after.
func oneRepo() *fakeActivityState {
	return &fakeActivityState{
		repos: []gh.Repo{{Name: "hw1-ada", DefaultBranch: "main"}},
		events: map[string][]gh.Activity{
			"hw1-ada": {
				act(gh.ActivityPush, "main", "aaa", "bbb", "ada", at("2026-03-01T10:00:00Z")),
				act(gh.ActivityPush, "main", "bbb", "ccc", "ada", at("2026-03-01T23:00:00Z")),
				act(gh.ActivityPush, "main", "ccc", "ddd", "ada", at("2026-03-02T09:00:00Z")), // after the deadline
			},
		},
		tips: map[string]string{"hw1-ada": "ddd"},
		gone: map[string]bool{},
	}
}

func TestActivitySnapshotTakesTheLastCommitBeforeTo(t *testing.T) {
	fake := oneRepo()
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = filepath.Join(t.TempDir(), "deadline.yml")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	body, err := os.ReadFile(o.out)
	if err != nil {
		t.Fatal(err)
	}
	// ccc is the last push at or before the deadline; ddd landed after it.
	if !strings.Contains(string(body), "ada: ccc") {
		t.Errorf("the snapshot should name the pre-deadline commit:\n%s", body)
	}
	if strings.Contains(string(body), "ddd") {
		t.Errorf("a post-deadline push must not be pinned:\n%s", body)
	}
	if !contains(fake.verified, "ccc") {
		t.Errorf("the recorded SHA should be verified retrievable: %v", fake.verified)
	}
}

func TestActivitySnapshotRefusesWhenTheRecordIsBehind(t *testing.T) {
	// GitHub documents no ingestion latency for this endpoint, so the command
	// checks rather than assumes: if the branch's current tip is absent from the
	// record, the record is stale and a pin taken from it could be wrong.
	fake := oneRepo()
	fake.tips["hw1-ada"] = "eee" // a push the record has not caught up with
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("a stale record must not be recorded from:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "behind") {
		t.Errorf("the failure should say the record is behind:\n%s", buf.String())
	}
}

func TestActivitySnapshotRefusesAnOrphanedCommit(t *testing.T) {
	// A force push can remove the commit that was the tip at the deadline. Writing
	// it would produce a snapshot collect cannot use, so it is excluded and the
	// run fails rather than handing back a broken artifact.
	fake := oneRepo()
	fake.gone["ccc"] = true
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = filepath.Join(t.TempDir(), "deadline.yml")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("an orphaned commit must fail the run:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "no longer retrievable") {
		t.Errorf("the report should name the unretrievable commit:\n%s", buf.String())
	}
	if _, statErr := os.Stat(o.out); statErr == nil {
		t.Error("no snapshot should be written when nothing could be recorded")
	}
}

// twoRepos is oneRepo plus a second student whose main branch was pushed once
// before the deadline. Both are pinnable.
func twoRepos() *fakeActivityState {
	s := oneRepo()
	s.repos = append(s.repos, gh.Repo{Name: "hw1-alan", DefaultBranch: "main"})
	s.events["hw1-alan"] = []gh.Activity{
		act(gh.ActivityPush, "main", "111", "222", "alan", at("2026-03-01T12:00:00Z")),
	}
	s.tips["hw1-alan"] = "222"
	return s
}

func TestActivitySnapshotWritesTheFileWhenEveryRepoPasses(t *testing.T) {
	fake := twoRepos()
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = filepath.Join(t.TempDir(), "deadline.yml")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	body, err := os.ReadFile(o.out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ada: ccc", "alan: 222"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the snapshot should contain %q:\n%s", want, body)
		}
	}
}

func TestActivitySnapshotRefusesAPartialFile(t *testing.T) {
	// COLLECT.md promises the freshness and orphan checks "fail the run rather
	// than handing back an artifact". A file holding only the repos that passed
	// looks complete: collect takes it at face value, and the students left out
	// appear only as "skipped (not in the snapshot)", which is what a student who never
	// pushed looks like too. So one failure blocks the file.
	cases := map[string]struct {
		setup func(*fakeActivityState)
		want  string // the reason the blocked repo is reported with
		next  string // the way forward, which differs by what blocked it
	}{
		"the record is behind on one repo": {
			func(s *fakeActivityState) { s.tips["hw1-alan"] = "999" },
			"is behind",
			"re-run once GitHub's record has caught up",
		},
		"one repo's commit was orphaned": {
			// A lagging record clears on its own; an orphaned commit never does, so
			// telling the user to retry would send them in a circle.
			func(s *fakeActivityState) { s.gone["222"] = true },
			"no longer retrievable",
			"use -w to see the force pushes",
		},
	}
	for name, tc := range cases {
		fake := twoRepos()
		tc.setup(fake)
		o := newActivityOpts(fake)
		o.snapshot = true
		o.to = "2026-03-01T23:59:59Z"
		o.out = filepath.Join(t.TempDir(), "deadline.yml")

		var buf bytes.Buffer
		err := o.run(context.Background(), &buf, "hw1")
		if err == nil {
			t.Errorf("%s: the run should fail:\n%s", name, buf.String())
			continue
		}
		if !strings.Contains(err.Error(), "refusing to write "+o.out) {
			t.Errorf("%s: the error should name the file it refused to write, got %v", name, err)
		}
		if !strings.Contains(err.Error(), tc.next) {
			t.Errorf("%s: the error should offer %q, got %v", name, tc.next, err)
		}
		if _, statErr := os.Stat(o.out); statErr == nil {
			t.Errorf("%s: no snapshot should be written when a repo could not be recorded", name)
		}
		out := buf.String()
		// The blocked repo is named with its reason, since the error itself points
		// back to the report rather than repeating it.
		if !strings.Contains(out, "alan") || !strings.Contains(out, tc.want) {
			t.Errorf("%s: the blocking repo and reason should be reported:\n%s", name, out)
		}
		// The repos that did pin are still shown, so the run is not a dead end: their
		// lines can be copied into a hand-written snapshot file.
		if !strings.Contains(out, "ada: ccc") {
			t.Errorf("%s: the repos that did pin should still be printed:\n%s", name, out)
		}
	}
}

func TestActivitySnapshotReportsARepoWithNothingToRecord(t *testing.T) {
	// A student who never pushed before the deadline is named, not silently
	// omitted, matching how collect reports a student with no repo.
	fake := oneRepo()
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-alan", DefaultBranch: "main"})
	fake.tips["hw1-alan"] = ""
	fake.events["hw1-alan"] = nil
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "no activity in the window") || !strings.Contains(buf.String(), "alan") {
		t.Errorf("the unpinnable student should be named:\n%s", buf.String())
	}
}

func TestActivitySnapshotIgnoresDeletionsAsTips(t *testing.T) {
	// A branch deletion's After is all zeroes: pinning it would write a SHA that
	// is not a commit at all.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityBranchDeletion, "main", "ccc", strings.Repeat("0", 40), "ada", at("2026-03-01T23:30:00Z")))
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = filepath.Join(t.TempDir(), "d.yml")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	body, _ := os.ReadFile(o.out)
	if !strings.Contains(string(body), "ada: ccc") || strings.Contains(string(body), "0000") {
		t.Errorf("a deletion must not be pinned as a tip:\n%s", body)
	}
}

func TestActivityListsForcePushesAndDeletions(t *testing.T) {
	// The reason these exist: an assignment's branch-protection ruleset would
	// block both, but needs a paid plan for private repos. On a free org this is
	// the only way to see them.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityForcePush, "main", "ccc", "fff", "ada", at("2026-03-01T20:00:00Z")),
		act(gh.ActivityBranchDeletion, "scratch", "ggg", strings.Repeat("0", 40), "ada", at("2026-03-01T21:00:00Z")))
	o := newActivityOpts(fake)
	o.rewrites = true
	o.branch = "" // default branch only, so the scratch deletion is out of scope

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	// One listing covers both kinds; the WHAT column tells them apart.
	if !strings.Contains(out, "Force pushes and branch deletions: 1") || !strings.Contains(out, "force_push") {
		t.Errorf("the force push should be listed:\n%s", out)
	}
	// The deletion is on another branch, so reporting on main must exclude it.
	if strings.Contains(out, "branch_deletion") {
		t.Errorf("a deletion on another branch is out of scope:\n%s", out)
	}
}

func TestActivityFiltersByBranchClientSide(t *testing.T) {
	// The ref parameter is sent, but an ignored filter would silently mix other
	// branches in, which for -p would pin a commit from the wrong branch. The
	// fake returns everything regardless of the ref, standing in for that.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityPush, "feature/models", "xxx", "yyy", "ada", at("2026-03-01T23:30:00Z")))
	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = filepath.Join(t.TempDir(), "b.yml")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	body, _ := os.ReadFile(o.out)
	// yyy is newer than ccc but on another branch, so ccc must still win.
	if !strings.Contains(string(body), "ada: ccc") {
		t.Errorf("a push on another branch must not be pinned:\n%s", body)
	}
}

func TestActivityWindowValidation(t *testing.T) {
	tests := map[string]struct{ from, to, want string }{
		"bad from":       {"nonsense", "", "not an RFC3339 time"},
		"bad to":         {"", "nonsense", "not an RFC3339 time"},
		"to before from": {"2026-03-02T00:00:00Z", "2026-03-01T00:00:00Z", "is before"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			o := newActivityOpts(oneRepo())
			o.from, o.to = tc.from, tc.to
			err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestActivityOutRequiresOneMode(t *testing.T) {
	// A snapshot and a rewrite listing are different artifacts, so there is no
	// sensible way to write both to one file.
	for _, tc := range []struct{ snapshot, rewrites bool }{{true, true}, {false, false}} {
		o := newActivityOpts(oneRepo())
		o.snapshot, o.rewrites = tc.snapshot, tc.rewrites
		o.out = filepath.Join(t.TempDir(), "x")
		err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("snapshot=%v rewrites=%v should be rejected, got %v", tc.snapshot, tc.rewrites, err)
		}
	}
}

func TestActivityNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "taken.yml")
	if err := os.WriteFile(existing, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := newActivityOpts(oneRepo())
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"
	o.out = existing

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("an existing --out must not be replaced, got %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Errorf("the existing file was modified: %q", b)
	}
}

func TestActivitySummaryIsTheDefault(t *testing.T) {
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityForcePush, "main", "ccc", "fff", "ada", at("2026-03-01T20:00:00Z")))
	o := newActivityOpts(fake)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"KEY", "PUSHES", "FORCE", "ada"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestActivityUnknownAssignment(t *testing.T) {
	o := newActivityOpts(oneRepo())
	err := o.run(context.Background(), &bytes.Buffer{}, "bogus")
	if err == nil || !strings.Contains(err.Error(), "not found in config") {
		t.Fatalf("an unknown assignment should error, got %v", err)
	}
}

func TestActivityEmptyFieldsUseANeutralPlaceholder(t *testing.T) {
	// audit's dash() renders an empty value as "(not in roster)", which is
	// nonsense for a timestamp or an actor. A repo with no activity in the window
	// must not claim its student is missing from the roster.
	fake := oneRepo()
	o := newActivityOpts(fake)
	o.from = "2027-01-01T00:00:00Z" // a window after every event
	o.to = "2027-02-01T00:00:00Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "not in roster") {
		t.Errorf("an empty activity field must not borrow audit's wording:\n%s", buf.String())
	}
}

func TestActivityNamesABranchThatFoundNothing(t *testing.T) {
	// A mistyped branch otherwise prints a wall of zeroes that reads as a broken
	// report rather than a typo.
	o := newActivityOpts(oneRepo())
	o.branch = "features/models" // the real branch is feature/models

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), `no activity on branch "features/models"`) {
		t.Errorf("an empty branch report should name the branch:\n%s", buf.String())
	}
}

func TestActivitySnapshotPrintsTheMappingWithoutOut(t *testing.T) {
	// -o is a redirect, not the only route to the result: a pin run with no file
	// must still show what it pinned, in the same form the file uses.
	o := newActivityOpts(oneRepo())
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "ada: ccc") {
		t.Errorf("the pin mapping should be printed when there is no --out:\n%s", buf.String())
	}
}

func TestActivitySnapshotReportsForcePushesOnBothSides(t *testing.T) {
	// The two mean different things: before the pin the tip at that instant is
	// still valid, after it the pinned commit may be gone. Both are reported, and
	// they must not be conflated.
	fake := oneRepo()
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-alan", DefaultBranch: "main"})
	fake.tips["hw1-alan"] = "zzz"
	fake.events["hw1-alan"] = []gh.Activity{
		act(gh.ActivityPush, "main", "p", "q", "alan", at("2026-03-01T12:00:00Z")),
		act(gh.ActivityForcePush, "main", "q", "r", "alan", at("2026-03-01T13:00:00Z")), // before the pin
		act(gh.ActivityPush, "main", "r", "zzz", "alan", at("2026-03-05T00:00:00Z")),
	}
	// ada force-pushes after the pinned instant.
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityForcePush, "main", "ddd", "eee", "ada", at("2026-03-03T00:00:00Z")))
	fake.tips["hw1-ada"] = "eee"

	o := newActivityOpts(fake)
	o.snapshot = true
	o.to = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "within the window") || !strings.Contains(out, "alan") {
		t.Errorf("a force push before the pin should be reported:\n%s", out)
	}
	if !strings.Contains(out, "AFTER the snapshot instant") || !strings.Contains(out, "ada") {
		t.Errorf("a force push after the pin should be reported separately:\n%s", out)
	}
}

func TestActivitySummaryOmitsQuietRepos(t *testing.T) {
	// A row of zeroes for every student buries the ones that matter. Only repos
	// with something to report are listed; the rest are counted.
	fake := oneRepo()
	for _, k := range []string{"alan", "grace", "kath"} {
		fake.repos = append(fake.repos, gh.Repo{Name: "hw1-" + k, DefaultBranch: "main"})
		fake.tips["hw1-"+k] = ""
		fake.events["hw1-"+k] = nil
	}
	o := newActivityOpts(fake)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "ada") {
		t.Errorf("the repo with activity should be listed:\n%s", out)
	}
	for _, quiet := range []string{"alan", "grace", "kath"} {
		if strings.Contains(out, quiet) {
			t.Errorf("%s has no activity and should not be listed:\n%s", quiet, out)
		}
	}
	if !strings.Contains(out, "3 repos with no activity, not listed") {
		t.Errorf("the omitted repos should be counted:\n%s", out)
	}
}

func TestActivityListingsReportCoverage(t *testing.T) {
	// A bare "Force pushes: 0" cannot be distinguished from having examined
	// nothing, so the count states how many repos it looked at.
	fake := oneRepo()
	for _, k := range []string{"alan", "grace"} {
		fake.repos = append(fake.repos, gh.Repo{Name: "hw1-" + k, DefaultBranch: "main"})
		fake.tips["hw1-"+k] = ""
		fake.events["hw1-"+k] = nil
	}
	o := newActivityOpts(fake)
	o.rewrites = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Force pushes and branch deletions: 0 in 0 of 3 repos examined") {
		t.Errorf("an empty listing should still show what it examined:\n%s", buf.String())
	}
}

func TestActivityAllListsEveryChangeWithItsActor(t *testing.T) {
	// The question this exists for: "who has been pushing to this repo". -w
	// only show their own kind, so without -a that needs a raw API call.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityForcePush, "main", "ddd", "eee", "someone-else", at("2026-03-03T00:00:00Z")))
	fake.tips["hw1-ada"] = "eee"
	o := newActivityOpts(fake)
	o.all = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "All activity: 4 by 2 actors in 1 of 1 repo examined") {
		t.Errorf("every change should be counted, by actor:\n%s", out)
	}
	// Per actor rather than per change: thousands of individual rows are what
	// --out is for. ada made 3 pushes, someone-else 1 force push.
	for _, want := range []string{"WHO", "TOTAL", "PUSH", "FORCE", "ada", "someone-else"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Only the types that occurred become columns; nothing here merged or deleted.
	for _, absent := range []string{"MERGE", "CREATE", "DELETE", "MQMERGE"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q did not occur and should not be a column:\n%s", absent, out)
		}
	}
	if strings.Contains(out, "WHAT") {
		t.Errorf("-a summarizes by actor; the per-change table belongs in the CSV:\n%s", out)
	}
}

func TestActivityAllShowsAColumnPerTypeThatOccurred(t *testing.T) {
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityBranchDeletion, "main", "ccc", strings.Repeat("0", 40), "ada", at("2026-03-01T21:00:00Z")),
		act(gh.ActivityPRMerge, "main", "ccc", "mmm", "ada", at("2026-03-01T22:00:00Z")))
	o := newActivityOpts(fake)
	o.all = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"PUSH", "MERGE", "DELETE"} {
		if !strings.Contains(out, want) {
			t.Errorf("a type that occurred should be a column, missing %q:\n%s", want, out)
		}
	}
	// Still nothing force-pushed or created a branch here.
	for _, absent := range []string{"FORCE", "CREATE"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q did not occur and should not be a column:\n%s", absent, out)
		}
	}
}

func TestActivityAllKeepsAnUnknownType(t *testing.T) {
	// A type GitHub adds later must still be counted, under its raw name, rather
	// than silently dropped from the tally.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act("teleport", "main", "ccc", "ttt", "ada", at("2026-03-01T22:00:00Z")))
	o := newActivityOpts(fake)
	o.all = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "TELEPORT") {
		t.Errorf("an unrecognized type should appear under its raw name:\n%s", buf.String())
	}
}

func TestActivityAllCountsAsAModeForOut(t *testing.T) {
	o := newActivityOpts(oneRepo())
	o.all, o.rewrites = true, true
	o.out = filepath.Join(t.TempDir(), "x.csv")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("-a with another mode and --out should be rejected, got %v", err)
	}
}

func TestActivityAllWritesCSV(t *testing.T) {
	o := newActivityOpts(oneRepo())
	o.all = true
	o.out = filepath.Join(t.TempDir(), "all.csv")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	recs := readCSV(t, o.out)
	want := []string{"key", "repo", "branch", "timestamp", "activity_type", "actor", "before", "after"}
	if len(recs) != 4 || !equalRow(recs[0], want) {
		t.Fatalf("CSV header/row count wrong: %v", recs)
	}
}

func TestActivityAllSummarizesTerminalButWritesEveryChange(t *testing.T) {
	// The terminal gets one row per actor; the file gets every individual change.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityForcePush, "main", "ddd", "eee", "grace", at("2026-03-03T00:00:00Z")))
	fake.tips["hw1-ada"] = "eee"
	o := newActivityOpts(fake)
	o.all = true
	o.out = filepath.Join(t.TempDir(), "all.csv")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	// Two actors on the terminal, four changes in the file.
	if strings.Count(buf.String(), "hw1-ada") > 0 {
		t.Errorf("the summary is keyed by student key, not repo name:\n%s", buf.String())
	}
	recs := readCSV(t, o.out)
	if len(recs) != 5 { // header + 4 changes
		t.Fatalf("the CSV should hold every change, got %d records", len(recs))
	}
}
