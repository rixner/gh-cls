package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeAuditState configures a ghtest.Fake for audit tests, per-repo
// configurable, and captures what it observed.
type fakeAuditState struct {
	role    string
	repos   map[string]bool
	collabs map[string][]gh.Collaborator
	invites map[string][]gh.Invitation
	listErr map[string]bool // repos whose collaborator listing fails
	addErr  map[string]bool // logins whose AddCollaborator fails
	silent  map[string]bool // logins whose grant records but produces no access
	nextID  int64

	// frozen is each repo's recorded freeze state; absent means never recorded.
	frozen map[string]freezeState
	// noProperty simulates an org that never ran setup, so the freeze record
	// cannot be read at all.
	noProperty bool

	added   []string // "repo:login:perm"
	deleted []string // "repo:invID"
}

func newFakeAudit(role string) *fakeAuditState {
	return &fakeAuditState{
		role:    role,
		repos:   map[string]bool{},
		collabs: map[string][]gh.Collaborator{},
		invites: map[string][]gh.Invitation{},
		listErr: map[string]bool{},
		addErr:  map[string]bool{},
		silent:  map[string]bool{},
		nextID:  1000,
		frozen:  map[string]freezeState{},
	}
}

// withProperties wires the freeze-record reads onto a fake. Every command that
// consults the record needs these, so the fakes share one helper.
func (s *fakeAuditState) withProperties(fk *ghtest.Fake) {
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
	fk.ListRepoPropertyValuesFunc = func(context.Context, string) (map[string]map[string]string, error) {
		fk.Lock()
		defer fk.Unlock()
		out := make(map[string]map[string]string, len(s.frozen))
		for repo, state := range s.frozen {
			out[repo] = map[string]string{frozenProperty: string(state)}
		}
		return out, nil
	}
}

func (s *fakeAuditState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.ListOrgReposByPrefixFunc = func(_ context.Context, _, prefix string) ([]gh.Repo, error) {
		fk.Lock()
		defer fk.Unlock()
		var out []gh.Repo
		for name := range s.repos {
			if strings.HasPrefix(name, prefix) {
				out = append(out, gh.Repo{Name: name})
			}
		}
		return out, nil
	}
	fk.ListDirectCollaboratorsFunc = func(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
		fk.Lock()
		defer fk.Unlock()
		if s.listErr[repo] {
			return nil, fmt.Errorf("listing failed for %s", repo)
		}
		return append([]gh.Collaborator(nil), s.collabs[repo]...), nil
	}
	fk.ListRepoInvitationsFunc = func(_ context.Context, _, repo string) ([]gh.Invitation, error) {
		fk.Lock()
		defer fk.Unlock()
		return append([]gh.Invitation(nil), s.invites[repo]...), nil
	}
	fk.AddCollaboratorFunc = func(_ context.Context, _, repo, login, perm string) error {
		fk.Lock()
		defer fk.Unlock()
		if s.addErr[login] {
			return fmt.Errorf("add failed for %s", login)
		}
		s.added = append(s.added, repo+":"+login+":"+perm)
		if s.silent[login] {
			return nil // records the call but leaves no access or invitation
		}
		s.nextID++
		inv := gh.Invitation{ID: s.nextID}
		inv.Invitee.Login = login
		s.invites[repo] = append(s.invites[repo], inv) // a fresh, non-expired invitation
		return nil
	}
	fk.DeleteRepoInvitationFunc = func(_ context.Context, _, repo string, id int64) error {
		fk.Lock()
		defer fk.Unlock()
		s.deleted = append(s.deleted, fmt.Sprintf("%s:%d", repo, id))
		var rest []gh.Invitation
		for _, inv := range s.invites[repo] {
			if inv.ID != id {
				rest = append(rest, inv)
			}
		}
		s.invites[repo] = rest
		return nil
	}
	s.withProperties(fk)
	return fk
}

func pushCollab(login string) gh.Collaborator {
	c := gh.Collaborator{Login: login}
	c.Permissions.Push = true
	return c
}

func pendingInvite(id int64, login string) gh.Invitation {
	var i gh.Invitation
	i.ID = id
	i.Invitee.Login = login
	return i
}

func expiredInvite(id int64, login string) gh.Invitation {
	i := pendingInvite(id, login)
	i.Expired = true
	return i
}

