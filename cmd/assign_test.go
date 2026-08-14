package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// assignGlobals is the loaded-config state assign and audit tests run against:
// the configured org and staff team, plus the two assignments under test. It
// stands in for what the root's PersistentPreRunE would load from a config file.
func assignGlobals() *globalOpts {
	cfg := &config.Config{
		Org:       "cs101-spring26",
		StaffTeam: "staff",
		Assignments: map[string]config.Assignment{
			"hw1":     {Type: config.TypeIndividual, Template: "hw1-template"}, // bare -> cs101-spring26/hw1-template
			"project": {Type: config.TypeGroup, Template: "project-template"},
		},
	}
	return &globalOpts{cfg: cfg, org: cfg.Org, staffTeam: cfg.StaffTeam, concurrency: 4}
}

const assignRoster = `identifier,username
student-001,ada
student-002,alan
student-003,grace
`

const assignGroups = `group-alpha: [student-001, student-003]
group-beta: [student-002]
`

// fakeAssignClient configures a ghtest.Fake for the assign operations and
// captures what it observed.
type fakeAssignClient struct {
	role           string
	teamMissing    bool            // the staff team does not exist (setup not run)
	unknownUsers   map[string]bool // usernames GitHub reports as non-existent
	userErr        error           // a lookup failure (not a plain 404) from UserExists
	hasIssues      bool
	withholdBranch bool // simulate generation that never lands the default branch
	forcePublic    bool // generation produces public repos regardless of the request
	exists         map[string]bool
	frozen         map[string]freezeState // recorded freeze state per repo
	// freezeAfterRead is merged into frozen once the record has been read, so a
	// second read sees a freeze that landed mid-run.
	freezeAfterRead map[string]freezeState
	noProperty      bool            // org never ran setup
	public          map[string]bool // "owner/name" -> repo is public; absent means private
	invited         []string        // "repo:username" entries modeled as pending invitations
	dropGrants      map[string]bool // usernames whose grant silently evaporates
	branches        []gh.BranchCount
	isTemplate      map[string]bool // "owner/name" -> repo is a template repository

	generated []string
	deleted   []string
	collabs   []string
	perms     []string // "repo:login=permission", to assert what access was granted
	teamRepos []string
	rulesets  map[string]bool // repos a protection ruleset was applied to
	refs      []string        // "repo:ref"
	refSHAs   []string        // "repo:ref@sha" — the SHA each ref was created at
	rebased   []string        // repos whose default branch was rebased onto an empty root
	prs       []string        // "repo:head->base"
	issues    []string        // repo
	enabled   []string        // repos where issues were enabled
}

