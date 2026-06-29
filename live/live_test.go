//go:build live

// Package live holds an opt-in, end-to-end test that drives the real `gh cls`
// commands against a real, disposable GitHub organization. It is excluded from a
// normal `go test ./...` by the `live` build tag, so it can never reach the
// network unless run deliberately:
//
//	go test -tags live -run TestLive -timeout 20m ./live/
//
// Auth is inherited from the `gh` CLI exactly as in production — the test never
// reads or sets a token. The `gh` login that runs it must:
//   - own the org named by GH_CLS_LIVE_ORG (an organization owner), and
//   - carry the admin:org and delete_repo scopes (the latter for teardown):
//     gh auth refresh -s admin:org -s delete_repo
//
// Every command runs purely against the GitHub API (template included), so no
// git binary or credential helper is involved.
//
// Environment (selectors, not auth):
//   - GH_CLS_LIVE_ORG  (required) the disposable org to operate in; also the
//     on/off switch — the test skips when it is unset.
//   - GH_CLS_STUDENT1  (required) a GitHub login to enroll as the student. For
//     the freeze downgrade assertions to run, this account must be a *member* of
//     the org (accept the org invite once); an unaccepted outside collaborator
//     does not appear in the repo's direct-collaborator list, in which case the
//     freeze assertions are skipped (but freeze/undo still run).
//   - GH_CLS_STUDENT2  (optional) a second member login, added to the group
//     team for extra coverage.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/rixner/gh-cls/cmd"
	"github.com/rixner/gh-cls/gh"
)

