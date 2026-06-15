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

// fakeSetupClient is a configurable stand-in for the GitHub operations setup
// uses. Zero values yield an already-hardened org with no Copilot and no team.
type fakeSetupClient struct {
	role           string
	settings       gh.OrgSettings
	actions        string
	copilotSeats   int
	copilotPresent bool
	teamExists     bool
	patched        map[string]any
	actionsSet     string
	createdTeam    string
	ignorePatches  bool // accept PATCH/PUT calls but leave the org state unchanged
}

func (f *fakeSetupClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }
func (f *fakeSetupClient) GetOrg(context.Context, string) (*gh.OrgSettings, error) {
	s := f.settings
	return &s, nil
}
func (f *fakeSetupClient) PatchOrg(_ context.Context, _ string, fields map[string]any) error {
	if f.patched == nil {
		f.patched = map[string]any{}
	}
	for k, v := range fields {
		f.patched[k] = v
		if f.ignorePatches {
			continue
		}
		// Apply to the org state so a later GetOrg (the post-condition check) sees
		// the change, mirroring a tier that honors the setting.
		switch k {
		case "default_repository_permission":
			f.settings.DefaultRepositoryPermission = v.(string)
		case "members_can_create_repositories":
			b := v.(bool)
			f.settings.MembersCanCreateRepositories = &b
		case "members_can_create_pages":
			b := v.(bool)
			f.settings.MembersCanCreatePages = &b
		}
	}
	return nil
}
func (f *fakeSetupClient) GetActionsPermissions(context.Context, string) (*gh.ActionsPermissions, error) {
	return &gh.ActionsPermissions{EnabledRepositories: f.actions}, nil
}
func (f *fakeSetupClient) SetActionsEnabledRepositories(_ context.Context, _, v string) error {
	f.actionsSet = v
	if !f.ignorePatches {
		f.actions = v
	}
	return nil
}
func (f *fakeSetupClient) CopilotSeatCount(context.Context, string) (int, bool, error) {
	return f.copilotSeats, f.copilotPresent, nil
}
func (f *fakeSetupClient) GetTeam(context.Context, string, string) (*gh.Team, bool, error) {
	if !f.teamExists {
		return nil, false, nil
	}
	return &gh.Team{ID: 1}, true, nil
}
func (f *fakeSetupClient) CreateTeam(_ context.Context, _, name string) (*gh.Team, error) {
	f.createdTeam = name
	return &gh.Team{ID: 2}, nil
}

// newSetupOpts builds setupOpts wired to a fake, writing config into a temp dir.
func newSetupOpts(t *testing.T, fake *fakeSetupClient, org, staffTeam string, dryRun bool) (*setupOpts, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	t.Setenv("GH_CLS_CONFIG", path)

	o := &setupOpts{
		g:         &globalOpts{org: org, staffTeam: staffTeam},
		dryRun:    dryRun,
		newClient: func(context.Context) (setupClient, error) { return fake, nil },
	}
	return o, path
}

func TestSetupOwnerGuard(t *testing.T) {
	fake := &fakeSetupClient{role: "member"}
	o, path := newSetupOpts(t, fake, "cs101-spring26", "", false)

	err := o.run(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if fake.patched != nil || fake.actionsSet != "" {
		t.Error("no org mutations should occur when the owner guard fails")
	}
	// The org is persisted only after the owner check passes, so a rejected
	// non-owner run must not record it.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("org must not be written to config when the owner guard fails")
	}
}

func TestSetupChangesAndReports(t *testing.T) {
	yes := true
	fake := &fakeSetupClient{
		role:           "admin",
		settings:       gh.OrgSettings{DefaultRepositoryPermission: "write", MembersCanCreateRepositories: &yes, MembersCanCreatePages: &yes},
		actions:        "all",
		copilotPresent: false,
		teamExists:     false,
	}
	o, _ := newSetupOpts(t, fake, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if fake.patched["default_repository_permission"] != "none" {
		t.Error("base permission should be set to none")
	}
	if fake.patched["members_can_create_repositories"] != false {
		t.Error("member repo creation should be disabled")
	}
	if fake.actionsSet != "none" {
		t.Error("Actions should be disabled org-wide")
	}
	if fake.createdTeam != "staff" {
		t.Error("staff team should be created when absent")
	}
	for _, want := range []string{"CONFIG ORG SET → cs101-spring26", "changed", "none present", "created staff",
		"Optional hardening", "creating teams", "deleting or transferring"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSetupAlreadyHardened(t *testing.T) {
	no := false
	fake := &fakeSetupClient{
		role:       "admin",
		settings:   gh.OrgSettings{DefaultRepositoryPermission: "none", MembersCanCreateRepositories: &no, MembersCanCreatePages: &no},
		actions:    "none",
		teamExists: true,
	}
	o, _ := newSetupOpts(t, fake, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if fake.patched != nil {
		t.Errorf("nothing should be patched when already hardened, got %v", fake.patched)
	}
	if fake.actionsSet != "" || fake.createdTeam != "" {
		t.Error("no changes expected when already in the desired state")
	}
	if !strings.Contains(buf.String(), "already") {
		t.Errorf("expected 'already' statuses:\n%s", buf.String())
	}
}

func TestSetupWarnsWhenSettingDoesNotStick(t *testing.T) {
	// The API accepts every change but the org silently ignores them (as some plan
	// tiers do). setup must re-read, notice the org is not actually hardened, and
	// warn loudly rather than report success.
	yes := true
	fake := &fakeSetupClient{
		role:          "admin",
		settings:      gh.OrgSettings{DefaultRepositoryPermission: "write", MembersCanCreateRepositories: &yes},
		actions:       "all",
		ignorePatches: true,
	}
	o, _ := newSetupOpts(t, fake, "cs101-spring26", "", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`still "write" after the change`,
		"member repository creation",
		`still "all" after the change`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected a post-condition warning containing %q:\n%s", want, out)
		}
	}
}

func TestSetupDryRunMakesNoChanges(t *testing.T) {
	fake := &fakeSetupClient{role: "admin"}
	o, path := newSetupOpts(t, fake, "cs101-spring26", "staff", true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("dry-run must not write the config file")
	}
	if fake.patched != nil || fake.actionsSet != "" || fake.createdTeam != "" {
		t.Error("dry-run must not mutate the org")
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should be labeled:\n%s", buf.String())
	}
}