// newAuditOpts wires auditOpts to a fake; the roster/groups files live in a temp
// dir and the config comes from assignGlobals.
func newAuditOpts(t *testing.T, fake *fakeAuditState, rosterCSV, groupsYML string) *auditOpts {
	t.Helper()
	return newAuditOptsG(t, assignGlobals(), fake, rosterCSV, groupsYML)
}

func newAuditOptsG(t *testing.T, g *globalOpts, fake *fakeAuditState, rosterCSV, groupsYML string) *auditOpts {
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
	return &auditOpts{
		g:         g,
		roster:    rosterPath,
		groups:    groupsPath,
		newClient: func(context.Context) (auditClient, error) { return fk, nil },
	}
}

func TestAuditClassifiesStatuses(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{pushCollab("ada")}
	fake.invites["hw1-alan"] = []gh.Invitation{pendingInvite(1, "alan")}
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(2, "grace")}
	o := newAuditOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// alan pending and grace expired are shown with their university ids; ada (on
	// repo) is summarized, not listed, by default.
	for _, want := range []string{"invited (pending)", "invited (EXPIRED)", "student-002", "student-003",
		"1 on repo, 1 pending, 1 expired", "Action needed: 1 expired"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hw1-ada") {
		t.Errorf("an on-repo student should not be listed without --all:\n%s", out)
	}
}

func TestAuditAllListsOnRepo(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{pushCollab("ada")}
	fake.collabs["hw1-alan"] = []gh.Collaborator{pushCollab("alan")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{pushCollab("grace")}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.all = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "hw1-ada") || !strings.Contains(out, "on repo") {
		t.Errorf("--all should list on-repo students:\n%s", out)
	}
}

func TestAuditReportsMissingAndNoRepo(t *testing.T) {
	fake := newFakeAudit("admin")
	// hw1-ada exists but is empty (missing); hw1-grace is on repo; hw1-alan was
	// never created (no repo).
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-grace": true}
	fake.collabs["hw1-grace"] = []gh.Collaborator{pushCollab("grace")}
	o := newAuditOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"MISSING", "NO REPO", "1 missing", "1 without a repo", "gh cls assign hw1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAuditFlagsUnexpectedAccess(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	// ada is correctly on repo; a stranger also has access; an admin (staff) is
	// present and must not be flagged.
	admin := gh.Collaborator{Login: "instructor"}
	admin.Permissions.Admin = true
	fake.collabs["hw1-ada"] = []gh.Collaborator{pushCollab("ada"), pushCollab("stranger"), admin}
	fake.collabs["hw1-alan"] = []gh.Collaborator{pushCollab("alan")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{pushCollab("grace")}
	o := newAuditOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Unexpected access") || !strings.Contains(out, "stranger") {
		t.Errorf("unexpected collaborator should be flagged:\n%s", out)
	}
	if strings.Contains(out, "instructor") {
		t.Errorf("an admin should not be flagged as unexpected:\n%s", out)
	}
}

func TestAuditOwnerGuard(t *testing.T) {
	fake := newFakeAudit("member")
	o := newAuditOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
}

func TestAuditReportsGroupProblems(t *testing.T) {
	// Unlike assign, audit never aborts on roster/groups inconsistencies: it reports
	// them as warnings and audits what it can. Here student-003 is in no group and
	// student-001 is in two groups.
	fake := newFakeAudit("admin")
	groupsYML := "group-alpha: [student-001, student-002]\ngroup-beta: [student-001]\n"
	o := newAuditOpts(t, fake, assignRoster, groupsYML)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "student-003 is in no group") {
		t.Errorf("no-group student should be reported: %s", out)
	}
	if !strings.Contains(out, "student-001 is in more than one group") || !strings.Contains(out, "group-alpha, group-beta") {
		t.Errorf("multi-group student should be reported with their groups: %s", out)
	}
}

func TestAuditGroupRequiresGroups(t *testing.T) {
	fake := newFakeAudit("admin")
	o := newAuditOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "project")
	if err == nil || !strings.Contains(err.Error(), "--groups is required") {
		t.Fatalf("group audit without groups should error, got %v", err)
	}
}

