package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rixner/gh-cls/gh"
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

type fakeFreezeClient struct {
	mu        sync.Mutex
	role      string
	repos     []gh.Repo
	collabs   map[string][]gh.Collaborator
	changes   []string // "repo:user=permission"
	dontApply bool     // record the change but leave the permission unchanged
}

func (f *fakeFreezeClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeFreezeClient) ListOrgReposByPrefix(_ context.Context, _, prefix string) ([]gh.Repo, error) {
	var out []gh.Repo
	for _, r := range f.repos {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeFreezeClient) ListDirectCollaborators(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.collabs[repo]
	out := make([]gh.Collaborator, len(cs))
	copy(out, cs)
	return out, nil
}

func (f *fakeFreezeClient) AddCollaborator(_ context.Context, _, repo, username, permission string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, repo+":"+username+"="+permission)
	if f.dontApply {
		return nil
	}
	// Reflect the new permission so a subsequent re-read (the post-condition
	// verification) sees the change, as the real API would.
	cs := f.collabs[repo]
	for i := range cs {
		if cs[i].Login == username {
			cs[i] = collab(username, permission)
		}
	}
	f.collabs[repo] = cs
	return nil
}

func newFreezeOpts(t *testing.T, fake *fakeFreezeClient, undo, dryRun bool) *freezeOpts {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("org: cs101-spring26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CLS_CONFIG", cfgPath)
	return &freezeOpts{
		g:         &globalOpts{concurrency: 4},
		undo:      undo,
		dryRun:    dryRun,
		newClient: func(context.Context) (freezeClient, error) { return fake, nil },
	}
}

func freezeFake(role string) *fakeFreezeClient {
	return &fakeFreezeClient{
		role:  role,
		repos: []gh.Repo{{Name: "hw1-ada"}, {Name: "hw1-alan"}, {Name: "project-x"}},
		collabs: map[string][]gh.Collaborator{
			"hw1-ada":  {collab("ada", "push"), collab("prof", "admin")},
			"hw1-alan": {collab("alan", "pull")}, // already frozen
		},
	}
}

func TestFreezeDowngradesNonAdmins(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
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

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1"); err != nil {
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

func TestFreezeVerifiesDowngradeTookEffect(t *testing.T) {
	// The API accepts the downgrade but it does not actually take effect. The
	// freeze must re-read, detect the still-open gate, and fail loudly rather than
	// report a deadline lock that never happened.
	fake := freezeFake("admin")
	fake.dontApply = true
	o := newFreezeOpts(t, fake, false, false)

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("freeze should fail when the downgrade did not take, got %v", err)
	}
}

func TestFreezeDryRunMakesNoChanges(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1"); err != nil {
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
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1")
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
}

func TestFreezeNoMatchingRepos(t *testing.T) {
	fake := freezeFake("admin")
	o := newFreezeOpts(t, fake, false, false)
	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "midterm"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no repositories named midterm-*") {
		t.Errorf("expected no-repos message: %s", buf.String())
	}
	if len(fake.changes) != 0 {
		t.Error("nothing should change when no repos match")
	}
}
