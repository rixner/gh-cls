package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
)

// newThawOpts wires thawOpts to a freeze fake; the roster/groups files live in a
// temp dir and the config comes from assignGlobals.
func newThawOpts(t *testing.T, fake *fakeFreezeState, rosterCSV, groupsYML string, dryRun bool) *thawOpts {
	t.Helper()
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.csv")
	if err := os.WriteFile(rosterPath, []byte(rosterCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	groupsPath := ""
	if groupsYML != "" {
		groupsPath = filepath.Join(dir, "groups.yml")
		if err := os.WriteFile(groupsPath, []byte(groupsYML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fk := fake.fake()
	return &thawOpts{
		g:         assignGlobals(),
		roster:    rosterPath,
		groups:    groupsPath,
		dryRun:    dryRun,
		newClient: func(context.Context) (thawClient, error) { return fk, nil },
	}
}

// thawFake is a frozen hw1: ada and alan at read, plus a collaborator the roster
// does not name and an admin on ada's repo.
func thawFake() *fakeFreezeState {
	fake := freezeFake("admin")
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull"), collab("zed", "pull"), collab("prof", "admin")}
	fake.frozen["hw1-ada"] = freezeFrozen
	fake.frozen["hw1-alan"] = freezeFrozen
	return fake
}

// TestThawRestoresOnlyEnrolledStudents is the divergence from freeze --undo,
// which gave push to every non-admin collaborator it found. Thaw grants only to
// the students the roster puts on each repo.
func TestThawRestoresOnlyEnrolledStudents(t *testing.T) {
	fake := thawFake()
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, want := range []string{"hw1-ada:ada=push", "hw1-alan:alan=push"} {
		if !contains(fake.changes, want) {
			t.Errorf("missing %s in %v", want, fake.changes)
		}
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "zed") || strings.Contains(c, "prof") {
			t.Errorf("only enrolled students should be changed: %v", fake.changes)
		}
		if strings.Contains(c, "project-x") {
			t.Errorf("only hw1-* repos should be processed: %v", fake.changes)
		}
	}
	for _, want := range []string{"hw1-ada=false", "hw1-alan=false"} {
		if !contains(fake.recorded, want) {
			t.Errorf("each thawed repo should be recorded thawed: %v", fake.recorded)
		}
	}
}

// TestThawLeavesDroppedStudentsFrozen checks a student the roster marks as
// dropped keeps read, and their repo stays recorded frozen.
func TestThawLeavesDroppedStudentsFrozen(t *testing.T) {
	fake := thawFake()
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan,read\n"
	o := newThawOpts(t, fake, roster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "alan") {
			t.Errorf("a dropped student must not be thawed: %v", fake.changes)
		}
	}
	if fake.frozen["hw1-alan"] != freezeFrozen {
		t.Errorf("a dropped student's repo should stay recorded frozen: %v", fake.frozen)
	}
	if !strings.Contains(buf.String(), "leaving 1 repo(s) frozen, with no enrolled student or group on them:\n  hw1-alan") {
		t.Errorf("the repo left frozen should be listed:\n%s", buf.String())
	}
}

// TestThawLeavesARepoNotInTheRosterFrozen checks a repo whose key is in neither
// file is not thawed: no one the roster names is on it.
func TestThawLeavesARepoNotInTheRosterFrozen(t *testing.T) {
	fake := thawFake()
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-zzz"})
	fake.collabs["hw1-zzz"] = []gh.Collaborator{collab("zzz", "pull")}
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "zzz") {
			t.Errorf("a repo not in the roster must not be thawed: %v", fake.changes)
		}
	}
	if !strings.Contains(buf.String(), "hw1-zzz") {
		t.Errorf("the repo left frozen should be listed:\n%s", buf.String())
	}
}

// TestThawRestoresPendingInvitations ports the --undo test: an extension for a
// student who has not accepted must put their invitation back to write, or it
// grants them nothing. An invitation for someone the roster does not name stays.
func TestThawRestoresPendingInvitations(t *testing.T) {
	fake := thawFake()
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationRead), invite(8, "stranger", gh.InvitationRead)}
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-alan:alan=invite:write") {
		t.Errorf("the enrolled student's invitation should be restored to write: %v", fake.changes)
	}
	if contains(fake.changes, "hw1-alan:stranger=invite:write") {
		t.Errorf("an invitation the roster does not name must be left alone: %v", fake.changes)
	}
	if !strings.Contains(buf.String(), "1 pending invitation(s) restored to write") {
		t.Errorf("the summary should report the invitation:\n%s", buf.String())
	}
}

// TestThawKeyRecordsThawedNotUnset ports the --undo test: an extension records
// false rather than clearing the value, so "never frozen" and "deliberately
// thawed" stay distinguishable, and an unnamed repo's record does not change.
func TestThawKeyRecordsThawedNotUnset(t *testing.T) {
	fake := thawFake()
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", []string{"ADA"}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !contains(fake.recorded, "hw1-ada=false") {
		t.Errorf("the named repo should be recorded thawed: %v", fake.recorded)
	}
	if fake.frozen["hw1-alan"] != freezeFrozen {
		t.Errorf("an unnamed repo's record must not change: %v", fake.frozen)
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "alan") {
			t.Errorf("an unnamed repo must be left alone: %v", fake.changes)
		}
	}
	if !strings.Contains(buf.String(), "Thawing 1 repo(s)") {
		t.Errorf("should report a single repo:\n%s", buf.String())
	}
}