func TestAuditRenewExpiredAndMissing(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{pushCollab("ada")}          // on repo: untouched
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(99, "grace")} // expired
	// hw1-alan exists but empty: missing
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("renew should succeed, got %v\n%s", err, buf.String())
	}
	if !contains(fake.deleted, "hw1-grace:99") {
		t.Errorf("expired invitation should be cancelled before re-inviting: %v", fake.deleted)
	}
	if !contains(fake.added, "hw1-grace:grace:push") || !contains(fake.added, "hw1-alan:alan:push") {
		t.Errorf("expired and missing students should be re-invited with push: %v", fake.added)
	}
	for _, a := range fake.added {
		if strings.Contains(a, ":ada:") {
			t.Errorf("an on-repo student must not be re-invited: %v", fake.added)
		}
	}
	if !strings.Contains(buf.String(), "access for 2 student(s), 0 failed") {
		t.Errorf("summary wrong:\n%s", buf.String())
	}
}

func TestAuditReportsFrozenStudentsAsFrozen(t *testing.T) {
	// A freeze leaves every student on their repo with read. Classifying that as
	// MISSING (the old behavior, because "on repo" required push) reported a
	// correctly frozen class as entirely absent and told the instructor to run
	// --renew, which would have unfrozen the assignment.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collab("alan", "pull")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collab("grace", "pull")}
	o := newAuditOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "MISSING") {
		t.Errorf("a frozen student is on their repo, not missing:\n%s", out)
	}
	if strings.Contains(out, "Action needed") {
		t.Errorf("a correctly frozen assignment needs no action:\n%s", out)
	}
	if !strings.Contains(out, "0 on repo, 3 frozen") {
		t.Errorf("summary should count the students as frozen:\n%s", out)
	}
	if !strings.Contains(out, "All 3 student(s) are on their repos (3 frozen).") {
		t.Errorf("the all-settled line should note the freeze:\n%s", out)
	}
}

func TestAuditRenewLeavesAFrozenAssignmentAlone(t *testing.T) {
	// The loophole this closes: --renew re-granting push to a frozen class would
	// silently undo the deadline. Frozen students are settled, so renew must find
	// nothing to do.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")}
	fake.collabs["hw1-alan"] = []gh.Collaborator{collab("alan", "pull")}
	fake.collabs["hw1-grace"] = []gh.Collaborator{collab("grace", "pull")}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 0 {
		t.Errorf("renew must not re-grant access on a frozen assignment: %v", fake.added)
	}
	if !strings.Contains(buf.String(), "nothing to re-issue") {
		t.Errorf("renew should report nothing to do:\n%s", buf.String())
	}
}

func TestAuditAllListsFrozenStudents(t *testing.T) {
	// Frozen is a settled state, so it is summarized rather than listed by
	// default; --all must still show it, with its own label.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true}
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.all = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "on repo (frozen)") {
		t.Errorf("--all should list the frozen student with its label:\n%s", buf.String())
	}
}

func TestAuditRenewRestoresReadOnAFrozenRepo(t *testing.T) {
	// The residual hole the freeze record closes. These students have no access at
	// all (expired or never issued), so their own repo carries no permission to
	// infer the deadline state from. Without the record, renew re-grants push and
	// reopens the assignment for exactly the students it touches.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.frozen["hw1-ada"] = freezeFrozen
	fake.frozen["hw1-alan"] = freezeFrozen
	fake.frozen["hw1-grace"] = freezeFrozen
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")}      // frozen, settled
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(99, "grace")} // expired
	// hw1-alan is empty: missing
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("renew should succeed, got %v\n%s", err, buf.String())
	}
	for _, want := range []string{"hw1-grace:grace:pull", "hw1-alan:alan:pull"} {
		if !contains(fake.added, want) {
			t.Errorf("a frozen repo must restore read, not write: %v", fake.added)
		}
	}
	for _, a := range fake.added {
		if strings.HasSuffix(a, ":push") {
			t.Errorf("no push may be granted on a frozen assignment: %v", fake.added)
		}
	}
	if !strings.Contains(buf.String(), "2 of them are on frozen repos and are being restored to read") {
		t.Errorf("the run should say it withheld write:\n%s", buf.String())
	}
}

func TestAuditRenewRestoresWriteOnAThawedExtensionRepo(t *testing.T) {
	// The state that makes assignment-wide inference impossible: the deadline has
	// passed for the class, but one repo has an extension. The record is per repo,
	// so alan gets write back while grace stays read.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.frozen["hw1-ada"] = freezeFrozen
	fake.frozen["hw1-alan"] = freezeThawed // extension granted
	fake.frozen["hw1-grace"] = freezeFrozen
	fake.collabs["hw1-ada"] = []gh.Collaborator{collab("ada", "pull")}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatalf("renew should succeed, got %v\n%s", err, buf.String())
	}
	if !contains(fake.added, "hw1-alan:alan:push") {
		t.Errorf("the extension repo should get write back: %v", fake.added)
	}
	if !contains(fake.added, "hw1-grace:grace:pull") {
		t.Errorf("a frozen repo in the same assignment should still get read: %v", fake.added)
	}
}