func (f *fakeAssignClient) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return f.role, nil }
	fk.UserExistsFunc = func(_ context.Context, username string) (bool, error) {
		if f.userErr != nil {
			return false, f.userErr
		}
		return !f.unknownUsers[username], nil
	}
	fk.GetTeamFunc = func(context.Context, string, string) (*gh.Team, bool, error) {
		if f.teamMissing {
			return nil, false, nil
		}
		return &gh.Team{ID: 1}, true, nil
	}
	fk.GetRepoFunc = func(_ context.Context, owner, name string) (*gh.Repo, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		if f.exists[owner+"/"+name] {
			// Repos default to private (the realistic state of an assign-created repo);
			// only those recorded in public are public.
			return &gh.Repo{Name: name, DefaultBranch: "main", HasIssues: f.hasIssues, Private: !f.public[owner+"/"+name], IsTemplate: f.isTemplate[owner+"/"+name]}, true, nil
		}
		return nil, false, nil
	}
	fk.SetRepoTemplateFunc = func(_ context.Context, owner, name string) error {
		fk.Lock()
		defer fk.Unlock()
		if f.isTemplate == nil {
			f.isTemplate = map[string]bool{}
		}
		f.isTemplate[owner+"/"+name] = true
		return nil
	}
	fk.ListBranchesWithCommitCountFunc = func(context.Context, string, string) ([]gh.BranchCount, error) {
		return f.branches, nil
	}
	fk.GenerateFromTemplateFunc = func(_ context.Context, _, _, owner, name string, private, _ bool) error {
		fk.Lock()
		defer fk.Unlock()
		f.exists[owner+"/"+name] = true
		if f.public == nil {
			f.public = map[string]bool{}
		}
		f.public[owner+"/"+name] = !private || f.forcePublic
		f.generated = append(f.generated, name)
		// Generation lands the default branch; record it so the readiness check
		// (waitRepoReady -> BranchExists) sees a populated repo. withholdBranch
		// simulates a generation whose content never appears.
		if !f.withholdBranch {
			f.refs = append(f.refs, name+":refs/heads/main")
		}
		return nil
	}
	fk.DeleteRepoFunc = func(_ context.Context, owner, name string) error {
		fk.Lock()
		defer fk.Unlock()
		delete(f.exists, owner+"/"+name)
		delete(f.public, owner+"/"+name)
		f.deleted = append(f.deleted, name)
		return nil
	}
	fk.AddCollaboratorFunc = func(_ context.Context, _, repo, username, permission string) error {
		fk.Lock()
		defer fk.Unlock()
		f.collabs = append(f.collabs, repo+":"+username)
		f.perms = append(f.perms, repo+":"+username+"="+permission)
		return nil
	}
	fk.ListDirectCollaboratorsFunc = func(_ context.Context, _, repo string) ([]gh.Collaborator, error) {
		fk.Lock()
		defer fk.Unlock()
		var out []gh.Collaborator
		for _, entry := range f.collabs {
			parts := strings.SplitN(entry, ":", 2)
			if len(parts) != 2 || parts[0] != repo {
				continue
			}
			user := parts[1]
			// A dropped grant never lands as a collaborator; an invited user is modeled
			// as a pending invitation instead of live access.
			if f.dropGrants[user] || contains(f.invited, repo+":"+user) {
				continue
			}
			c := gh.Collaborator{Login: user}
			// Reflect the permission actually granted (the last AddCollaborator call
			// for this repo:user), so a "pull" grant comes back holding only pull, not
			// push, faithfully modeling the real API.
			permission := "push"
			for _, p := range f.perms {
				if grantee, perm, ok := strings.Cut(p, "="); ok && grantee == entry {
					permission = perm
				}
			}
			if permission == "pull" {
				c.Permissions.Pull = true
			} else {
				c.Permissions.Push = true
			}
			out = append(out, c)
		}
		return out, nil
	}
	fk.ListRepoInvitationsFunc = func(_ context.Context, _, repo string) ([]gh.Invitation, error) {
		fk.Lock()
		defer fk.Unlock()
		var out []gh.Invitation
		for _, entry := range f.invited {
			parts := strings.SplitN(entry, ":", 2)
			if len(parts) != 2 || parts[0] != repo {
				continue
			}
			var inv gh.Invitation
			inv.Invitee.Login = parts[1]
			out = append(out, inv)
		}
		return out, nil
	}
	fk.GetPropertyDefinitionFunc = func(_ context.Context, _, name string) (*gh.PropertyDefinition, bool, error) {
		if f.noProperty {
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
		out := make(map[string]map[string]string, len(f.frozen))
		for repo, state := range f.frozen {
			out[repo] = map[string]string{frozenProperty: string(state)}
		}
		// Apply any simulated concurrent freeze only after this first read, so the
		// run proceeds on a stale picture exactly as it would in the real race.
		for repo, state := range f.freezeAfterRead {
			f.frozen[repo] = state
		}
		f.freezeAfterRead = nil
		return out, nil
	}
	fk.AddTeamRepoFunc = func(_ context.Context, _, _, _, repo, _ string) error {
		fk.Lock()
		defer fk.Unlock()
		f.teamRepos = append(f.teamRepos, repo)
		return nil
	}
	fk.ApplyRulesetFunc = func(_ context.Context, _, repo string) error {
		fk.Lock()
		defer fk.Unlock()
		if f.rulesets == nil {
			f.rulesets = map[string]bool{}
		}
		f.rulesets[repo] = true
		return nil
	}
	fk.RebaseOntoEmptyRootFunc = func(_ context.Context, _, repo, _ string) (string, error) {
		fk.Lock()
		defer fk.Unlock()
		f.rebased = append(f.rebased, repo)
		return "empty-root-sha", nil
	}
	fk.CreateRefFunc = func(_ context.Context, _, repo, ref, sha string) error {
		fk.Lock()
		defer fk.Unlock()
		f.refs = append(f.refs, repo+":"+ref)
		f.refSHAs = append(f.refSHAs, repo+":"+ref+"@"+sha)
		return nil
	}
	fk.BranchExistsFunc = func(_ context.Context, _, repo, branch string) (bool, error) {
		fk.Lock()
		defer fk.Unlock()
		return contains(f.refs, repo+":refs/heads/"+branch), nil
	}
	fk.CreatePRFunc = func(_ context.Context, _, repo, _, head, base, _ string) error {
		fk.Lock()
		defer fk.Unlock()
		f.prs = append(f.prs, repo+":"+head+"->"+base)
		return nil
	}
	fk.PRExistsFunc = func(_ context.Context, _, repo, base string) (bool, error) {
		fk.Lock()
		defer fk.Unlock()
		for _, p := range f.prs {
			if strings.HasPrefix(p, repo+":") && strings.HasSuffix(p, "->"+base) {
				return true, nil
			}
		}
		return false, nil
	}
	fk.EnableIssuesFunc = func(_ context.Context, _, repo string) error {
		fk.Lock()
		defer fk.Unlock()
		f.enabled = append(f.enabled, repo)
		return nil
	}
	fk.CreateIssueFunc = func(_ context.Context, _, repo, _, _ string) error {
		fk.Lock()
		defer fk.Unlock()
		f.issues = append(f.issues, repo)
		return nil
	}
	fk.IssueExistsFunc = func(_ context.Context, _, repo, _ string) (bool, error) {
		fk.Lock()
		defer fk.Unlock()
		return contains(f.issues, repo), nil
	}
	// The Find lookups answer with the same recorded state as the Exists ones,
	// numbered so a report can name them.
	fk.FindPRByBaseFunc = func(_ context.Context, _, repo, base string) (int, string, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		for i, p := range f.prs {
			if strings.HasPrefix(p, repo+":") && strings.HasSuffix(p, "->"+base) {
				return i + 1, "open", true, nil
			}
		}
		return 0, "", false, nil
	}
	fk.FindIssueByTitleFunc = func(_ context.Context, _, repo, _ string) (int, string, bool, error) {
		fk.Lock()
		defer fk.Unlock()
		for i, r := range f.issues {
			if r == repo {
				return i + 1, "open", true, nil
			}
		}
		return 0, "", false, nil
	}
	return fk
}

func newFakeAssign(role string) *fakeAssignClient {
	return &fakeAssignClient{
		role:       role,
		exists:     map[string]bool{"cs101-spring26/hw1-template": true, "cs101-spring26/project-template": true},
		isTemplate: map[string]bool{"cs101-spring26/hw1-template": true, "cs101-spring26/project-template": true},
		branches:   []gh.BranchCount{{Name: "main", Commits: 1}},
		frozen:     map[string]freezeState{},
	}
}

