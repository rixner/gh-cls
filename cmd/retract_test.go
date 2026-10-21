package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// retractDay is the date every retract test runs on, so backup names are fixed.
var retractDay = time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local)

// fakeRetractState configures a ghtest.Fake for retract tests and captures what
// it observed. Repos are keyed by name; a rename moves the entry, so later
// lookups of the old name find nothing, as on GitHub once the redirect is
// recognized.
type fakeRetractState struct {
	role     string
	repos    map[string]*gh.Repo
	collabs  map[string][]gh.Collaborator
	invites  map[string][]gh.Invitation
	activity map[string][]gh.Activity

	listErr      bool
	activityErr  map[string]bool // repos whose activity cannot be read
	inviteGone   map[string]bool // invitations answer 404 on delete (accepted in flight)
	removeNoop   bool            // accept RemoveCollaborator but change nothing
	renameErr    map[string]bool // repos whose rename fails
	renameKeeps  bool            // the rename lands but the old name still answers
	removed      []string        // repo:login
	cancelled    []string        // repo:login
	renamed      map[string]string
	mutationLog  []string // every mutating call, repo-tagged, in order
	mutatedRepos map[string]bool
}

func newFakeRetract() *fakeRetractState {
	s := &fakeRetractState{
		role:         "admin",
		repos:        map[string]*gh.Repo{},
		collabs:      map[string][]gh.Collaborator{},
		invites:      map[string][]gh.Invitation{},
		activity:     map[string][]gh.Activity{},
		activityErr:  map[string]bool{},
		inviteGone:   map[string]bool{},
		renameErr:    map[string]bool{},
		renamed:      map[string]string{},
		mutatedRepos: map[string]bool{},
	}
	for _, n := range []string{"hw1-ada", "hw1-alan", "project-group-alpha"} {
		s.repos[n] = &gh.Repo{Name: n}
	}
	s.repos["hw1-template"] = &gh.Repo{Name: "hw1-template", IsTemplate: true}

	s.collabs["hw1-ada"] = []gh.Collaborator{rcollab("ada", false), rcollab("prof", true)}
	s.invites["hw1-alan"] = []gh.Invitation{rinvite(7, "alan", false), rinvite(8, "olduser", true)}
	s.collabs["project-group-alpha"] = []gh.Collaborator{rcollab("grace", false)}
	return s
}

func rcollab(login string, admin bool) gh.Collaborator {
	var c gh.Collaborator
	c.Login = login
	c.Permissions.Admin = admin
	c.Permissions.Push = true
	c.Permissions.Pull = true
	return c
}

func rinvite(id int64, login string, expired bool) gh.Invitation {
	var inv gh.Invitation
	inv.ID = id
	inv.Invitee.Login = login
	inv.Expired = expired
	return inv
}

func push(login string, at time.Time) gh.Activity {
	var a gh.Activity
	a.ActivityType = gh.ActivityPush
	a.Ref = "refs/heads/main"
	a.Timestamp = at
	a.Actor.Login = login
	return a
}

func (s *fakeRetractState) mutate(what, repo string) {
	s.mutationLog = append(s.mutationLog, what+" "+repo)
	s.mutatedRepos[repo] = true
}

