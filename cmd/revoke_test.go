package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
)

// TestAuditRevokeReadsEachRepoOnce checks --revoke reads every repo of the
// assignment once for all the dropped students together, and reads a repo again
// only to change and verify it.
func TestAuditRevokeReadsEachRepoOnce(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{pushCollab("ada")}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "push")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collabWith("grace", "push")}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if n := count(fake.listed, "hw1-ada"); n != 1 {
		t.Errorf("a repo with nothing to revoke should be read once, got %d: %v", n, fake.listed)
	}
	// alan keeps read: downgraded in place. grace keeps nothing: removed.
	if !contains(fake.added, "hw1-alan:alan:pull") {
		t.Errorf("alan not downgraded to read: %v", fake.added)
	}
	if !contains(fake.removed, "hw1-grace:grace") {
		t.Errorf("grace not removed: %v", fake.removed)
	}
	out := buf.String()
	for _, want := range []string{
		"Revoking access for 2 dropped student(s)",
		"student-002", "student-003",
		"revoked hw1-alan alan (now read)",
		"revoked hw1-grace grace (now removed)",
		"revoked access for 2 dropped student(s), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestAuditRevokeOwnOnGroupRemoves checks own removes a dropped student from a
// group repo, cancelling a pending invitation as well as the collaborator, and
// leaves the enrolled member alone.
func TestAuditRevokeOwnOnGroupRemoves(t *testing.T) {
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan\nstudent-003,grace,own\n"
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"project-group-alpha": true, "project-group-beta": true}
	fake.collabs["project-group-alpha"] = []gh.Collaborator{pushCollab("ada"), collabWith("grace", "pull")}
	inv := pendingInvite(9, "grace")
	inv.Permissions = gh.InvitationRead
	fake.invites["project-group-alpha"] = []gh.Invitation{inv}
	o := newAuditOpts(t, fake, roster, assignGroups)
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project"); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !contains(fake.deleted, "project-group-alpha:9") {
		t.Errorf("grace's invitation not cancelled: %v", fake.deleted)
	}
	if !contains(fake.removed, "project-group-alpha:grace") {
		t.Errorf("grace not removed: %v", fake.removed)
	}
	if len(fake.removed) != 1 || len(fake.added) != 0 {
		t.Errorf("only grace should change: removed %v, added %v", fake.removed, fake.added)
	}
	if n := count(fake.listed, "project-group-beta"); n != 1 {
		t.Errorf("a repo with no dropped student should be read once, got %d: %v", n, fake.listed)
	}
}

// TestAuditRevokeOwnOnIndividualKeepsRead checks own leaves a dropped student
// read on their individual repo.
func TestAuditRevokeOwnOnIndividualKeepsRead(t *testing.T) {
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan,own\n"
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "push")}
	o := newAuditOpts(t, fake, roster, "")
	o.revoke = true

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.added, "hw1-alan:alan:pull") || len(fake.removed) != 0 {
		t.Errorf("own on an individual repo should downgrade to read: added %v, removed %v", fake.added, fake.removed)
	}
}

// TestAuditRevokeDowngradesPendingInvitation checks a pending write invitation
// for a student keeping read is reduced to read rather than cancelled.
func TestAuditRevokeDowngradesPendingInvitation(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true}
	inv := pendingInvite(7, "alan")
	inv.Permissions = gh.InvitationWrite
	fake.invites["hw1-alan"] = []gh.Invitation{inv}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !contains(fake.updated, "hw1-alan:7:read") {
		t.Errorf("invitation not downgraded to read: %v", fake.updated)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("a student keeping read should keep the invitation: deleted %v", fake.deleted)
	}
}

// TestAuditRevokeLeavesSettledStudentsAlone checks a dropped student already
// within their access is not touched.
func TestAuditRevokeLeavesSettledStudentsAlone(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "pull")}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.added)+len(fake.removed)+len(fake.deleted)+len(fake.updated) != 0 {
		t.Errorf("settled students changed: added %v removed %v deleted %v updated %v", fake.added, fake.removed, fake.deleted, fake.updated)
	}
	if !strings.Contains(buf.String(), "nothing to revoke") {
		t.Errorf("expected nothing to revoke:\n%s", buf.String())
	}
}

// TestAuditRevokeDryRun checks a dry run prints the plan and changes nothing.
func TestAuditRevokeDryRun(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "push")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collabWith("grace", "push")}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true
	o.dryRun = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.added)+len(fake.removed)+len(fake.deleted)+len(fake.updated) != 0 {
		t.Errorf("dry run changed something: added %v removed %v", fake.added, fake.removed)
	}
	out := buf.String()
	for _, want := range []string{"[dry-run] Revoking access for 2", "would revoke hw1-alan alan (now read)", "would revoke hw1-grace grace (now removed)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestAuditRevokeRequiresADroppedStudent checks --revoke against a roster that
// marks no one fails rather than quietly doing nothing, since that almost always
// means the wrong roster file.
func TestAuditRevokeRequiresADroppedStudent(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.revoke = true

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "is marked as dropped") {
		t.Fatalf("want a no-dropped-students error, got %v", err)
	}
	// The roster alone shows there is nothing to revoke, so the run stops before
	// its first request.
	if len(fake.fk.Calls) != 0 {
		t.Errorf("no request should be made: %v", fake.fk.Calls)
	}
}