func boolp(b bool) *bool    { return &b }
func strp(s string) *string { return &s }

// newAssignOpts wires assignOpts to a fake; the roster/groups files live in a
// temp dir and the config comes from assignGlobals.
func newAssignOpts(t *testing.T, fake *fakeAssignClient, rosterCSV, groupsYML string) *assignOpts {
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
	return &assignOpts{
		g:         assignGlobals(),
		roster:    rosterPath,
		groups:    groupsPath,
		newClient: func(context.Context) (assignClient, error) { return fk, nil },
		sleep:     func(time.Duration) {},
	}
}

func contains(haystack []string, needle string) bool {
	return count(haystack, needle) > 0
}

func count(haystack []string, needle string) int {
	n := 0
	for _, h := range haystack {
		if h == needle {
			n++
		}
	}
	return n
}

func TestAssignIndividual(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"hw1-ada", "hw1-alan", "hw1-grace"} {
		if !contains(fake.generated, repo) {
			t.Errorf("missing generated repo %q (got %v)", repo, fake.generated)
		}
		if !contains(fake.teamRepos, repo) {
			t.Errorf("staff team not granted on %q", repo)
		}
	}
	if !contains(fake.collabs, "hw1-ada:ada") {
		t.Errorf("student push grant missing: %v", fake.collabs)
	}
	if !strings.Contains(buf.String(), "3 created") {
		t.Errorf("summary wrong: %s", buf.String())
	}
}

func TestAssignGroup(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, assignGroups)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.generated, "project-group-alpha") || !contains(fake.generated, "project-group-beta") {
		t.Errorf("group repos not generated: %v", fake.generated)
	}
	// group-alpha resolves student-001 and student-003 to ada and grace.
	if !contains(fake.collabs, "project-group-alpha:ada") || !contains(fake.collabs, "project-group-alpha:grace") {
		t.Errorf("group members not granted: %v", fake.collabs)
	}
}

func TestAssignGroupRequiresGroups(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "--groups is required") {
		t.Fatalf("group without groups should error, got %v", err)
	}
}

func TestAssignIndividualRejectsGroups(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, assignGroups)
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("individual with groups should error, got %v", err)
	}
}

func TestAssignTemplateMissing(t *testing.T) {
	fake := newFakeAssign("admin")
	delete(fake.exists, "cs101-spring26/hw1-template")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("missing template should error, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("no repos should be generated when the template is missing")
	}
}

func TestAssignStaffTeamMissing(t *testing.T) {
	// The staff team is granted on every repo, so a missing group must abort before
	// any repo is generated, with guidance to run setup.
	fake := newFakeAssign("admin")
	fake.teamMissing = true
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("a missing staff team should fail pointing at setup, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("no repos should be generated when the staff team is missing")
	}
}

func TestAssignTemplateNotATemplateRepo(t *testing.T) {
	// The template repo exists but is not a GitHub template repository, and
	// --mark-template was not given: fail with guidance, generate nothing.
	fake := newFakeAssign("admin")
	delete(fake.isTemplate, "cs101-spring26/hw1-template")
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "--mark-template") {
		t.Fatalf("a non-template template repo should fail pointing at --mark-template, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("nothing should be generated when the template is not a template repo")
	}
}

func TestAssignMarkTemplate(t *testing.T) {
	// --mark-template opts into marking the template repo, then proceeds.
	fake := newFakeAssign("admin")
	delete(fake.isTemplate, "cs101-spring26/hw1-template")
	o := newAssignOpts(t, fake, assignRoster, "")
	o.markTemplate = true

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if !fake.isTemplate["cs101-spring26/hw1-template"] {
		t.Error("--mark-template should mark the template repo a template repository")
	}
	if len(fake.generated) == 0 {
		t.Error("generation should proceed after marking the template")
	}
}

func TestAssignUnsquashedAborts(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.branches = []gh.BranchCount{{Name: "main", Commits: 1}, {Name: "solution", Commits: 4}}
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "not fully squashed") {
		t.Fatalf("unsquashed template should abort, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Error("no repos should be generated when the template is unsquashed")
	}

	// With --allow-unsquashed it proceeds.
	o.allowUnsquashed = true
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{}); err != nil {
		t.Fatalf("--allow-unsquashed should proceed, got %v", err)
	}
	if len(fake.generated) == 0 {
		t.Error("repos should be generated when unsquashed is allowed")
	}
}

func TestAssignIdempotentSkip(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true // already created
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if contains(fake.generated, "hw1-ada") {
		t.Error("existing repo should be skipped for generation")
	}
	// Grants are still re-asserted on the skipped repo, and since it carries no
	// freeze record the re-assert is push. (The old "re-asserted push" warning is
	// gone: assign now consults the record instead of warning that it might have
	// undone a freeze. See TestAssignKeepsFrozenReposFrozen.)
	if !contains(fake.collabs, "hw1-ada:ada") {
		t.Error("grants should be re-asserted on a skipped repo")
	}
	if !contains(fake.perms, "hw1-ada:ada=push") {
		t.Errorf("an unfrozen repo should be re-asserted at push: %v", fake.perms)
	}
	out := buf.String()
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("skip summary missing: %s", out)
	}
	if strings.Contains(out, "recorded frozen") {
		t.Errorf("nothing is frozen, so no frozen note belongs: %s", out)
	}
}

