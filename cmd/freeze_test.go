package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// collab builds a Collaborator with a single permission level set, mirroring how
// the API reports effective permissions for our purposes.
func collab(login, level string) gh.Collaborator {
	c := gh.Collaborator{Login: login}
	switch level {
	case "admin":
		c.Permissions.Admin = true
	case "push":
		c.Permissions.Push = true
	case "pull":
		c.Permissions.Pull = true
	}
	return c
}

// invite builds a pending Invitation carrying the given invitation-vocabulary
// permission ("read", "write", ...).
func invite(id int64, login, permission string) gh.Invitation {
	i := gh.Invitation{ID: id, Permissions: permission}
	i.Invitee.Login = login
	return i
}

// fakeFreezeState configures a ghtest.Fake for freeze tests and captures what
// it observed.
type fakeFreezeState struct {
	role      string
	repos     []gh.Repo
	collabs   map[string][]gh.Collaborator
	invites   map[string][]gh.Invitation
	frozen    map[string]freezeState // the recorded freeze state per repo
	dontApply bool                   // record the change but leave the permission unchanged
	// noProperty simulates an org that never ran setup, so freeze cannot record.
	noProperty bool
	// dontRecord accepts the property write but leaves the value unchanged, the
	// way a silently-ignored API call would.
	dontRecord bool
	// acceptOnUpdate maps an invitation id to the login that accepts it the moment
	// freeze tries to update it, simulating the acceptance race.
	acceptOnUpdate map[int64]string

	// changes records access changes only ("repo:user=permission" for
	// collaborators, "repo:user=invite:permission" for invitations). Freeze-record
	// writes go to recorded instead, so tests asserting that a given student's
	// access was untouched are not tripped by a property write naming their repo.
	changes  []string
	recorded []string // "repo=value"
	apiCalls int      // count of calls into the fake, to assert an aborted run made none
}

func (s *fakeFreezeState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) {
		fk.Lock()
		defer fk.Unlock()
		s.apiCalls++
		return s.role, nil
	}
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		fk.Lock()
		defer fk.Unlock()
		var out []gh.Repo
		for _, r := range s.repos {
			if strings.HasPrefix(r.Name, prefix) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	fk.ListDirectCollaboratorsFunc = func(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
		fk.Lock()
		defer fk.Unlock()
		cs := s.collabs[repo]
		out := make([]gh.Collaborator, len(cs))
		copy(out, cs)
		return out, nil
	}
	fk.AddCollaboratorFunc = func(_ context.Context, _, repo, username, permission string) error {
		fk.Lock()
		defer fk.Unlock()
		s.changes = append(s.changes, repo+":"+username+"="+permission)
		if s.dontApply {
			return nil
		}
		// Reflect the new permission so a subsequent re-read (the post-condition
		// verification) sees the change, as the real API would.
		cs := s.collabs[repo]
		for i := range cs {
			if cs[i].Login == username {
				cs[i] = collab(username, permission)
			}
		}
		s.collabs[repo] = cs
		return nil
	}
	fk.ListRepoInvitationsFunc = func(_ context.Context, _, repo string) ([]gh.Invitation, error) {
		fk.Lock()
		defer fk.Unlock()
		return append([]gh.Invitation(nil), s.invites[repo]...), nil
	}
	fk.UpdateRepoInvitationFunc = func(_ context.Context, _, repo string, id int64, permission string) (bool, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.acceptOnUpdate != nil {
			if login, ok := s.acceptOnUpdate[id]; ok {
				// The invitee accepted between the listing and this call: the
				// invitation is consumed and they become a collaborator holding
				// whatever it conferred at that moment, not what we were about to
				// set it to.
				delete(s.acceptOnUpdate, id)
				level := "pull"
				for _, inv := range s.invites[repo] {
					if inv.ID == id && inv.ConfersPush() {
						level = "push"
					}
				}
				s.dropInvite(repo, id)
				s.collabs[repo] = append(s.collabs[repo], collab(login, level))
				return false, nil
			}
		}
		for i, inv := range s.invites[repo] {
			if inv.ID != id {
				continue
			}
			s.changes = append(s.changes, repo+":"+inv.Invitee.Login+"=invite:"+permission)
			if !s.dontApply {
				s.invites[repo][i].Permissions = permission
			}
			return true, nil
		}
		return false, fmt.Errorf("no invitation %d on %s", id, repo)
	}
	fk.GetPropertyDefinitionFunc = func(_ context.Context, _, name string) (*gh.PropertyDefinition, bool, error) {
		if s.noProperty {
			return nil, false, nil
		}
		return &gh.PropertyDefinition{
			PropertyName:     name,
			ValueType:        gh.PropertyTypeTrueFalse,
			ValuesEditableBy: gh.PropertyEditableByOrg,
		}, true, nil
	}
	fk.SetRepoPropertyValueFunc = func(_ context.Context, _, repo, name, value string) error {
		fk.Lock()
		defer fk.Unlock()
		if name != frozenProperty {
			return fmt.Errorf("unexpected property %q on %s", name, repo)
		}
		s.recorded = append(s.recorded, repo+"="+value)
		if !s.dontRecord {
			if s.frozen == nil {
				s.frozen = map[string]freezeState{} // tests may build the fake as a literal
			}
			s.frozen[repo] = freezeState(value)
		}
		return nil
	}
	fk.GetRepoPropertyValuesFunc = func(_ context.Context, _, repo string) (map[string]string, error) {
		fk.Lock()
		defer fk.Unlock()
		return map[string]string{frozenProperty: string(s.frozen[repo])}, nil
	}
	return fk
}