// TestAuditRevokeRefusesAdmin checks a dropped student holding admin aborts the
// run before any change, since gh cls never changes admin access.
func TestAuditRevokeRefusesAdmin(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "admin")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collabWith("grace", "push")}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "alan on hw1-alan") || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("want an admin refusal naming alan, got %v", err)
	}
	if len(fake.removed)+len(fake.added) != 0 {
		t.Errorf("nothing should change before the refusal: removed %v added %v", fake.removed, fake.added)
	}
}

// TestAuditRevokeAbortsOnAuditError checks a repo that cannot be read aborts the
// run before any change.
func TestAuditRevokeAbortsOnAuditError(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collabWith("grace", "push")}
	fake.listErr["hw1-alan"] = true
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "aborting --revoke") {
		t.Fatalf("want an abort, got %v", err)
	}
	if len(fake.removed) != 0 {
		t.Errorf("nothing should change: removed %v", fake.removed)
	}
}

// TestAuditRevokeVerifiesResult checks a removal that reports success but does
// not take is reported as a failure, not a clean run.
func TestAuditRevokeVerifiesResult(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-grace": true}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collabWith("grace", "push")}
	fake.sticky = map[string]bool{"grace": true}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil {
		t.Fatalf("a revoke that did not take should fail:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "did not take") {
		t.Errorf("failure should say the revoke did not take:\n%s", buf.String())
	}
}

// TestAuditRevokeHandlesAcceptanceMidRun checks a student who accepts their
// invitation just before it is cancelled is still removed, as a collaborator.
func TestAuditRevokeHandlesAcceptanceMidRun(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-grace": true}
	inv := pendingInvite(5, "grace")
	inv.Permissions = gh.InvitationWrite
	fake.invites["hw1-grace"] = []gh.Invitation{inv}
	fake.acceptOnDelete = map[string]bool{"grace": true}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !contains(fake.removed, "hw1-grace:grace") {
		t.Errorf("grace accepted mid-run and was not removed: %v", fake.removed)
	}
}

// TestAuditRevokeFindsADroppedStudentOutOfTheirGroup is the case that first
// failed in use: students marked none and taken out of their groups were never
// looked for, since only the groups file said which repo was theirs, and
// --revoke reported that no one was marked at all.
func TestAuditRevokeFindsADroppedStudentOutOfTheirGroup(t *testing.T) {
	roster := "identifier,username,access\nstudent-001,ada\nstudent-002,alan\nstudent-003,grace,none\nstudent-004,katherine,none\n"
	// grace and katherine are in no group; grace still holds write on alpha and
	// katherine a pending invitation to beta.
	groups := "group-alpha: [student-001]\ngroup-beta: [student-002]\n"
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"project-group-alpha": true, "project-group-beta": true}
	fake.collabs["project-group-alpha"] = []gh.Collaborator{pushCollab("ada"), collabWith("grace", "push")}
	fake.collabs["project-group-beta"] = []gh.Collaborator{pushCollab("alan")}
	inv := pendingInvite(4, "katherine")
	inv.Permissions = gh.InvitationWrite
	fake.invites["project-group-beta"] = []gh.Invitation{inv}
	o := newAuditOpts(t, fake, roster, groups)
	o.revoke = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project"); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !contains(fake.removed, "project-group-alpha:grace") {
		t.Errorf("grace not removed: %v", fake.removed)
	}
	if !contains(fake.deleted, "project-group-beta:4") {
		t.Errorf("katherine's invitation not cancelled: %v", fake.deleted)
	}
	if len(fake.removed) != 1 || len(fake.added) != 0 {
		t.Errorf("enrolled members must not change: removed %v, added %v", fake.removed, fake.added)
	}
}

// TestAuditRevokeIgnoresTemplates checks --revoke does not read a template
// repository, which is never a student's.
func TestAuditRevokeIgnoresTemplates(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-alan": true, "hw1-starter": true}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collabWith("alan", "push")}
	fk := fake.fake()
	fk.ListOrgReposByPrefixFunc = func(context.Context, string, string) ([]gh.Repo, error) {
		return []gh.Repo{{Name: "hw1-alan"}, {Name: "hw1-starter", IsTemplate: true}}, nil
	}
	o := newAuditOpts(t, fake, auditDroppedRoster, "")
	o.newClient = func(context.Context) (auditClient, error) { return fk, nil }
	o.revoke = true

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
		t.Fatal(err)
	}
	if contains(fake.listed, "hw1-starter") {
		t.Errorf("a template should not be read: %v", fake.listed)
	}
}