func TestAssignRealRunAnnouncesWhatItIsProvisioning(t *testing.T) {
	// assign mutates a whole class of repositories and, before this, said nothing
	// until the results summary: the org and template it was aimed at never
	// appeared. With a config per semester and $GH_CLS_CONFIG able to point at
	// either, that line is the last chance to notice the wrong one.
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	first := strings.SplitN(buf.String(), "\n", 2)[0]
	want := "Provisioning 3 private repo(s) in cs101-spring26 from cs101-spring26/hw1-template"
	if first != want {
		t.Errorf("first line = %q, want %q", first, want)
	}
}

func TestAssignDryRunReportsPerRepoTruth(t *testing.T) {
	// The old dry run returned before the client was built: it counted existing
	// repos as "would create" and claimed push for members of a frozen repo, which
	// is the opposite of what a real run does.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.frozen["hw1-ada"] = freezeFrozen
	fake.exists["cs101-spring26/hw1-alan"] = true
	o := newAssignOpts(t, fake, assignRoster, "")
	o.dryRun = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"DRY RUN: no changes will be made",
		"Provisioning 3 private repo(s) in cs101-spring26 from cs101-spring26/hw1-template",
		"skip    hw1-ada    read: ada (exists, recorded frozen)",
		"skip    hw1-alan   push: alan (exists)",
		"create  hw1-grace  push: grace",
		"1 would be created, 2 already exist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run should report %q:\n%s", want, out)
		}
	}
	// Nothing was touched.
	if len(fake.generated) > 0 || len(fake.collabs) > 0 || len(fake.teamRepos) > 0 {
		t.Errorf("a dry run must mutate nothing: generated=%v collabs=%v teams=%v", fake.generated, fake.collabs, fake.teamRepos)
	}
}

func TestAssignDryRunFlagsARepoTheRunWouldRefuse(t *testing.T) {
	// A reused repo whose visibility disagrees with the policy is aborted by a
	// real run before any grant (TestAssignRejectsExistingPublicRepo). Previewing
	// it as an ordinary skip would promise access re-assertion that the run then
	// refuses, so the dry run reports it and exits non-zero, as the run does.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.public = map[string]bool{"cs101-spring26/hw1-ada": true}
	o := newAssignOpts(t, fake, assignRoster, "")
	o.dryRun = true

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "would be refused") {
		t.Fatalf("a repo the run would refuse should fail the dry run, got %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"ABORT   hw1-ada    public but private was requested, so a run would refuse it before granting access",
		"2 would be created, 0 already exist, 1 would be refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run should report %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "push: ada") {
		t.Errorf("a refused repo must not be previewed as a routine grant:\n%s", out)
	}
}

func TestAssignDryRunRunsThePreflights(t *testing.T) {
	// A dry run that skips the preflights reports a plan that a real run would
	// refuse to carry out, which is worse than no preview at all.
	cases := map[string]struct {
		setup func(*fakeAssignClient)
		want  string
	}{
		"missing staff team": {
			func(f *fakeAssignClient) { f.teamMissing = true },
			"staff team",
		},
		"template is not a template repository": {
			func(f *fakeAssignClient) { f.isTemplate["cs101-spring26/hw1-template"] = false },
			"not a template repository",
		},
		"unsquashed template": {
			func(f *fakeAssignClient) {
				f.branches = []gh.BranchCount{{Name: "main", Commits: 4}}
			},
			"not fully squashed",
		},
		"roster username that does not exist": {
			func(f *fakeAssignClient) { f.unknownUsers = map[string]bool{"grace": true} },
			"do not exist",
		},
		"the freeze record was never declared": {
			func(f *fakeAssignClient) { f.noProperty = true },
			"run `gh cls setup`",
		},
	}
	for name, tc := range cases {
		fake := newFakeAssign("admin")
		tc.setup(fake)
		o := newAssignOpts(t, fake, assignRoster, "")
		o.dryRun = true

		err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
		if err == nil {
			t.Errorf("%s: a dry run should surface this before reporting a plan", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error should mention %q, got %v", name, tc.want, err)
		}
	}
}

func TestAssignDryRunLeavesTheTemplateUnmarked(t *testing.T) {
	// --mark-template is a mutation on the template repo, so a dry run reports it
	// instead of doing it.
	fake := newFakeAssign("admin")
	fake.isTemplate["cs101-spring26/hw1-template"] = false
	o := newAssignOpts(t, fake, assignRoster, "")
	o.dryRun = true
	o.markTemplate = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "marking cs101-spring26/hw1-template a template repository first") {
		t.Errorf("the dry run should report the pending mark:\n%s", buf.String())
	}
	if fake.isTemplate["cs101-spring26/hw1-template"] {
		t.Error("a dry run must not mark the template")
	}
}

func TestAssignKeepsFrozenReposFrozen(t *testing.T) {
	// assign is idempotent and re-run freely, and it re-asserts grants on existing
	// repos. Doing that unconditionally at push meant adding one late student
	// after a deadline handed write back to the whole assignment. The grant now
	// follows each repo's recorded freeze state.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/project-group-alpha"] = true
	fake.frozen["project-group-alpha"] = freezeFrozen
	o := newAssignOpts(t, fake, assignRoster, assignGroups)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", config.Overrides{}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	for _, want := range []string{"project-group-alpha:ada=pull", "project-group-alpha:grace=pull"} {
		if !contains(fake.perms, want) {
			t.Errorf("a frozen repo's members must be re-asserted at read: %v", fake.perms)
		}
	}
	for _, p := range fake.perms {
		if strings.HasPrefix(p, "project-group-alpha:") && strings.HasSuffix(p, "=push") {
			t.Errorf("no push may be granted on a frozen repo: %v", fake.perms)
		}
	}
	// group-beta is newly created, so it is not frozen and gets push as usual.
	if !contains(fake.perms, "project-group-beta:alan=push") {
		t.Errorf("a freshly created repo should get push: %v", fake.perms)
	}
	if !strings.Contains(buf.String(), "1 existing repo(s) are recorded frozen") {
		t.Errorf("the run should say it withheld write:\n%s", buf.String())
	}
}