func newFreezeOpts(t *testing.T, fake *fakeFreezeState, undo, dryRun bool) *freezeOpts {
	t.Helper()
	return newFreezeOptsG(t, assignGlobals(), fake, undo, dryRun)
}

func newFreezeOptsG(t *testing.T, g *globalOpts, fake *fakeFreezeState, undo, dryRun bool) *freezeOpts {
	t.Helper()
	fk := fake.fake()
	return &freezeOpts{
		g:         g,
		undo:      undo,
		dryRun:    dryRun,
		newClient: func(context.Context) (freezeClient, error) { return fk, nil },
	}
}

func freezeFake(role string) *fakeFreezeState {
	return &fakeFreezeState{
		role:  role,
		repos: []gh.Repo{{Name: "hw1-ada"}, {Name: "hw1-alan"}, {Name: "project-x"}},
		collabs: map[string][]gh.Collaborator{
			"hw1-ada":  {collab("ada", "push"), collab("prof", "admin")},
			"hw1-alan": {collab("alan", "pull")}, // already frozen
		},
		invites: map[string][]gh.Invitation{},
		frozen:  map[string]freezeState{},
	}
}

func TestFreezeDowngradesNonAdmins(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.changes, "hw1-ada:ada=pull") {
		t.Errorf("student push should be downgraded to pull: %v", fake.changes)
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "prof") {
			t.Error("admins must be left untouched")
		}
		if strings.Contains(c, "alan") {
			t.Error("already-frozen (pull) collaborators should not be touched")
		}
		if strings.Contains(c, "project-x") {
			t.Error("only hw1-* repos should be processed")
		}
	}
}

func TestFreezeUndoRestoresPush(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, true, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	// alan was pull (frozen); undo restores push. ada already has push: untouched.
	if !contains(fake.changes, "hw1-alan:alan=push") {
		t.Errorf("undo should restore push to frozen collaborators: %v", fake.changes)
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "ada") {
			t.Error("a collaborator who already has push should not be changed by undo")
		}
		if strings.Contains(c, "prof") {
			t.Error("admins must be left untouched by undo")
		}
	}
}

func TestFreezeDowngradesPendingInvitations(t *testing.T) {
	// The loophole: a student who has not accepted yet is not a collaborator, so
	// walking collaborators alone leaves their invitation carrying write. They
	// accept after the deadline and can push. Freeze must downgrade the invitation
	// itself.
	fake := freezeFake("admin")
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationWrite)}
	o := newFreezeOpts(t, fake, false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-alan:alan=invite:read") {
		t.Errorf("a pending write invitation must be downgraded to read: %v", fake.changes)
	}
	if !strings.Contains(buf.String(), "1 pending invitation(s) downgraded to read") {
		t.Errorf("the summary should report the invitation:\n%s", buf.String())
	}
}

func TestFreezeUndoRestoresPendingInvitations(t *testing.T) {
	// The reverse: an extension for a student who still has not accepted must put
	// their invitation back to write, or the extension grants them nothing.
	fake := freezeFake("admin")
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationRead)}
	o := newFreezeOpts(t, fake, true, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-alan:alan=invite:write") {
		t.Errorf("undo should restore a frozen invitation to write: %v", fake.changes)
	}
	if !strings.Contains(buf.String(), "1 pending invitation(s) restored to write") {
		t.Errorf("the summary should report the invitation:\n%s", buf.String())
	}
}