func (s *fakeRetractState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.listErr {
			return nil, fmt.Errorf("listing failed")
		}
		var out []gh.Repo
		for name, r := range s.repos {
			if strings.HasPrefix(name, prefix) {
				out = append(out, *r)
			}
		}
		return out, nil
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
	fk.ListDirectCollaboratorsFunc = func(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
		fk.Lock()
		defer fk.Unlock()
		return append([]gh.Collaborator(nil), s.collabs[repo]...), nil
	}
	fk.ListRepoInvitationsFunc = func(_ context.Context, _, repo string) ([]gh.Invitation, error) {
		fk.Lock()
		defer fk.Unlock()
		return append([]gh.Invitation(nil), s.invites[repo]...), nil
	}
	fk.ListRepoActivityFunc = func(_ context.Context, _, repo, ref string) ([]gh.Activity, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.activityErr[repo] {
			return nil, fmt.Errorf("activity unavailable")
		}
		return s.activity[repo], nil
	}
	fk.RemoveCollaboratorFunc = func(_ context.Context, _, repo, login string) error {
		fk.Lock()
		defer fk.Unlock()
		s.mutate("remove", repo)
		s.removed = append(s.removed, repo+":"+login)
		if s.removeNoop {
			return nil
		}
		var kept []gh.Collaborator
		for _, c := range s.collabs[repo] {
			if c.Login != login {
				kept = append(kept, c)
			}
		}
		s.collabs[repo] = kept
		return nil
	}
	fk.DeleteRepoInvitationFunc = func(_ context.Context, _, repo string, id int64) error {
		fk.Lock()
		defer fk.Unlock()
		s.mutate("cancel", repo)
		var kept []gh.Invitation
		for _, inv := range s.invites[repo] {
			if inv.ID == id {
				s.cancelled = append(s.cancelled, repo+":"+inv.Invitee.Login)
				continue
			}
			kept = append(kept, inv)
		}
		s.invites[repo] = kept
		if s.inviteGone[repo] {
			return &api.HTTPError{StatusCode: http.StatusNotFound}
		}
		return nil
	}
	fk.RenameRepoFunc = func(_ context.Context, _, name, newName string) error {
		fk.Lock()
		defer fk.Unlock()
		s.mutate("rename", name)
		if s.renameErr[name] {
			return fmt.Errorf("rename refused")
		}
		r := s.repos[name]
		cp := *r
		cp.Name = newName
		s.repos[newName] = &cp
		if !s.renameKeeps {
			delete(s.repos, name)
		}
		s.renamed[name] = newName
		return nil
	}
	return fk
}

func newRetractOpts(fake *fakeRetractState, dryRun bool) (*retractOpts, *ghtest.Fake) {
	fk := fake.fake()
	return &retractOpts{
		g:         assignGlobals(),
		dryRun:    dryRun,
		now:       func() time.Time { return retractDay },
		newClient: func(context.Context) (retractClient, error) { return fk, nil },
	}, fk
}

func TestRetractRemovesAccessThenRenames(t *testing.T) {
	fake := newFakeRetract()
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()

	want := map[string]string{"hw1-ada": "hw1_20261021-ada", "hw1-alan": "hw1_20261021-alan"}
	for old, backup := range want {
		if fake.renamed[old] != backup {
			t.Errorf("%s renamed to %q, want %q", old, fake.renamed[old], backup)
		}
		if _, still := fake.repos[old]; still {
			t.Errorf("%s should no longer exist under its old name", old)
		}
	}
	if len(fake.renamed) != 2 {
		t.Errorf("only hw1's student repos should be renamed (not the template or project's): %v", fake.renamed)
	}
	if !contains(fake.removed, "hw1-ada:ada") || contains(fake.removed, "hw1-ada:prof") {
		t.Errorf("the student should be removed and the admin left alone: %v", fake.removed)
	}
	// The expired invitation is cancelled too: it grants nothing, but there is no
	// reason to leave it on the backup.
	if !contains(fake.cancelled, "hw1-alan:alan") || !contains(fake.cancelled, "hw1-alan:olduser") {
		t.Errorf("every pending invitation should be cancelled: %v", fake.cancelled)
	}

	// Per repo, every access change precedes the rename: a repo must never sit
	// under its backup name with a student still able to push to it.
	for _, repo := range []string{"hw1-ada", "hw1-alan"} {
		renamedAt := -1
		for i, m := range fake.mutationLog {
			if m == "rename "+repo {
				renamedAt = i
			}
		}
		for i, m := range fake.mutationLog {
			if strings.HasSuffix(m, " "+repo) && !strings.HasPrefix(m, "rename") && i > renamedAt {
				t.Errorf("%q came after %s was renamed: %v", m, repo, fake.mutationLog)
			}
		}
	}

	for _, line := range []string{
		"Retracting 2 repo(s) of hw1 in cs101-spring26, each renamed to hw1_20261021-<key>",
		"alan (invited), olduser (invited)",
		"note: hw1-ada: prof hold(s) admin, which retract leaves alone, so they keep access to the backup",
		"retracted 2 repo(s), 0 failed",
		"The backups are hw1_20261021-*. Only staff can reach them, and they are not archived.",
		"Next: point assignments.hw1.template at the right template, run `gh cls assign hw1`, and tell students to clone their repo again.",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q:\n%s", line, out)
		}
	}
}

