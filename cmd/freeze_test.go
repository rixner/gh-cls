package cmd

import (
	"bytes"
	"context"
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

// fakeFreezeState configures a ghtest.Fake for freeze tests and captures what
// it observed.
type fakeFreezeState struct {
	role      string
	repos     []gh.Repo
	collabs   map[string][]gh.Collaborator
	dontApply bool // record the change but leave the permission unchanged

	changes  []string // "repo:user=permission"
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
	// the zero-matches fallback below — which a prefix collision (#1) or a stale
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
	// student work — freeze must never touch it.
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