func TestLive(t *testing.T) {
	org := os.Getenv("GH_CLS_LIVE_ORG")
	if org == "" {
		t.Skip("set GH_CLS_LIVE_ORG to a disposable org you own to run the live test")
	}
	student1 := os.Getenv("GH_CLS_STUDENT1")
	if student1 == "" {
		t.Skip("set GH_CLS_STUDENT1 to a GitHub login (ideally an org member) to run the live test")
	}
	student2 := os.Getenv("GH_CLS_STUDENT2") // optional

	ctx := context.Background()

	client, err := gh.New()
	if err != nil {
		t.Fatalf("building gh client (is `gh` authenticated?): %v", err)
	}
	rc, err := api.DefaultRESTClient()
	if err != nil {
		t.Fatalf("building go-gh REST client: %v", err)
	}

	// Unique per-run names so repeated or crashed runs never collide. The source
	// uses a distinct prefix so it is not swept up by the <name>-* operations of
	// template/assign/freeze.
	ts := time.Now().UTC().Format("20060102t150405")
	name := "ghclslive" + ts   // individual assignment
	grp := "ghclslivegrp" + ts // group assignment
	srcName := "ghclssrc" + ts // source template to generate from

	// Tear everything down even on failure or panic. Registered before any repo
	// is created so partial runs still clean up. Best-effort: log, never fail.
	t.Cleanup(func() {
		cctx := context.Background()
		for _, prefix := range []string{name + "-", grp + "-"} {
			repos, err := client.ListOrgReposByPrefix(cctx, org, prefix)
			if err != nil {
				t.Logf("cleanup: listing %s* in %s: %v", prefix, org, err)
				continue
			}
			for _, r := range repos {
				if err := client.DeleteRepo(cctx, org, r.Name); err != nil {
					t.Logf("cleanup: deleting %s/%s: %v", org, r.Name, err)
				}
			}
		}
		if err := client.DeleteRepo(cctx, org, srcName); err != nil {
			t.Logf("cleanup: deleting %s/%s: %v", org, srcName, err)
		}
		// The staff team is intentionally left in place: there is no delete-team
		// primitive, and setup is idempotent so its presence never breaks a re-run.
	})

	// Hermetic, user-authored config: GH_CLS_CONFIG points the CLI at a throwaway
	// file so the developer's real config is never touched. It is written before
	// any command runs, since every command (setup included) reads the org and
	// staff team from it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gh-cls-test.yml")
	t.Setenv("GH_CLS_CONFIG", cfgPath)
	writeConfig(t, cfgPath, org, name, grp)

	// 0. Seed a source template with content: a repo with at least one commit for
	// `template` to generate from. Created with auto_init via the raw client so it
	// has a README commit (CreateOrgRepo deliberately makes empty repos).
	seedSource(t, rc, org, srcName)

	// 1. setup — harden the org, then verify, then prove idempotency.
	mustRunCLI(t, ctx, "setup")
	assertOrgHardened(t, ctx, client, org)
	out := mustRunCLI(t, ctx, "setup")
	if !strings.Contains(out, "already") {
		t.Errorf("re-running setup should report 'already' for hardened settings, got:\n%s", out)
	}

	// 2. template — build the squashed template repo, verify, then confirm the
	// overwrite guard (no -F errors; -F recreates). --mark-source flags the
	// freshly-seeded source a template repository (the pre-req to generate from it);
	// later calls find it already marked.
	mustRunCLI(t, ctx, "template", name+"-template", "-s", org+"/"+srcName, "--mark-source")
	assertTemplate(t, ctx, client, org, name+"-template")
	if _, err := runCLI(ctx, "template", name+"-template", "-s", org+"/"+srcName); err == nil {
		t.Error("re-running template without -F should error (template already exists)")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("template re-run error = %v, want it to mention 'already exists'", err)
	}
	mustRunCLI(t, ctx, "template", name+"-template", "-s", org+"/"+srcName, "-F")
	assertTemplate(t, ctx, client, org, name+"-template")

	// 3. assign (individual) — create the student repo, verify the push grant,
	// then prove idempotency (existing repo skipped).
	rosterInd := filepath.Join(dir, "roster-individual.csv")
	writeRoster(t, rosterInd, student1)
	mustRunCLI(t, ctx, "assign", "-r", rosterInd, "-p", "-f", "issue", name)
	repo := name + "-" + student1
	assertRepoExists(t, ctx, client, org, repo)
	studentIsCollaborator := assertPushGranted(t, ctx, client, org, repo, student1)
	out = mustRunCLI(t, ctx, "assign", "-r", rosterInd, "-p", "-f", "issue", name)
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("re-running assign should skip the existing repo (want '1 skipped'), got:\n%s", out)
	}

	// 3b. feedback — post a graded feedback file to the student's feedback issue,
	// verify the comment landed, then prove a re-run is idempotent (no duplicate).
	fbDir := filepath.Join(dir, "feedback")
	if err := os.Mkdir(fbDir, 0o755); err != nil {
		t.Fatalf("creating feedback dir: %v", err)
	}
	fbBody := "Great work on " + name + " — see inline notes."
	if err := os.WriteFile(filepath.Join(fbDir, student1+".md"), []byte(fbBody), 0o600); err != nil {
		t.Fatalf("writing feedback file: %v", err)
	}
	mustRunCLI(t, ctx, "feedback", name, "-d", fbDir, "-r", rosterInd)
	if n := feedbackCommentCount(t, ctx, client, org, repo, fbBody); n != 1 {
		t.Errorf("feedback issue on %s should have exactly one comment with the body, got %d", repo, n)
	}
	out = mustRunCLI(t, ctx, "feedback", name, "-d", fbDir, "-r", rosterInd)
	if !strings.Contains(out, "0 posted, 1 up-to-date") {
		t.Errorf("re-running feedback should be a no-op (want '0 posted, 1 up-to-date'), got:\n%s", out)
	}
	if n := feedbackCommentCount(t, ctx, client, org, repo, fbBody); n != 1 {
		t.Errorf("re-running feedback must not duplicate the comment, got %d", n)
	}

	// 4 & 5. freeze + undo. The write->read downgrade is only observable when the
	// student is a real direct collaborator (an accepted org member) who does not
	// also hold standing admin: an org owner keeps push on every repo regardless
	// of the collaborator grant, so freeze downgrades the grant but the effective
	// permission never drops. In the unobservable cases, run the commands to prove
	// they don't error and skip the downgrade assertions.
	switch {
	case studentIsCollaborator && !isEffectiveAdmin(t, ctx, client, org, repo, student1):
		mustRunCLI(t, ctx, "freeze", name)
		assertPermission(t, ctx, client, org, repo, student1, false /*push*/, true /*pull*/)
		mustRunCLI(t, ctx, "freeze", "-u", name)
		assertPushGranted(t, ctx, client, org, repo, student1)
		out = mustRunCLI(t, ctx, "freeze", "-u", name)
		if !strings.Contains(out, "0 collaborator grant(s)") {
			t.Errorf("a second --undo should change nothing, got:\n%s", out)
		}
	case studentIsCollaborator:
		t.Logf("student %q has admin on %s (likely an org owner) — freeze cannot downgrade "+
			"an owner's inherited push, so the effective push->pull and pull->push reads are "+
			"unobservable and are skipped. Use a non-owner member account to exercise them. "+
			"The grant operations and undo idempotency are still checked.", student1, repo)
		mustRunCLI(t, ctx, "freeze", name)
		mustRunCLI(t, ctx, "freeze", "-u", name)
		out = mustRunCLI(t, ctx, "freeze", "-u", name)
		if !strings.Contains(out, "0 collaborator grant(s)") {
			t.Errorf("a second --undo should change nothing, got:\n%s", out)
		}
	default:
		t.Logf("student %q is not a direct collaborator on %s — likely a pending invite; "+
			"make the account a member of %s to exercise the freeze downgrade. "+
			"Running freeze/undo without downgrade assertions.", student1, repo, org)
		mustRunCLI(t, ctx, "freeze", name)
		mustRunCLI(t, ctx, "freeze", "-u", name)
	}

	// 6. group flow — exercises the teams resolution and multi-member grants. The
	// source is already a template repository by now, so no --mark-source is needed.
	mustRunCLI(t, ctx, "template", grp+"-template", "-s", org+"/"+srcName)
	assertTemplate(t, ctx, client, org, grp+"-template")
	rosterGrp := filepath.Join(dir, "roster-group.csv")
	teamsPath := filepath.Join(dir, "teams.yml")
	members := []string{student1}
	if student2 != "" {
		members = append(members, student2)
	}
	writeRoster(t, rosterGrp, members...)
	writeTeams(t, teamsPath, "alpha", members)
	mustRunCLI(t, ctx, "assign", "-r", rosterGrp, "-T", teamsPath, "-p", grp)
	grpRepo := grp + "-alpha"
	assertRepoExists(t, ctx, client, org, grpRepo)
	assertPushGranted(t, ctx, client, org, grpRepo, student1)
	// The group's whole point is multi-member grants: verify the second member too,
	// or say plainly that the single-member run leaves that path uncovered.
	if student2 != "" {
		assertPushGranted(t, ctx, client, org, grpRepo, student2)
	} else {
		t.Logf("GH_CLS_STUDENT2 is unset — group %s has a single member, so the "+
			"multi-member grant path is not exercised; set GH_CLS_STUDENT2 to cover it.", grpRepo)
	}
}