func TestFreezeLeavesSettledInvitationsAlone(t *testing.T) {
	// An expired invitation cannot be accepted, so it is not a way past the freeze
	// and there is nothing to change; one already at read is already frozen. Both
	// must be left untouched so freeze stays idempotent and does not resurrect a
	// lapsed invitation.
	fake := freezeFake("admin")
	expired := invite(8, "grace", gh.InvitationWrite)
	expired.Expired = true
	fake.invites["hw1-ada"] = []gh.Invitation{expired, invite(9, "bob", gh.InvitationRead)}
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "invite:") {
			t.Errorf("an expired or already-read invitation must not be touched: %v", fake.changes)
		}
	}
}

func TestFreezeVerifiesInvitationDowngradeTookEffect(t *testing.T) {
	// As with a collaborator grant, a 200 on the invitation PATCH is not proof.
	// The post-condition re-read must catch an invitation that still confers write
	// and fail loudly rather than report a deadline lock with a hole in it.
	fake := freezeFake("admin")
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationWrite)}
	fake.dontApply = true
	o := newFreezeOpts(t, fake, false, false)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("freeze should fail when the invitation downgrade did not take, got %v", err)
	}
	if !strings.Contains(buf.String(), "pending invitation still confers write") {
		t.Errorf("the failure should name the invitation as the reason:\n%s", buf.String())
	}
}

func TestFreezeDryRunLeavesInvitationsAlone(t *testing.T) {
	fake := freezeFake("admin")
	fake.invites["hw1-alan"] = []gh.Invitation{invite(7, "alan", gh.InvitationWrite)}
	o := newFreezeOpts(t, fake, false, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.changes) != 0 {
		t.Errorf("dry-run must not change any invitation, got %v", fake.changes)
	}
	if !strings.Contains(buf.String(), "1 pending invitation(s)") {
		t.Errorf("dry-run should still report what it would change:\n%s", buf.String())
	}
}

func TestFreezeRecordsTheFreezeState(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hw1-ada=true", "hw1-alan=true"} {
		if !contains(fake.recorded, want) {
			t.Errorf("every frozen repo should be recorded: %v", fake.recorded)
		}
	}
	for _, r := range fake.recorded {
		if strings.HasPrefix(r, "project-x") {
			t.Errorf("only this assignment's repos should be recorded: %v", fake.recorded)
		}
	}
}

func TestFreezeUndoRecordsThawedNotUnset(t *testing.T) {
	// An extension records false rather than clearing the value. "Never frozen" and
	// "deliberately thawed" must stay distinguishable, or a later reader cannot
	// tell an extension from a repo that predates the record.
	fake := freezeFake("admin")
	fake.frozen["hw1-ada"] = freezeFrozen
	fake.frozen["hw1-alan"] = freezeFrozen
	o := newFreezeOpts(t, fake, true, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada"}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.recorded, "hw1-ada=false") {
		t.Errorf("undo should record the repo as thawed: %v", fake.recorded)
	}
	if fake.frozen["hw1-alan"] != freezeFrozen {
		t.Errorf("an unnamed repo's record must not change: %v", fake.frozen)
	}
}

func TestFreezeRecordsBeforeRemovingAccess(t *testing.T) {
	// Ordering matters for a run that dies midway. Recording first leaves the safe
	// intermediate state (recorded frozen, still writable), so a later renew
	// withholds write from a half-locked repo. The reverse order would leave a
	// locked repo unrecorded, and renew would hand push back.
	fake := freezeFake("admin")
	var order []string
	fk := fake.fake()
	setProp := fk.SetRepoPropertyValueFunc
	fk.SetRepoPropertyValueFunc = func(ctx context.Context, org, repo, name, value string) error {
		fk.Lock()
		order = append(order, "record:"+repo)
		fk.Unlock()
		return setProp(ctx, org, repo, name, value)
	}
	addCollab := fk.AddCollaboratorFunc
	fk.AddCollaboratorFunc = func(ctx context.Context, org, repo, user, perm string) error {
		fk.Lock()
		order = append(order, "access:"+repo)
		fk.Unlock()
		return addCollab(ctx, org, repo, user, perm)
	}
	o := &freezeOpts{
		g:         assignGlobals(),
		newClient: func(context.Context) (freezeClient, error) { return fk, nil },
	}
	o.g.concurrency = 1 // deterministic ordering across repos

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada"}); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "record:hw1-ada" || order[1] != "access:hw1-ada" {
		t.Errorf("the freeze must be recorded before access is removed, got %v", order)
	}
}