func TestAssignVerifiesFrozenRepoAlreadyHoldingPull(t *testing.T) {
	// Re-running assign against a repo recorded frozen, whose members already hold
	// pull, must succeed: the post-condition check must accept a pull grant as
	// live access rather than demanding push on a repo that is supposed to stay
	// read-only. Every member re-asserted at pull is exactly this state.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/project-group-alpha"] = true
	fake.frozen["project-group-alpha"] = freezeFrozen
	o := newAssignOpts(t, fake, assignRoster, assignGroups)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", config.Overrides{}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "recorded frozen") {
		t.Errorf("the frozen note should be reported: %s", out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("no repo should fail verification when frozen members hold pull: %s", out)
	}
}

func TestAssignVerifiesGrantTookEffectOnFrozenRepo(t *testing.T) {
	// A member with no access at all on a frozen repo must still fail
	// verification, even though the grant is only "pull": dropped means dropped,
	// regardless of which permission was expected.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/project-group-alpha"] = true
	fake.frozen["project-group-alpha"] = freezeFrozen
	fake.dropGrants = map[string]bool{"ada": true}
	o := newAssignOpts(t, fake, assignRoster, assignGroups)

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a dropped grant on a frozen repo should fail the run, got %v", err)
	}
	if !strings.Contains(buf.String(), "pull grant to ada") {
		t.Errorf("the failure should name the pull grant that did not take: %s", buf.String())
	}
}

func TestAssignDetectsAConcurrentFreeze(t *testing.T) {
	// A freeze that starts after assign read the record is invisible to it, so
	// assign grants push on a repo that is being locked. It cannot prevent that
	// (there is no atomic compare-and-set across the property and the grant), but
	// it must not report a clean run: the re-read afterwards catches it and names
	// the repos to re-freeze.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	// The record is empty when assign reads it and frozen when it re-reads.
	fake.freezeAfterRead = map[string]freezeState{"hw1-ada": freezeFrozen}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil {
		t.Fatalf("a concurrent freeze must not be reported as a clean run:\n%s", buf.String())
	}
	for _, want := range []string{"hw1-ada", "gh cls freeze hw1", "same time"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name the repo and the fix, got: %v", err)
		}
	}
}

func TestAssignReportsAFailureAndAConcurrentFreezeTogether(t *testing.T) {
	// Returning on the first failed repo discarded the race result, so one
	// unrelated failure hid "other repos are writable past their deadline" in
	// exactly the messy run where a freeze is most likely to be racing. Both must
	// be reported.
	fake := newFakeAssign("admin")
	fake.dropGrants = map[string]bool{"ada": true}                          // hw1-ada fails verification
	fake.freezeAfterRead = map[string]freezeState{"hw1-alan": freezeFrozen} // a freeze lands mid-run
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil {
		t.Fatalf("both problems must surface:\n%s", buf.String())
	}
	for _, want := range []string{"1 repo(s) failed", "hw1-alan", "gh cls freeze hw1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got: %v", want, err)
		}
	}
	// The per-repo detail still reaches the terminal.
	if !strings.Contains(buf.String(), "FAILED hw1-ada") {
		t.Errorf("the failed repo should still be named in the report:\n%s", buf.String())
	}
}

func TestAssignRaceCheckCoversAGrantThatLandedBeforeALaterFailure(t *testing.T) {
	// The grant is what makes a repo writable, and it lands before the steps that
	// follow it. Excluding a repo because a later step failed dropped the one repo
	// that is both writable and known to be in a bad state from the race check.
	fake := newFakeAssign("admin")
	fake.dropGrants = map[string]bool{"ada": true} // hw1-ada: push granted, verification then fails
	fake.freezeAfterRead = map[string]freezeState{"hw1-ada": freezeFrozen}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil {
		t.Fatalf("the concurrent freeze must be reported:\n%s", buf.String())
	}
	if !contains(fake.perms, "hw1-ada:ada=push") {
		t.Fatalf("the test needs the push grant to have landed: %v", fake.perms)
	}
	if !strings.Contains(err.Error(), "writable past the deadline") || !strings.Contains(err.Error(), "hw1-ada") {
		t.Errorf("a repo granted write before its later failure must still be race-checked, got: %v", err)
	}
}

func TestAssignRefusesASecondFeedbackArtifact(t *testing.T) {
	// The config declares what to create; it is not a record of what exists. An
	// assignment whose feedback is changed after its repos were made would
	// otherwise get an issue added to every repo that already carries a feedback
	// pull request, splitting the class between two artifacts with nothing saying
	// which one a student read.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/main", "hw1-ada:refs/heads/feedback"}
	fake.prs = []string{"hw1-ada:main->feedback"} // the PR an earlier pr-configured run made
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	// The config now says issue.
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{Feedback: strp(config.FeedbackIssue)})
	if err == nil {
		t.Fatalf("adding an issue to a repo that has a feedback PR must fail that repo:\n%s", buf.String())
	}
	if contains(fake.issues, "hw1-ada") {
		t.Error("no second artifact should be created")
	}
	out := buf.String()
	for _, want := range []string{
		"already has a feedback pull request (#1)",
		"now configured for a feedback issue",
		"will not add a second one",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure should say %q:\n%s", want, out)
		}
	}
	// The repos that carry nothing yet are unaffected: they get the issue.
	if !contains(fake.issues, "hw1-alan") || !contains(fake.issues, "hw1-grace") {
		t.Errorf("repos with no artifact should still get one: %v", fake.issues)
	}
}