// TestRetractReportsStudentPushes checks the per-repo push report counts only the
// students' changes, so assign's own feedback-branch work and staff pushes do not
// make an untouched repo look like it holds student work.
func TestRetractReportsStudentPushes(t *testing.T) {
	fake := newFakeRetract()
	t1 := time.Date(2026, 10, 19, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 20, 15, 30, 0, 0, time.UTC)
	fake.activity["hw1-ada"] = []gh.Activity{
		push("prof", t2.Add(time.Hour)), // staff, newest: not student work
		push("ada", t2),
		push("ada", t1),
	}
	fake.activity["hw1-alan"] = []gh.Activity{push("prof", t1)}
	o, _ := newRetractOpts(fake, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	var adaLine, alanLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "hw1-ada ") {
			adaLine = l
		}
		if strings.Contains(l, "hw1-alan ") {
			alanLine = l
		}
	}
	if !strings.Contains(adaLine, "2, last 2026-10-20T15:30:00Z by ada") {
		t.Errorf("ada's row should count her two pushes and name the latest:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(alanLine), "none") {
		t.Errorf("a repo only staff pushed to should report no student pushes:\n%s", out)
	}
}

func TestRetractSummarizesBackupsWithPushes(t *testing.T) {
	fake := newFakeRetract()
	fake.activity["hw1-ada"] = []gh.Activity{push("ada", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))}
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "1 backup(s) hold student pushes; see STUDENT PUSHES above.") {
		t.Errorf("the summary should count the backups holding student work:\n%s", buf.String())
	}
}

func TestRetractDryRunChangesNothing(t *testing.T) {
	fake := newFakeRetract()
	o, _ := newRetractOpts(fake, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if len(fake.mutationLog) != 0 {
		t.Errorf("a dry run must change nothing: %v", fake.mutationLog)
	}
	out := buf.String()
	for _, line := range []string{"[dry-run] Retracting 2 repo(s)", "hw1_20261021-ada", "[dry-run] would retract 2 repo(s); nothing was changed"} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q:\n%s", line, out)
		}
	}
}

// TestRetractPreflightAbortsBeforeAnyChange checks every way the run can be
// doomed is found while reading, so it stops having changed nothing.
func TestRetractPreflightAbortsBeforeAnyChange(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fakeRetractState, *retractOpts)
		want  string
	}{
		"backup name taken": {
			func(s *fakeRetractState, _ *retractOpts) {
				s.repos["hw1_20261021-alan"] = &gh.Repo{Name: "hw1_20261021-alan"}
			},
			"hw1-alan: the backup name cs101-spring26/hw1_20261021-alan is already taken",
		},
		"backup inside another assignment's namespace": {
			func(_ *fakeRetractState, o *retractOpts) {
				o.g.cfg.Assignments["hw1_20261021"] = config.Assignment{Type: config.TypeIndividual}
			},
			`falls under assignment "hw1_20261021"'s hw1_20261021-* repositories`,
		},
		"backup name too long": {
			func(s *fakeRetractState, _ *retractOpts) {
				long := "hw1-" + strings.Repeat("x", 90)
				s.repos[long] = &gh.Repo{Name: long}
			},
			"over GitHub's limit of 100",
		},
		"archived repo": {
			func(s *fakeRetractState, _ *retractOpts) { s.repos["hw1-alan"].Archived = true },
			"hw1-alan is archived",
		},
		"unreadable activity": {
			func(s *fakeRetractState, _ *retractOpts) { s.activityErr["hw1-ada"] = true },
			"hw1-ada: reading who has pushed: activity unavailable",
		},
		"failed listing": {
			func(s *fakeRetractState, _ *retractOpts) { s.listErr = true },
			"listing hw1-* repositories",
		},
		"no repos": {
			func(s *fakeRetractState, _ *retractOpts) {
				delete(s.repos, "hw1-ada")
				delete(s.repos, "hw1-alan")
			},
			"no student repositories found for hw1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeRetract()
			o, _ := newRetractOpts(fake, false)
			tc.setup(fake, o)
			err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "nothing was retracted") {
				t.Errorf("the error should say nothing changed: %v", err)
			}
			if len(fake.mutationLog) != 0 {
				t.Errorf("a doomed run must change nothing: %v", fake.mutationLog)
			}
		})
	}
}