func TestFreezeVerifiesTheRecordTookEffect(t *testing.T) {
	// A 200 on the property write is not proof. If the record did not take, a later
	// renew would restore write on a frozen repo, so freeze must fail loudly.
	fake := freezeFake("admin")
	fake.dontRecord = true
	o := newFreezeOpts(t, fake, false, false)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("freeze should fail when the record did not take, got %v", err)
	}
	if !strings.Contains(buf.String(), "audit --renew") {
		t.Errorf("the failure should say what it would break:\n%s", buf.String())
	}
}

func TestFreezeAbortsWithoutTheFreezeProperty(t *testing.T) {
	// An org that never ran setup cannot record a freeze. Freezing anyway would
	// lock the repos while leaving renew free to unlock them, so freeze refuses
	// before touching anything.
	fake := freezeFake("admin")
	fake.noProperty = true
	o := newFreezeOpts(t, fake, false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "gh cls setup") {
		t.Fatalf("freeze should abort and name the fix, got %v", err)
	}
	if len(fake.changes) != 0 || len(fake.recorded) != 0 {
		t.Errorf("nothing may change when the freeze cannot be recorded: %v %v", fake.changes, fake.recorded)
	}
}

func TestFreezeDowngradesInvitationsBeforeCollaborators(t *testing.T) {
	// Ordering is the whole defence against the acceptance race. A student is in
	// the invitation list until they accept and in the collaborator list after.
	// Doing collaborators first leaves a window where a student is in neither, so
	// the invitation pass must run first.
	fake := freezeFake("admin")
	fake.invites["hw1-ada"] = []gh.Invitation{invite(7, "bob", gh.InvitationWrite)}
	var order []string
	fk := fake.fake()
	listInv := fk.ListRepoInvitationsFunc
	fk.ListRepoInvitationsFunc = func(ctx context.Context, org, repo string) ([]gh.Invitation, error) {
		fk.Lock()
		order = append(order, "invitations:"+repo)
		fk.Unlock()
		return listInv(ctx, org, repo)
	}
	listCollab := fk.ListDirectCollaboratorsFunc
	fk.ListDirectCollaboratorsFunc = func(ctx context.Context, org, repo string) ([]gh.Collaborator, error) {
		fk.Lock()
		order = append(order, "collaborators:"+repo)
		fk.Unlock()
		return listCollab(ctx, org, repo)
	}
	o := &freezeOpts{g: assignGlobals(), newClient: func(context.Context) (freezeClient, error) { return fk, nil }}
	o.g.concurrency = 1

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada"}); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "invitations:hw1-ada" || order[1] != "collaborators:hw1-ada" {
		t.Errorf("invitations must be read before collaborators, got %v", order)
	}
}

