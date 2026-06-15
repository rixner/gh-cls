package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rixner/gh-cls/gh"
)

// fakeAuditClient is a concurrency-safe, per-repo configurable stand-in for the
// audit operations.
type fakeAuditClient struct {
	mu      sync.Mutex
	role    string
	repos   map[string]bool
	collabs map[string][]gh.Collaborator
	invites map[string][]gh.Invitation
	listErr map[string]bool // repos whose collaborator listing fails
	addErr  map[string]bool // logins whose AddCollaborator fails
	silent  map[string]bool // logins whose grant records but produces no access
	added   []string        // "repo:login:perm"
	deleted []string        // "repo:invID"
	nextID  int64
}

func newFakeAudit(role string) *fakeAuditClient {
	return &fakeAuditClient{
		role:    role,
		repos:   map[string]bool{},
		collabs: map[string][]gh.Collaborator{},
		invites: map[string][]gh.Invitation{},
		listErr: map[string]bool{},
		addErr:  map[string]bool{},
		silent:  map[string]bool{},
		nextID:  1000,
	}
}

func (f *fakeAuditClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeAuditClient) ListOrgReposByPrefix(_ context.Context, _, prefix string) ([]gh.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gh.Repo
	for name := range f.repos {
		if strings.HasPrefix(name, prefix) {
			out = append(out, gh.Repo{Name: name})
		}
	}
	return out, nil
}

func (f *fakeAuditClient) ListDirectCollaborators(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr[repo] {
		return nil, fmt.Errorf("listing failed for %s", repo)
	}
	return append([]gh.Collaborator(nil), f.collabs[repo]...), nil
}

func (f *fakeAuditClient) ListRepoInvitations(_ context.Context, _, repo string) ([]gh.Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gh.Invitation(nil), f.invites[repo]...), nil
}

func (f *fakeAuditClient) AddCollaborator(_ context.Context, _, repo, login, perm string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr[login] {
		return fmt.Errorf("add failed for %s", login)
	}
	f.added = append(f.added, repo+":"+login+":"+perm)
	if f.silent[login] {
		return nil // records the call but leaves no access or invitation
	}
	f.nextID++
	inv := gh.Invitation{ID: f.nextID}
	inv.Invitee.Login = login
	f.invites[repo] = append(f.invites[repo], inv) // a fresh, non-expired invitation
	return nil
}

func (f *fakeAuditClient) DeleteRepoInvitation(_ context.Context, _, repo string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, fmt.Sprintf("%s:%d", repo, id))
	var rest []gh.Invitation
	for _, inv := range f.invites[repo] {
		if inv.ID != id {
			rest = append(rest, inv)
		}
	}
	f.invites[repo] = rest
	return nil
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

// newAuditOpts wires auditOpts to a fake, isolating config to a temp file.
func newAuditOpts(t *testing.T, fake *fakeAuditClient, rosterCSV, teamsYML string) *auditOpts {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte(assignConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CLS_CONFIG", cfgPath)

	rosterPath := filepath.Join(dir, "roster.csv")
	if err := os.WriteFile(rosterPath, []byte(rosterCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	teamsPath := ""
	if teamsYML != "" {
		teamsPath = filepath.Join(dir, "teams.yml")
		if err := os.WriteFile(teamsPath, []byte(teamsYML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &auditOpts{
		g:         &globalOpts{concurrency: 4},
		roster:    rosterPath,
		teams:     teamsPath,
		newClient: func(context.Context) (auditClient, error) { return fake, nil },
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

func TestAuditGroupRequiresTeams(t *testing.T) {
	fake := newFakeAudit("admin")
	o := newAuditOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "project")
	if err == nil || !strings.Contains(err.Error(), "--teams is required") {
		t.Fatalf("group audit without teams should error, got %v", err)
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