// runCLI drives the root command in-process with the given args, capturing its
// combined output. Each call builds a fresh root so flag state never leaks.
func runCLI(ctx context.Context, args ...string) (string, error) {
	root := cmd.NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return buf.String(), err
}

// mustRunCLI runs a command that is expected to succeed, failing the test (with
// the captured output) otherwise.
func mustRunCLI(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := runCLI(ctx, args...)
	if err != nil {
		t.Fatalf("`gh cls %s` failed: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// seedSource creates an initialized (clone-able) source repo in the org via the
// raw client, since CreateOrgRepo deliberately creates empty repos.
func seedSource(t *testing.T, rc *api.RESTClient, org, name string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"name":        name,
		"private":     true,
		"auto_init":   true,
		"description": "gh-cls live test source template",
	})
	if err != nil {
		t.Fatalf("encoding source repo request: %v", err)
	}
	var repo struct {
		Name string `json:"name"`
	}
	if err := rc.Post(fmt.Sprintf("orgs/%s/repos", org), bytes.NewReader(payload), &repo); err != nil {
		t.Fatalf("seeding source repo %s/%s: %v", org, name, err)
	}
}

// writeConfig writes the course config the run needs: the org plus one
// individual and one group assignment, each pointing at the seeded source.
func writeConfig(t *testing.T, path, org, indName, grpName string) {
	t.Helper()
	// Each assignment names the template repo assign clones: the <name>-template
	// repo built by the template command (a bare name, resolved to this org).
	content := fmt.Sprintf(`org: %[1]s
staff_team: staff
assignments:
  %[2]s:
    type: individual
    template: %[2]s-template
    feedback: issue
  %[3]s:
    type: group
    template: %[3]s-template
`, org, indName, grpName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config %s: %v", path, err)
	}
}

// writeRoster writes a roster CSV mapping each login to itself (identifier ==
// username), one row per login.
func writeRoster(t *testing.T, path string, logins ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("identifier,username\n")
	for _, l := range logins {
		fmt.Fprintf(&b, "%s,%s\n", l, l)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing roster %s: %v", path, err)
	}
}

// writeTeams writes a teams YAML with a single team and its member identifiers.
func writeTeams(t *testing.T, path, team string, members []string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", team)
	for _, m := range members {
		fmt.Fprintf(&b, "  - %s\n", m)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing teams file %s: %v", path, err)
	}
}

func assertOrgHardened(t *testing.T, ctx context.Context, client gh.Client, org string) {
	t.Helper()
	s, err := client.GetOrg(ctx, org)
	if err != nil {
		t.Fatalf("reading org settings: %v", err)
	}
	if s.DefaultRepositoryPermission != "none" {
		t.Errorf("base repository permission = %q, want %q", s.DefaultRepositoryPermission, "none")
	}
	assertToggledOff(t, "member repository creation", s.MembersCanCreateRepositories)
	assertToggledOff(t, "member Pages creation", s.MembersCanCreatePages)
	ap, err := client.GetActionsPermissions(ctx, org)
	if err != nil {
		t.Fatalf("reading Actions policy: %v", err)
	}
	if ap.EnabledRepositories != "none" {
		t.Errorf("Actions enabled_repositories = %q, want %q", ap.EnabledRepositories, "none")
	}
	if _, ok, err := client.GetTeam(ctx, org, "staff"); err != nil {
		t.Fatalf("reading staff team: %v", err)
	} else if !ok {
		t.Error("staff team should exist after setup")
	}
}

// assertToggledOff checks an org setting setup is meant to disable. The toggle is
// a *bool because some plan tiers omit the field entirely; a nil value means the
// org doesn't expose it, which is reported (not silently passed) so a tier that
// can't be hardened on this point is visible rather than mistaken for hardened.
func assertToggledOff(t *testing.T, label string, v *bool) {
	t.Helper()
	if v == nil {
		t.Logf("%s toggle is absent from this org's settings (some plan tiers omit it) — "+
			"skipping its check.", label)
		return
	}
	if *v {
		t.Errorf("%s should be disabled after setup, got enabled", label)
	}
}

func assertTemplate(t *testing.T, ctx context.Context, client gh.Client, org, derived string) {
	t.Helper()
	r := assertRepoExists(t, ctx, client, org, derived)
	if !r.IsTemplate {
		t.Errorf("%s/%s should be marked a template repository", org, derived)
	}
	if !r.Private {
		t.Errorf("%s/%s should be private", org, derived)
	}
	branches, err := client.ListBranchesWithCommitCount(ctx, org, derived)
	if err != nil {
		t.Fatalf("listing branches of %s: %v", derived, err)
	}
	if len(branches) == 0 {
		t.Fatalf("%s/%s has no branches", org, derived)
	}
	for _, b := range branches {
		if b.Commits != 1 {
			t.Errorf("branch %s of %s has %d commits, want 1 (not squashed)", b.Name, derived, b.Commits)
		}
	}
}

// feedbackCommentCount returns how many comments on the repo's feedback issue
// contain body. It resolves the issue by its title, the same way the feedback
// command does.
func feedbackCommentCount(t *testing.T, ctx context.Context, client gh.Client, org, repo, body string) int {
	t.Helper()
	number, found, err := client.FindIssueByTitle(ctx, org, repo, "Feedback")
	if err != nil {
		t.Fatalf("finding feedback issue on %s: %v", repo, err)
	}
	if !found {
		t.Fatalf("feedback issue should exist on %s/%s", org, repo)
	}
	comments, err := client.ListIssueComments(ctx, org, repo, number)
	if err != nil {
		t.Fatalf("listing comments on %s feedback issue: %v", repo, err)
	}
	n := 0
	for _, c := range comments {
		if strings.Contains(c.Body, body) {
			n++
		}
	}
	return n
}

func assertRepoExists(t *testing.T, ctx context.Context, client gh.Client, org, name string) *gh.Repo {
	t.Helper()
	r, ok, err := client.GetRepo(ctx, org, name)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", org, name, err)
	}
	if !ok {
		t.Fatalf("repository %s/%s should exist", org, name)
	}
	return r
}