func TestAssignKeepsTheArtifactARepoAlreadyHas(t *testing.T) {
	// Re-running assign on an assignment whose repos already carry the configured
	// artifact must be a no-op, not a second create.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.hasIssues = true
	fake.issues = []string{"hw1-ada"}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{Feedback: strp(config.FeedbackIssue)}); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if count(fake.issues, "hw1-ada") != 1 {
		t.Errorf("the existing issue should be left alone, got %v", fake.issues)
	}
}

func TestAssignExplainsAFeedbackBranchItCannotAdd(t *testing.T) {
	// The state an assign run leaves if it dies between generating the repo and
	// creating the feedback branch: the repo exists, its default branch is there,
	// the feedback branch is not. Re-running cannot repair it, because building
	// the branch rewrites the default branch's history. The refusal is right, but
	// the instructor is the one who has to act, so the message has to say how.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/main"} // no refs/heads/feedback
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{Feedback: strp(config.FeedbackPR)})
	if err == nil {
		t.Fatalf("a missing feedback branch on an existing repo must fail that repo:\n%s", buf.String())
	}
	msg := buf.String()
	if !strings.Contains(msg, "FAILED hw1-ada") {
		t.Fatalf("the repo should be reported as failed:\n%s", msg)
	}
	// Both ways out, since the tool cannot tell which repo this is: delete and
	// re-run when it came from an interrupted run, restore the branch when it
	// holds student work. Switching the assignment to issues is called out as a
	// non-answer: it would leave every other repo carrying two artifacts, and the
	// config describing something other than what exists.
	for _, want := range []string{
		"gh repo delete cs101-spring26/hw1-ada",
		"before anyone is granted access",
		"no student is a collaborator on it",
		"do not delete it",
		"restore the feedback branch from its pull request",
		"Do not switch the assignment to feedback: issue",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message should contain %q:\n%s", want, msg)
		}
	}
	// The refusal happens before any grant, which is what makes the delete advice
	// true in the first place.
	if contains(fake.collabs, "hw1-ada:ada") || contains(fake.rebased, "hw1-ada") {
		t.Errorf("nothing should be granted or rewritten on the refused repo: collabs=%v rebased=%v", fake.collabs, fake.rebased)
	}
}

func TestAssignAbortsWithoutTheFreezeProperty(t *testing.T) {
	// Without the record assign cannot tell whether re-asserting push would
	// reopen a frozen assignment, so it refuses rather than guessing.
	fake := newFakeAssign("admin")
	fake.noProperty = true
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "gh cls setup") {
		t.Fatalf("assign should abort and name the fix, got %v", err)
	}
	if len(fake.generated) != 0 || len(fake.collabs) != 0 {
		t.Errorf("nothing may be created or granted when the freeze state is unknown: %v %v", fake.generated, fake.collabs)
	}
}

func TestAssignUnknownGroupMember(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "group-x: [student-999]\n")
	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "student-999") {
		t.Fatalf("unknown group member should be a hard error, got %v", err)
	}
}

func TestAssignRejectsBogusRosterUser(t *testing.T) {
	// A roster username that does not exist on GitHub must abort before any repo is
	// generated, rather than surfacing only when the invite fails and leaving a
	// stray repo behind.
	fake := newFakeAssign("admin")
	fake.unknownUsers = map[string]bool{"alan": true}
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "alan") {
		t.Fatalf("a bogus roster username should abort naming it, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Errorf("no repos should be generated when a roster username is bogus: %v", fake.generated)
	}
}

func TestAssignReportsAllBogusRosterUsers(t *testing.T) {
	// Every non-existent handle is reported together so the whole roster can be
	// fixed in one pass.
	fake := newFakeAssign("admin")
	fake.unknownUsers = map[string]bool{"ada": true, "grace": true}
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "ada") || !strings.Contains(err.Error(), "grace") {
		t.Fatalf("all bogus usernames should be reported together, got %v", err)
	}
}

func TestAssignUserLookupErrorAborts(t *testing.T) {
	// A lookup failure (not a plain 404) must abort rather than be mistaken for a
	// bogus username and let a real student be skipped.
	fake := newFakeAssign("admin")
	fake.userErr = errors.New("boom")
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "validating roster username") {
		t.Fatalf("a lookup error should abort the run, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Errorf("no repos should be generated when a lookup errors: %v", fake.generated)
	}
}

func TestAssignRejectsStudentOnNoGroup(t *testing.T) {
	// student-003 (grace) is in no group: assign must abort before creating anything.
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "group-alpha: [student-001]\ngroup-beta: [student-002]\n")

	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "student-003") || !strings.Contains(err.Error(), "no group") {
		t.Fatalf("a student in no group should abort naming them, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Errorf("no repos should be generated when a student is in no group: %v", fake.generated)
	}
}