func TestAuditRenewGrantsWriteWhenNothingIsFrozen(t *testing.T) {
	// The ordinary pre-deadline repair: no repo has ever been stamped, so renew
	// restores write as it always did. An unrecorded repo must not read as frozen.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hw1-ada:ada:push", "hw1-alan:alan:push", "hw1-grace:grace:push"} {
		if !contains(fake.added, want) {
			t.Errorf("an unfrozen assignment should restore write: %v", fake.added)
		}
	}
	if strings.Contains(buf.String(), "restored to read") {
		t.Errorf("nothing is frozen, so no read-restore note belongs:\n%s", buf.String())
	}
}

func TestAuditRenewAbortsWithoutTheFreezeRecord(t *testing.T) {
	// An org that never ran setup has no record to consult. Defaulting to "nothing
	// is frozen" would re-grant write across an assignment whose deadline may have
	// passed, so renew must refuse rather than guess.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true}
	fake.noProperty = true
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "gh cls setup") {
		t.Fatalf("renew should abort and name the fix, got %v", err)
	}
	if len(fake.added) != 0 {
		t.Errorf("nothing may be granted when the freeze state is unknown: %v", fake.added)
	}
}

func TestAuditRenewDryRun(t *testing.T) {
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(99, "grace")}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true
	o.dryRun = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 0 || len(fake.deleted) != 0 {
		t.Errorf("dry-run must not mutate: added=%v deleted=%v", fake.added, fake.deleted)
	}
	if !strings.Contains(buf.String(), "dry-run") || !strings.Contains(buf.String(), "would re-issue") {
		t.Errorf("dry-run output missing:\n%s", buf.String())
	}
}

func TestAuditRenewVerifiesResult(t *testing.T) {
	// The grant call returns success but leaves no access or invitation: the
	// post-condition check must catch it and fail.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(99, "grace")}
	fake.silent = map[string]bool{"grace": true}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1")
	if err == nil || !strings.Contains(err.Error(), "failed to renew") {
		t.Fatalf("a renew that did not take should fail, got %v", err)
	}
	if !strings.Contains(buf.String(), "did not take") {
		t.Errorf("the failure should explain the renew did not take:\n%s", buf.String())
	}
}

func TestAuditRenewAbortsOnAuditError(t *testing.T) {
	// If a repo cannot be audited, renew must not act on a partial picture.
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"hw1-ada": true, "hw1-alan": true, "hw1-grace": true}
	fake.invites["hw1-grace"] = []gh.Invitation{expiredInvite(99, "grace")}
	fake.listErr = map[string]bool{"hw1-alan": true}
	o := newAuditOpts(t, fake, assignRoster, "")
	o.renew = true

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "aborting --renew") {
		t.Fatalf("renew should abort when an audit fails, got %v", err)
	}
	if len(fake.added) != 0 || len(fake.deleted) != 0 {
		t.Errorf("no mutation should occur when renew aborts: added=%v deleted=%v", fake.added, fake.deleted)
	}
}

func TestAuditExcludesLongerOverlappingAssignmentRepos(t *testing.T) {
	// "proj" and "proj-final" both configured, with a student on "proj" whose
	// key happens to collide with a proj-final repo's suffix ("proj-final-y"
	// matches the "proj-" prefix as key "final-y"). Auditing "proj" must not
	// treat proj-final's repo as this student's repo.
	g := &globalOpts{
		org: "cs101-spring26",
		cfg: &config.Config{Assignments: map[string]config.Assignment{
			"proj":       {Type: config.TypeIndividual},
			"proj-final": {Type: config.TypeIndividual},
		}},
		concurrency: 4,
	}
	fake := newFakeAudit("admin")
	fake.repos = map[string]bool{"proj-final-y": true}
	roster := "identifier,username\nstudent-001,final-y\n"
	o := newAuditOptsG(t, g, fake, roster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "proj"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 without a repo") {
		t.Errorf("final-y should show as having no proj repo (proj-final-y belongs to proj-final):\n%s", out)
	}
}