// assertPushGranted checks the login is a direct collaborator with push and
// reports whether it was found at all (false means a likely pending invite).
func assertPushGranted(t *testing.T, ctx context.Context, client gh.Client, org, repo, login string) bool {
	t.Helper()
	c, ok := directCollaborator(t, ctx, client, org, repo, login)
	if !ok {
		t.Logf("%s is not a direct collaborator on %s/%s (likely a pending invite) — "+
			"skipping the push-grant check; have the account accept the org invite to verify it.",
			login, org, repo)
		return false
	}
	if !c.Permissions.Push {
		t.Errorf("%s should have push on %s/%s", login, org, repo)
	}
	return true
}

// isEffectiveAdmin reports whether login has standing admin on the repo. An org
// owner is reported as admin on every repo regardless of the direct collaborator
// grant, so freeze (which only downgrades the grant) cannot strip their push and
// the downgrade is unobservable. assign grants push, never admin, so an admin bit
// on a freshly-assigned student means inherited ownership/role, not the grant.
func isEffectiveAdmin(t *testing.T, ctx context.Context, client gh.Client, org, repo, login string) bool {
	t.Helper()
	c, ok := directCollaborator(t, ctx, client, org, repo, login)
	return ok && c.Permissions.Admin
}

// assertPermission requires the login to be a direct collaborator with the
// expected push/pull bits.
func assertPermission(t *testing.T, ctx context.Context, client gh.Client, org, repo, login string, wantPush, wantPull bool) {
	t.Helper()
	c, ok := directCollaborator(t, ctx, client, org, repo, login)
	if !ok {
		t.Fatalf("%s should be a direct collaborator on %s/%s", login, org, repo)
	}
	if c.Permissions.Push != wantPush {
		t.Errorf("%s push on %s = %t, want %t", login, repo, c.Permissions.Push, wantPush)
	}
	if c.Permissions.Pull != wantPull {
		t.Errorf("%s pull on %s = %t, want %t", login, repo, c.Permissions.Pull, wantPull)
	}
}

func directCollaborator(t *testing.T, ctx context.Context, client gh.Client, org, repo, login string) (*gh.Collaborator, bool) {
	t.Helper()
	cs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		t.Fatalf("listing collaborators of %s/%s: %v", org, repo, err)
	}
	for i := range cs {
		if strings.EqualFold(cs[i].Login, login) {
			return &cs[i], true
		}
	}
	return nil, false
}