func TestAssignRejectsStudentOnMultipleGroups(t *testing.T) {
	// student-001 (ada) is in both group-alpha and group-beta: abort before creating.
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "group-alpha: [student-001, student-003]\ngroup-beta: [student-001, student-002]\n")

	err := o.run(context.Background(), &bytes.Buffer{}, "project", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "student-001") || !strings.Contains(err.Error(), "more than one group") {
		t.Fatalf("a student in multiple groups should abort naming them, got %v", err)
	}
	if len(fake.generated) != 0 {
		t.Errorf("no repos should be generated when a student is in multiple groups: %v", fake.generated)
	}
}

func TestAssignForceProceedsPastGroupProblems(t *testing.T) {
	// --force downgrades the roster/groups inconsistency to a warning and proceeds.
	// student-003 is in no group, so only group-alpha and group-beta are created.
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "group-alpha: [student-001]\ngroup-beta: [student-002]\n")
	o.force = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "project", config.Overrides{}); err != nil {
		t.Fatalf("--force should proceed, got %v", err)
	}
	if !contains(fake.generated, "project-group-alpha") || !contains(fake.generated, "project-group-beta") {
		t.Errorf("--force should create the well-formed groups' repos: %v", fake.generated)
	}
	out := buf.String()
	if !strings.Contains(out, "--force") || !strings.Contains(out, "student-003") {
		t.Errorf("the force warning should name the skipped-over problem: %s", out)
	}
}

func TestAssignOwnerGuard(t *testing.T) {
	fake := newFakeAssign("member")
	o := newAssignOpts(t, fake, assignRoster, "")
	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
}

func TestAssignBranchProtection(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{BranchProtection: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	if !fake.rulesets["hw1-ada"] {
		t.Errorf("ruleset not applied: %v", fake.rulesets)
	}
	if len(fake.rulesets) != 3 {
		t.Errorf("expected a ruleset on each of 3 repos, got %d", len(fake.rulesets))
	}
}

func TestAssignFeedbackPR(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.refs, "hw1-ada:refs/heads/feedback") {
		t.Errorf("feedback branch not created: %v", fake.refs)
	}
	// Divergence guard: the default branch is rebased onto an empty root and the
	// feedback branch points at that root, so the two share history (the PR opens)
	// with an empty merge base (the whole project is the diff). A branch left at
	// the starter commit would be identical to main -> "No commits between" and no
	// PR; a detached orphan would share no history -> "no history in common".
	if !contains(fake.rebased, "hw1-ada") {
		t.Errorf("default branch should be rebased onto an empty root: %v", fake.rebased)
	}
	if !contains(fake.refSHAs, "hw1-ada:refs/heads/feedback@empty-root-sha") {
		t.Errorf("feedback branch must point at the empty root: %v", fake.refSHAs)
	}
	if !contains(fake.prs, "hw1-ada:main->feedback") {
		t.Errorf("feedback PR not opened with base feedback: %v", fake.prs)
	}
	if len(fake.issues) != 0 {
		t.Error("pr mode should not open issues")
	}
}

func TestAssignFeedbackIssueEnablesWhenNeeded(t *testing.T) {
	// Template has issues disabled: assign must enable them first.
	fake := newFakeAssign("admin")
	fake.hasIssues = false
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.enabled, "hw1-ada") {
		t.Errorf("issues should be enabled when the template has them off: %v", fake.enabled)
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Errorf("feedback issue not opened: %v", fake.issues)
	}
	if len(fake.prs) != 0 {
		t.Error("issue mode should not open PRs")
	}
}

func TestAssignFeedbackIssueSkipsEnableWhenOn(t *testing.T) {
	fake := newFakeAssign("admin")
	fake.hasIssues = true
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if len(fake.enabled) != 0 {
		t.Errorf("issues already on: should not re-enable, got %v", fake.enabled)
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Error("feedback issue should still be opened")
	}
}

func TestAssignProtectionAndFeedbackReconciled(t *testing.T) {
	// An existing repo is reused: both branch protection and the feedback artifact
	// are reconciled. Re-applying protection (ApplyRuleset is idempotent) repairs a
	// repo that was created but never protected on a prior partial run, instead of
	// leaving it permanently unprotected.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	o := newAssignOpts(t, fake, assignRoster, "")

	ov := config.Overrides{BranchProtection: boolp(true), Feedback: strp("issue")}
	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", ov); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.rulesets["hw1-ada"]; !ok {
		t.Error("branch protection should be reconciled (re-applied) on a reused repo")
	}
	if !contains(fake.issues, "hw1-ada") {
		t.Error("feedback should be reconciled on a reused repo that lacks it")
	}
	// Brand-new repos in the same run get both protection and feedback.
	if _, ok := fake.rulesets["hw1-alan"]; !ok {
		t.Error("new repos should still get protection")
	}
	if !contains(fake.issues, "hw1-alan") {
		t.Error("new repos should get feedback")
	}
}

func TestAssignFeedbackPRIdempotent(t *testing.T) {
	// A reused repo already has its feedback branch and PR: neither is recreated,
	// so a closed PR is never reopened.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/feedback"}
	fake.prs = []string{"hw1-ada:main->feedback"}
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.refs, "hw1-ada:refs/heads/feedback") != 1 {
		t.Errorf("existing feedback branch should not be recreated: %v", fake.refs)
	}
	if count(fake.prs, "hw1-ada:main->feedback") != 1 {
		t.Errorf("existing feedback PR should not be reopened: %v", fake.prs)
	}
}