// TestThawClosesTheAcceptanceRace ports the --undo test: alan accepts a read
// invitation as thaw runs, and the collaborator pass afterwards must grant him
// push, or his extension gives him nothing.
func TestThawClosesTheAcceptanceRace(t *testing.T) {
	fake := thawFake()
	fake.collabs["hw1-alan"] = nil
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationRead)}
	fake.acceptOnUpdate = map[int64]string{7: "alan"}
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("the race must not fail the thaw: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-alan:alan=push") {
		t.Errorf("a student who accepted mid-thaw must still get push: %v", fake.changes)
	}
}

// TestThawKeyForADroppedStudentAborts checks an extension cannot be granted to a
// dropped student: the run fails before any change rather than reporting a
// thaw that gave them nothing.
func TestThawKeyForADroppedStudentAborts(t *testing.T) {
	fake := thawFake()
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan,none\n"
	o := newThawOpts(t, fake, roster, "", false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada", "alan"})
	if err == nil || !strings.Contains(err.Error(), "hw1-alan") {
		t.Fatalf("want an error naming hw1-alan, got %v", err)
	}
	if len(fake.changes)+len(fake.recorded) != 0 {
		t.Errorf("nothing should change: changes %v recorded %v", fake.changes, fake.recorded)
	}
}

func TestThawUnknownKeyAborts(t *testing.T) {
	fake := thawFake()
	o := newThawOpts(t, fake, assignRoster, "", false)
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada", "adaa"})
	if err == nil || !strings.Contains(err.Error(), "hw1-adaa") {
		t.Fatalf("unknown key should be an error naming the missing repo, got %v", err)
	}
	if len(fake.changes) != 0 {
		t.Errorf("nothing should change, got %v", fake.changes)
	}
}

// TestThawVerifiesTheGrantTookEffect checks a grant that reports success but
// does not change the permission fails the repo instead of reading as thawed.
func TestThawVerifiesTheGrantTookEffect(t *testing.T) {
	fake := thawFake()
	fake.dontApply = true
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", nil)
	if err == nil {
		t.Fatalf("a thaw that did not take should fail:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "did not take") {
		t.Errorf("the failure should say the thaw did not take:\n%s", buf.String())
	}
}

func TestThawDryRunMakesNoChanges(t *testing.T) {
	fake := thawFake()
	o := newThawOpts(t, fake, assignRoster, "", true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.changes)+len(fake.recorded) != 0 {
		t.Errorf("a dry run changed something: changes %v recorded %v", fake.changes, fake.recorded)
	}
	out := buf.String()
	if !strings.Contains(out, "would change 2 collaborator grant(s)") || strings.Contains(out, " thawed ") {
		t.Errorf("a dry run should report what it would do and never claim it thawed:\n%s", out)
	}
}

// TestThawReportsStudentsWithNoAccess checks an enrolled student with neither
// access nor an invitation is not invited, but is reported with the fix.
func TestThawReportsStudentsWithNoAccess(t *testing.T) {
	fake := thawFake()
	fake.collabs["hw1-alan"] = nil
	o := newThawOpts(t, fake, assignRoster, "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "alan") {
			t.Errorf("thaw must not invite a student with no access: %v", fake.changes)
		}
	}
	if !strings.Contains(buf.String(), "1 enrolled student(s) hold no access") || !strings.Contains(buf.String(), "--renew") {
		t.Errorf("the student with no access should be reported with the fix:\n%s", buf.String())
	}
}

// TestThawGroupGrantsOnlyEnrolledMembers checks a group repo is thawed for its
// enrolled members while a member marked own stays at read.
func TestThawGroupGrantsOnlyEnrolledMembers(t *testing.T) {
	fake := freezeFake("admin")
	fake.repos = []gh.Repo{{Name: "project-group-alpha"}, {Name: "project-group-beta"}}
	fake.collabs = map[string][]gh.Collaborator{
		"project-group-alpha": {collab("ada", "pull"), collab("grace", "pull")},
		"project-group-beta":  {collab("alan", "pull")},
	}
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan\nstudent-003,grace,own\n"
	o := newThawOpts(t, fake, roster, assignGroups, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, want := range []string{"project-group-alpha:ada=push", "project-group-beta:alan=push"} {
		if !contains(fake.changes, want) {
			t.Errorf("missing %s in %v", want, fake.changes)
		}
	}
	if contains(fake.changes, "project-group-alpha:grace=push") {
		t.Errorf("a dropped group member must not be thawed: %v", fake.changes)
	}
}

func TestThawAbortsWithoutTheFreezeProperty(t *testing.T) {
	fake := thawFake()
	fake.noProperty = true
	o := newThawOpts(t, fake, assignRoster, "", false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "gh cls setup") {
		t.Fatalf("want a setup error, got %v", err)
	}
	if len(fake.changes) != 0 {
		t.Errorf("nothing should change: %v", fake.changes)
	}
}

func TestThawOwnerGuard(t *testing.T) {
	fake := thawFake()
	fake.role = "member"
	o := newThawOpts(t, fake, assignRoster, "", false)
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err == nil {
		t.Fatal("a non-owner should be refused")
	}
	if len(fake.changes) != 0 {
		t.Errorf("nothing should change: %v", fake.changes)
	}
}
