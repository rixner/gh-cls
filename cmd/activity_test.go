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

func TestActivityPinTakesTheLastCommitBeforeUntil(t *testing.T) {
	fake := oneRepo()
	o := newActivityOpts(fake)
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"
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
		t.Errorf("pin file should name the pre-deadline commit:\n%s", body)
	}
	if strings.Contains(string(body), "ddd") {
		t.Errorf("a post-deadline push must not be pinned:\n%s", body)
	}
	if !contains(fake.verified, "ccc") {
		t.Errorf("the pinned SHA should be verified retrievable: %v", fake.verified)
	}
}

func TestActivityPinRefusesWhenTheRecordIsBehind(t *testing.T) {
	// GitHub documents no ingestion latency for this endpoint, so the command
	// checks rather than assumes: if the branch's current tip is absent from the
	// record, the record is stale and a pin taken from it could be wrong.
	fake := oneRepo()
	fake.tips["hw1-ada"] = "eee" // a push the record has not caught up with
	o := newActivityOpts(fake)
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("a stale record must not be pinned from:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "behind") {
		t.Errorf("the failure should say the record is behind:\n%s", buf.String())
	}
}

func TestActivityPinRefusesAnOrphanedCommit(t *testing.T) {
	// A force push can remove the commit that was the tip at the deadline. Writing
	// it would produce a pin file collect cannot use, so it is excluded and the
	// run fails rather than handing back a broken artifact.
	fake := oneRepo()
	fake.gone["ccc"] = true
	o := newActivityOpts(fake)
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"
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
		t.Error("no pin file should be written when nothing could be pinned")
	}
}

func TestActivityPinReportsAnUnpinnableRepo(t *testing.T) {
	// A student who never pushed before the deadline is named, not silently
	// omitted, matching how collect reports a student with no repo.
	fake := oneRepo()
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-alan", DefaultBranch: "main"})
	fake.tips["hw1-alan"] = ""
	fake.events["hw1-alan"] = nil
	o := newActivityOpts(fake)
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "no activity in the window") || !strings.Contains(buf.String(), "alan") {
		t.Errorf("the unpinnable student should be named:\n%s", buf.String())
	}
}

func TestActivityPinIgnoresDeletionsAsTips(t *testing.T) {
	// A branch deletion's After is all zeroes: pinning it would write a SHA that
	// is not a commit at all.
	fake := oneRepo()
	fake.events["hw1-ada"] = append(fake.events["hw1-ada"],
		act(gh.ActivityBranchDeletion, "main", "ccc", strings.Repeat("0", 40), "ada", at("2026-03-01T23:30:00Z")))
	o := newActivityOpts(fake)
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"
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
	o.forced, o.deleted = true, true
	o.branch = "" // default branch only, so the scratch deletion is out of scope

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "Force pushes: 1") {
		t.Errorf("the force push should be listed:\n%s", out)
	}
	// The deletion is on another branch, so reporting on main must exclude it.
	if !strings.Contains(out, "Branch deletions: 0") {
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
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"
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
	tests := map[string]struct{ since, until, want string }{
		"bad since":    {"nonsense", "", "not an RFC3339 time"},
		"bad until":    {"", "nonsense", "not an RFC3339 time"},
		"until before": {"2026-03-02T00:00:00Z", "2026-03-01T00:00:00Z", "is before"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			o := newActivityOpts(oneRepo())
			o.since, o.until = tc.since, tc.until
			err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestActivityOutRequiresOneMode(t *testing.T) {
	// A pin file and a force-push listing are different artifacts, so there is no
	// sensible way to write both to one file.
	for _, tc := range []struct{ pin, forced bool }{{true, true}, {false, false}} {
		o := newActivityOpts(oneRepo())
		o.pin, o.forced = tc.pin, tc.forced
		o.out = filepath.Join(t.TempDir(), "x")
		err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("pin=%v forced=%v should be rejected, got %v", tc.pin, tc.forced, err)
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
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"
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
	o.since = "2027-01-01T00:00:00Z" // a window after every event
	o.until = "2027-02-01T00:00:00Z"

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

func TestActivityPinPrintsTheMappingWithoutOut(t *testing.T) {
	// -o is a redirect, not the only route to the result: a pin run with no file
	// must still show what it pinned, in the same form the file uses.
	o := newActivityOpts(oneRepo())
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "ada: ccc") {
		t.Errorf("the pin mapping should be printed when there is no --out:\n%s", buf.String())
	}
}

func TestActivityPinReportsForcePushesOnBothSides(t *testing.T) {
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
	o.pin = true
	o.until = "2026-03-01T23:59:59Z"

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "within the window") || !strings.Contains(out, "alan") {
		t.Errorf("a force push before the pin should be reported:\n%s", out)
	}
	if !strings.Contains(out, "AFTER the pinned instant") || !strings.Contains(out, "ada") {
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
	o.forced = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Force pushes: 0 in 0 of 3 repos examined") {
		t.Errorf("an empty listing should still show what it examined:\n%s", buf.String())
	}
}

func TestActivityAllListsEveryChangeWithItsActor(t *testing.T) {
	// The question this exists for: "who has been pushing to this repo". -f and -d
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
	o.all, o.forced = true, true
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