func TestFreezeClosesTheAcceptanceRace(t *testing.T) {
	// bob accepts his write invitation at the exact moment freeze tries to
	// downgrade it: the invitation is consumed and he becomes a write
	// collaborator. Because the collaborator pass runs afterwards, it catches him
	// and the freeze still holds. With the passes the other way round he would
	// have been in neither list and kept write.
	fake := freezeFake("admin")
	fake.invites["hw1-ada"] = []gh.Invitation{invite(7, "bob", gh.InvitationWrite)}
	fake.acceptOnUpdate = map[int64]string{7: "bob"}
	o := newFreezeOpts(t, fake, false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("the race must not fail the freeze: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-ada:bob=pull") {
		t.Errorf("a student who accepted mid-run must still be downgraded: %v", fake.changes)
	}
	// He was never counted as an invitation change, since that update found
	// nothing to change.
	if strings.Contains(buf.String(), "pending invitation(s)") {
		t.Errorf("a consumed invitation should not be reported as downgraded:\n%s", buf.String())
	}
}

func TestFreezeUndoClosesTheAcceptanceRace(t *testing.T) {
	// The same race on the way back: bob accepts a read invitation as --undo runs.
	// The collaborator pass afterwards must grant him push, or his extension gives
	// him nothing.
	fake := freezeFake("admin")
	fake.invites["hw1-ada"] = []gh.Invitation{invite(7, "bob", gh.InvitationRead)}
	fake.acceptOnUpdate = map[int64]string{7: "bob"}
	// He lands as a read collaborator, which is what accepting a frozen invite gives.
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull"), collab("prof", "admin")}
	o := newFreezeOpts(t, fake, true, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatalf("the race must not fail the undo: %v\n%s", err, buf.String())
	}
	if !contains(fake.changes, "hw1-ada:bob=push") {
		t.Errorf("a student who accepted mid-undo must still get push: %v", fake.changes)
	}
}

func TestFreezeVerifiesDowngradeTookEffect(t *testing.T) {
	// The API accepts the downgrade but it does not actually take effect. The
	// freeze must re-read, detect the still-open gate, and fail loudly rather than
	// report a deadline lock that never happened.
	fake := freezeFake("admin")
	fake.dontApply = true
	o := newFreezeOpts(t, fake, false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("freeze should fail when the downgrade did not take, got %v", err)
	}
}

func TestFreezeDryRunMakesNoChanges(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.changes) != 0 {
		t.Errorf("dry-run must not change anything, got %v", fake.changes)
	}
	out := buf.String()
	if !strings.Contains(out, "dry-run") || !strings.Contains(out, "would change 1") {
		t.Errorf("dry-run summary wrong: %s", out)
	}
}

func TestFreezeOwnerGuard(t *testing.T) {
	fake := freezeFake("member")
	o := newFreezeOpts(t, fake, false, false)
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil)
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
}

func TestFreezeUnknownAssignmentIsRejected(t *testing.T) {
	// A typo'd assignment name must be caught before any API call, not left to
	// the zero-matches fallback below, which a prefix collision (#1) or a stale
	// assignment could defeat by still matching repos.
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)
	err := o.run(context.Background(), &bytes.Buffer{}, "nosuch", nil)
	if err == nil || !strings.Contains(err.Error(), `assignment "nosuch" not found in config`) {
		t.Fatalf("unknown assignment should be rejected, got %v", err)
	}
	if fake.apiCalls != 0 {
		t.Errorf("no API call should be made for an unknown assignment, got %d", fake.apiCalls)
	}
}

func TestFreezeNoMatchingReposIsAnError(t *testing.T) {
	// Zero matches at a deadline is almost always a wrong name/org, so freeze must
	// fail loudly rather than report a silent no-op success. "midterm" is a
	// configured assignment (so it passes the not-found-in-config check) with no
	// matching repos in the fake.
	fake := freezeFake("admin")
	g := assignGlobals()
	g.cfg.Assignments["midterm"] = config.Assignment{Type: config.TypeIndividual}
	o := newFreezeOptsG(t, g, fake, false, false)
	err := o.run(context.Background(), &bytes.Buffer{}, "midterm", nil)
	if err == nil || !strings.Contains(err.Error(), "no student repositories named midterm-*") {
		t.Fatalf("zero matches should be an error, got %v", err)
	}
	if len(fake.changes) != 0 {
		t.Error("nothing should change when no repos match")
	}
}

func TestFreezeKeyRestrictsToNamedRepo(t *testing.T) {
	// An extension: unfreeze only ada's repo, leaving alan's frozen. Naming a key
	// must scope the operation to that one repo.
	fake := freezeFake("admin")
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")} // frozen
	o := newFreezeOpts(t, fake, true, false)                           // undo

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", []string{"ada"}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.changes, "hw1-ada:ada=push") {
		t.Errorf("named repo should be unfrozen: %v", fake.changes)
	}
	for _, c := range fake.changes {
		if strings.Contains(c, "alan") {
			t.Errorf("an unnamed repo must be left alone: %v", fake.changes)
		}
	}
	if !strings.Contains(buf.String(), "1 repo") {
		t.Errorf("should report a single repo: %s", buf.String())
	}
}

func TestFreezeKeyMatchesCaseInsensitively(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ADA"}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.changes, "hw1-ada:ada=pull") {
		t.Errorf("key should match repo case-insensitively: %v", fake.changes)
	}
}