func TestAssignFeedbackPRRecoversMissingPR(t *testing.T) {
	// A prior run created the feedback branch but failed before opening the PR;
	// the re-run opens only the missing PR.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.refs = []string{"hw1-ada:refs/heads/feedback"} // branch present, no PR
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("pr")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.refs, "hw1-ada:refs/heads/feedback") != 1 {
		t.Errorf("branch should not be recreated: %v", fake.refs)
	}
	if !contains(fake.prs, "hw1-ada:main->feedback") {
		t.Errorf("missing feedback PR should be opened on re-run: %v", fake.prs)
	}
}

func TestAssignFeedbackIssueIdempotent(t *testing.T) {
	// A reused repo already has its feedback issue: no duplicate is opened.
	fake := newFakeAssign("admin")
	fake.hasIssues = true
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.issues = []string{"hw1-ada"}
	o := newAssignOpts(t, fake, assignRoster, "")

	if err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{Feedback: strp("issue")}); err != nil {
		t.Fatal(err)
	}
	if count(fake.issues, "hw1-ada") != 1 {
		t.Errorf("existing feedback issue should not be duplicated: %v", fake.issues)
	}
}

func TestAssignWaitsForContent(t *testing.T) {
	// A generated repo whose default branch never lands must be reported as a
	// failure, not silently treated as ready (which would let assign create a
	// feedback ref against, or grant access to, an empty shell).
	fake := newFakeAssign("admin")
	fake.withholdBranch = true
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a repo that never becomes ready should fail the run, got %v", err)
	}
	// No grants should be asserted on a repo that never became ready.
	if len(fake.collabs) != 0 {
		t.Errorf("no access should be granted before the repo is ready: %v", fake.collabs)
	}
}

func TestCheckVisibility(t *testing.T) {
	// Lock every polarity: the check passes only when the repo's visibility
	// matches the policy. Guards the easy-to-invert Private/wantPublic comparison.
	cases := []struct {
		name       string
		private    bool
		wantPublic bool
		match      bool
	}{
		{"private repo, private wanted", true, false, true},
		{"public repo, private wanted", false, false, false},
		{"public repo, public wanted", false, true, true},
		{"private repo, public wanted", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkVisibility("hw1-ada", &gh.Repo{Private: tc.private}, tc.wantPublic)
			if tc.match && err != nil {
				t.Errorf("matching visibility should pass, got %v", err)
			}
			if !tc.match && err == nil {
				t.Error("mismatched visibility should error, got nil")
			}
		})
	}
}

func TestAssignVerifiesVisibility(t *testing.T) {
	// A private assignment whose repos come out public would expose student work.
	// assign must catch the mismatch and fail before granting any access.
	fake := newFakeAssign("admin")
	fake.forcePublic = true
	o := newAssignOpts(t, fake, assignRoster, "")

	err := o.run(context.Background(), &bytes.Buffer{}, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a visibility mismatch should fail the run, got %v", err)
	}
	if len(fake.collabs) != 0 {
		t.Errorf("no access should be granted on a wrongly-public repo: %v", fake.collabs)
	}
	// Each leaked repo we just created is rolled back rather than left behind.
	if len(fake.deleted) != 3 {
		t.Errorf("wrongly-public just-created repos should be deleted, got %v", fake.deleted)
	}
}

func TestAssignRejectsExistingPublicRepo(t *testing.T) {
	// A reused repo that is public (drift, or a prior leaky run) must abort before
	// access is re-asserted, just like a freshly-generated public repo would.
	fake := newFakeAssign("admin")
	fake.exists["cs101-spring26/hw1-ada"] = true
	fake.public = map[string]bool{"cs101-spring26/hw1-ada": true}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a public reused repo should fail the run, got %v", err)
	}
	if contains(fake.collabs, "hw1-ada:ada") {
		t.Errorf("no access should be re-asserted on a wrongly-public repo: %v", fake.collabs)
	}
	if !strings.Contains(buf.String(), "is public but private was requested") {
		t.Errorf("visibility mismatch should be reported clearly: %s", buf.String())
	}
	// A reused repo must never be deleted: it may already hold student work.
	if contains(fake.deleted, "hw1-ada") || !fake.exists["cs101-spring26/hw1-ada"] {
		t.Errorf("a reused public repo must not be deleted, got deleted=%v", fake.deleted)
	}
}

func TestAssignReportsPendingInvitations(t *testing.T) {
	// An outside collaborator's grant becomes a pending invitation: the run still
	// succeeds, but reports that the student must accept before they can push.
	fake := newFakeAssign("admin")
	fake.invited = []string{"hw1-ada:ada"}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "pending") {
		t.Errorf("pending invitation should be reported: %s", buf.String())
	}
}

func TestAssignVerifiesGrantTookEffect(t *testing.T) {
	// A grant that lands as neither access nor an invitation is a silent failure;
	// the post-condition check must catch it and fail the repo loudly.
	fake := newFakeAssign("admin")
	fake.dropGrants = map[string]bool{"ada": true}
	o := newAssignOpts(t, fake, assignRoster, "")

	var buf bytes.Buffer
	err := o.run(context.Background(), &buf, "hw1", config.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a grant that did not take effect should fail the run, got %v", err)
	}
	if !strings.Contains(buf.String(), "did not take effect") {
		t.Errorf("the failure should explain the grant did not take effect: %s", buf.String())
	}
}

func TestAssignDryRun(t *testing.T) {
	fake := newFakeAssign("admin")
	o := newAssignOpts(t, fake, assignRoster, "")
	o.dryRun = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf, "hw1", config.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.generated) != 0 {
		t.Error("dry-run must not generate repos")
	}
	out := buf.String()
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "hw1-ada") {
		t.Errorf("dry-run plan missing: %s", out)
	}
}