func TestRetractRefusesUnknownAssignmentAndNonOwner(t *testing.T) {
	fake := newFakeRetract()
	o, fk := newRetractOpts(fake, false)
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw9"); err == nil || !strings.Contains(err.Error(), `assignment "hw9" not found`) {
		t.Errorf("an unknown assignment should be refused, got %v", err)
	}
	if len(fk.Calls) != 0 {
		t.Errorf("an unknown assignment should be refused before any request: %v", fk.Calls)
	}

	fake.role = "member"
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err == nil || !strings.Contains(err.Error(), "organization owner") {
		t.Errorf("a non-owner should be refused, got %v", err)
	}
	if len(fake.mutationLog) != 0 {
		t.Errorf("a non-owner run must change nothing: %v", fake.mutationLog)
	}
}

// TestRetractNeverRenamesAReposStudentsCanStillReach checks a repo whose access
// removal did not take is left under its old name, where every command still
// sees it, rather than renamed out of view with a student still on it.
func TestRetractNeverRenamesAReposStudentsCanStillReach(t *testing.T) {
	fake := newFakeRetract()
	fake.removeNoop = true
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "re-run `gh cls retract hw1`") {
		t.Fatalf("want a failure telling the user to re-run, got %v", err)
	}
	if _, renamed := fake.renamed["hw1-ada"]; renamed {
		t.Error("hw1-ada still has a student on it and must not be renamed")
	}
	if fake.renamed["hw1-alan"] != "hw1_20261021-alan" {
		t.Errorf("hw1-alan's access came off, so it should still be retracted: %v", fake.renamed)
	}
	if !strings.Contains(buf.String(), "FAILED hw1-ada (some of its students' access may be removed; it was not renamed): removing access to hw1-ada did not take: ada is still a collaborator") {
		t.Errorf("the failure should say what state the repo was left in:\n%s", buf.String())
	}
}

func TestRetractReportsAFailedRename(t *testing.T) {
	fake := newFakeRetract()
	fake.renameErr["hw1-ada"] = true
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err == nil {
		t.Fatal("a failed rename should fail the run")
	}
	if !strings.Contains(buf.String(), "FAILED hw1-ada (its students' access is gone; it was not renamed): renaming hw1-ada to hw1_20261021-ada: rename refused") {
		t.Errorf("the failure should say access is gone but the rename is left:\n%s", buf.String())
	}

	// Re-running finishes it: hw1-alan has left hw1-* and is not touched again.
	fake.renameErr["hw1-ada"] = false
	fake.mutationLog = nil
	buf.Reset()
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("re-run: %v\n%s", err, buf.String())
	}
	if fake.renamed["hw1-ada"] != "hw1_20261021-ada" {
		t.Errorf("the re-run should finish hw1-ada: %v", fake.renamed)
	}
	for _, m := range fake.mutationLog {
		if strings.HasSuffix(m, " hw1-alan") {
			t.Errorf("the re-run touched the already-retracted hw1-alan: %v", fake.mutationLog)
		}
	}
}

// TestRetractChecksTheOldNameIsFree checks the post-condition assign depends on:
// if the old name still answers after the rename, assign would find the backup
// there and grant students access to it.
func TestRetractChecksTheOldNameIsFree(t *testing.T) {
	fake := newFakeRetract()
	fake.renameKeeps = true
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err == nil {
		t.Fatal("a rename that leaves the old name answering should fail the run")
	}
	if !strings.Contains(buf.String(), "renaming hw1-ada did not free its name") {
		t.Errorf("the failure should name the post-condition:\n%s", buf.String())
	}
}

// TestRetractToleratesAnInvitationAcceptedInFlight checks a 404 cancelling an
// invitation (accepted between listing and cancelling) is not a failure.
func TestRetractToleratesAnInvitationAcceptedInFlight(t *testing.T) {
	fake := newFakeRetract()
	fake.inviteGone["hw1-alan"] = true
	o, _ := newRetractOpts(fake, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if fake.renamed["hw1-alan"] != "hw1_20261021-alan" {
		t.Errorf("hw1-alan should be retracted: %v", fake.renamed)
	}
}

// TestRetractedBackupIsInvisibleToAssignmentCommands checks the backup name is
// outside the assignment's namespace, so freeze, collect, archive and assign do
// not pick it up as a student repo.
func TestRetractedBackupIsInvisibleToAssignmentCommands(t *testing.T) {
	fake := newFakeRetract()
	o, fk := newRetractOpts(fake, false)
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	repos, err := fk.ListOrgReposByPrefix(context.Background(), "cs101-spring26", "hw1-")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range filterAssignmentRepos(o.g.cfg, "hw1", repos) {
		if !r.IsTemplate {
			t.Errorf("%s is still in hw1's namespace after retract", r.Name)
		}
	}
}