func TestFreezeUnknownKeyAbortsWithoutChanges(t *testing.T) {
	// A mistyped extension key must fail loudly before any mutation, so it never
	// silently freezes (or spares) nothing.
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, true, false)
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", []string{"ada", "adaa"})
	if err == nil || !strings.Contains(err.Error(), "hw1-adaa") {
		t.Fatalf("unknown key should be an error naming the missing repo, got %v", err)
	}
	if len(fake.changes) != 0 {
		t.Errorf("nothing should change when any key is unknown, got %v", fake.changes)
	}
}

func TestFreezeSkipsTemplateRepo(t *testing.T) {
	// hw1-template matches the hw1-* prefix but is a template repository, not
	// student work, so freeze must never touch it.
	fake := freezeFake("admin")
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-template", IsTemplate: true})
	fake.collabs["hw1-template"] = []gh.Collaborator{collab("ada", "push")}
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	for _, ch := range fake.changes {
		if strings.HasPrefix(ch, "hw1-template:") {
			t.Errorf("freeze must not touch the template repo: %v", fake.changes)
		}
	}
}

func TestFreezeExcludesLongerOverlappingAssignmentRepos(t *testing.T) {
	// "proj" and "proj-final" both configured (a config from before that
	// combination was rejected, or a mid-semester rename) would otherwise let
	// `freeze proj` match proj-final's repos too, since they also start with
	// "proj-". filterAssignmentRepos must exclude them.
	g := &globalOpts{
		org:         "cs101-spring26",
		concurrency: 4,
		cfg: &config.Config{Assignments: map[string]config.Assignment{
			"proj":       {Type: config.TypeIndividual},
			"proj-final": {Type: config.TypeIndividual},
		}},
	}
	fake := &fakeFreezeState{
		role:  "admin",
		repos: []gh.Repo{{Name: "proj-x"}, {Name: "proj-final-y"}},
		collabs: map[string][]gh.Collaborator{
			"proj-x":       {collab("x", "push")},
			"proj-final-y": {collab("y", "push")},
		},
	}
	o := newFreezeOptsG(t, g, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "proj", nil); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.changes, "proj-x:x=pull") {
		t.Errorf("proj-x should be frozen: %v", fake.changes)
	}
	for _, ch := range fake.changes {
		if strings.HasPrefix(ch, "proj-final-y:") {
			t.Errorf("freeze proj must not touch proj-final's repos: %v", fake.changes)
		}
	}
}

// dropInvite removes an invitation from a repo, as accepting it does.
func (s *fakeFreezeState) dropInvite(repo string, id int64) {
	var rest []gh.Invitation
	for _, inv := range s.invites[repo] {
		if inv.ID != id {
			rest = append(rest, inv)
		}
	}
	s.invites[repo] = rest
}

func TestFreezeSkipsAConfiguredTemplateWhoseFlagWasCleared(t *testing.T) {
	// GitHub's template flag is remote and mutable: cleared in the web UI, the
	// template is student-repo-shaped to a <name>-* listing, and freeze would
	// downgrade the instructor's own access to the starter code. hw1-template is
	// the template hw1's config names, so it is excluded on that alone.
	fake := freezeFake("admin")
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-template"}) // IsTemplate cleared
	fake.collabs["hw1-template"] = []gh.Collaborator{collab("ada", "push")}
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	for _, ch := range fake.changes {
		if strings.HasPrefix(ch, "hw1-template:") {
			t.Errorf("freeze must not touch the configured template repo: %v", fake.changes)
		}
	}
}

func TestFreezeStreamsEachRepoAsItIsProcessed(t *testing.T) {
	// freeze runs at a deadline, when the instructor is watching. A repo that had
	// nothing to downgrade is called out as such rather than reported as frozen.
	fake := freezeFake("admin")
	fake.repos = append(fake.repos, gh.Repo{Name: "hw1-kath"}) // no collaborators
	o := newFreezeOpts(t, fake, false, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{"[1/3]", "frozen       hw1-ada", "no change    hw1-kath"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestFreezeDryRunNeverClaimsARepoWasFrozen(t *testing.T) {
	// The whole point of --dry-run is that nothing changed, so the per-repo word
	// has to stay conditional even though the run does the same per-repo reads.
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, true /*dryRun*/)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log("\n" + out)
	if !strings.Contains(out, "would freeze hw1-ada") {
		t.Errorf("a dry run should say what it would do:\n%s", out)
	}
	if strings.Contains(out, "frozen       hw1-ada") {
		t.Errorf("a dry run must not report a repo as frozen:\n%s", out)
	}
}
