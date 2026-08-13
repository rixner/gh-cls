package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeSetupState configures a ghtest.Fake for setup tests and captures what
// it observed. Zero values yield an already-hardened org with no Copilot and
// no team.
type fakeSetupState struct {
	role           string
	settings       gh.OrgSettings
	actions        string
	copilotSeats   int
	copilotPresent bool
	teamExists     bool
	ignorePatches  bool // accept PATCH/PUT calls but leave the org state unchanged

	// property is the existing freeze-state property declaration, nil when the org
	// has none yet.
	property *gh.PropertyDefinition

	patched     map[string]any
	actionsSet  string
	createdTeam string
	setProperty *gh.PropertyDefinition
}

func (s *fakeSetupState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.GetOrgFunc = func(context.Context, string) (*gh.OrgSettings, error) {
		settings := s.settings
		return &settings, nil
	}
	fk.PatchOrgFunc = func(_ context.Context, _ string, fields map[string]any) error {
		if s.patched == nil {
			s.patched = map[string]any{}
		}
		for k, v := range fields {
			s.patched[k] = v
			if s.ignorePatches {
				continue
			}
			// Apply to the org state so a later GetOrg (the post-condition check) sees
			// the change, mirroring a tier that honors the setting.
			switch k {
			case "default_repository_permission":
				s.settings.DefaultRepositoryPermission = v.(string)
			case "members_can_create_repositories":
				b := v.(bool)
				s.settings.MembersCanCreateRepositories = &b
			case "members_can_create_pages":
				b := v.(bool)
				s.settings.MembersCanCreatePages = &b
			case "members_can_fork_private_repositories":
				b := v.(bool)
				s.settings.MembersCanForkPrivateRepos = &b
			}
		}
		return nil
	}
	fk.GetActionsPermissionsFunc = func(context.Context, string) (*gh.ActionsPermissions, error) {
		return &gh.ActionsPermissions{EnabledRepositories: s.actions}, nil
	}
	fk.SetActionsEnabledRepositoriesFunc = func(_ context.Context, _, v string) error {
		s.actionsSet = v
		if !s.ignorePatches {
			s.actions = v
		}
		return nil
	}
	fk.CopilotSeatCountFunc = func(context.Context, string) (int, bool, error) {
		return s.copilotSeats, s.copilotPresent, nil
	}
	fk.GetTeamFunc = func(context.Context, string, string) (*gh.Team, bool, error) {
		if !s.teamExists {
			return nil, false, nil
		}
		return &gh.Team{ID: 1}, true, nil
	}
	fk.CreateTeamFunc = func(_ context.Context, _, name string) (*gh.Team, error) {
		s.createdTeam = name
		return &gh.Team{ID: 2}, nil
	}
	fk.GetPropertyDefinitionFunc = func(context.Context, string, string) (*gh.PropertyDefinition, bool, error) {
		if s.property == nil {
			return nil, false, nil
		}
		def := *s.property
		return &def, true, nil
	}
	fk.SetPropertyDefinitionFunc = func(_ context.Context, _ string, def gh.PropertyDefinition) error {
		s.setProperty = &def
		if s.ignorePatches {
			return nil // accepted but not applied, as an unsupported tier would
		}
		s.property = &def
		return nil
	}
	return fk
}

// newSetupOpts builds setupOpts wired to a state. The org and staff team stand in
// for what the root loads from config before setup runs.
func newSetupOpts(t *testing.T, state *fakeSetupState, org, staffTeam string, dryRun bool) *setupOpts {
	t.Helper()
	fk := state.fake()
	return &setupOpts{
		g:         &globalOpts{org: org, staffTeam: staffTeam},
		dryRun:    dryRun,
		newClient: func(context.Context) (setupClient, error) { return fk, nil },
	}
}

func TestSetupOwnerGuard(t *testing.T) {
	state := &fakeSetupState{role: "member"}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	err := o.run(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if state.patched != nil || state.actionsSet != "" {
		t.Error("no org mutations should occur when the owner guard fails")
	}
}

func TestSetupDeclaresTheFreezeProperty(t *testing.T) {
	state := &fakeSetupState{role: "admin", teamExists: true}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if state.setProperty == nil {
		t.Fatal("setup should declare the freeze-state property")
	}
	if state.setProperty.PropertyName != frozenProperty {
		t.Errorf("property name = %q, want %q", state.setProperty.PropertyName, frozenProperty)
	}
	if state.setProperty.ValueType != gh.PropertyTypeTrueFalse {
		t.Errorf("value type = %q, want %q", state.setProperty.ValueType, gh.PropertyTypeTrueFalse)
	}
	// The edit scope is the part that matters: it keeps a repository-level role
	// from rewriting a deadline record.
	if state.setProperty.ValuesEditableBy != gh.PropertyEditableByOrg {
		t.Errorf("values editable by = %q, want %q", state.setProperty.ValuesEditableBy, gh.PropertyEditableByOrg)
	}
	if !strings.Contains(buf.String(), frozenProperty) {
		t.Errorf("the property should be reported:\n%s", buf.String())
	}
}

func TestSetupCorrectsAWidenedFreezeProperty(t *testing.T) {
	// A property someone widened so repository actors can edit values would let a
	// repo admin rewrite a deadline record. setup re-asserts the restricted scope.
	state := &fakeSetupState{
		role:       "admin",
		teamExists: true,
		property: &gh.PropertyDefinition{
			PropertyName:     frozenProperty,
			ValueType:        gh.PropertyTypeTrueFalse,
			ValuesEditableBy: "org_and_repo_actors",
		},
	}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if state.setProperty == nil || state.setProperty.ValuesEditableBy != gh.PropertyEditableByOrg {
		t.Fatalf("setup should re-assert the restricted edit scope, got %+v", state.setProperty)
	}
	if !strings.Contains(buf.String(), "corrected") {
		t.Errorf("the correction should be reported:\n%s", buf.String())
	}
}

func TestSetupLeavesACorrectFreezePropertyAlone(t *testing.T) {
	state := &fakeSetupState{
		role:       "admin",
		teamExists: true,
		property: &gh.PropertyDefinition{
			PropertyName:     frozenProperty,
			ValueType:        gh.PropertyTypeTrueFalse,
			ValuesEditableBy: gh.PropertyEditableByOrg,
		},
	}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	if err := o.run(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if state.setProperty != nil {
		t.Errorf("an already-correct property should not be rewritten, got %+v", state.setProperty)
	}
}

func TestSetupChangesAndReports(t *testing.T) {
	yes := true
	state := &fakeSetupState{
		role:           "admin",
		settings:       gh.OrgSettings{DefaultRepositoryPermission: "write", MembersCanCreateRepositories: &yes, MembersCanCreatePages: &yes, MembersCanForkPrivateRepos: &yes},
		actions:        "all",
		copilotPresent: false,
		teamExists:     false,
	}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if state.patched["default_repository_permission"] != "none" {
		t.Error("base permission should be set to none")
	}
	if state.patched["members_can_create_repositories"] != false {
		t.Error("member repo creation should be disabled")
	}
	if state.patched["members_can_fork_private_repositories"] != false {
		t.Error("private repository forking should be disabled")
	}
	if state.actionsSet != "none" {
		t.Error("Actions should be disabled org-wide")
	}
	if state.createdTeam != "staff" {
		t.Error("staff team should be created when absent")
	}
	for _, want := range []string{"Hardening cs101-spring26", "changed", "none present", "created staff",
		"Optional hardening", "creating teams", "deleting or transferring"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSetupAlreadyHardened(t *testing.T) {
	no := false
	state := &fakeSetupState{
		role:       "admin",
		settings:   gh.OrgSettings{DefaultRepositoryPermission: "none", MembersCanCreateRepositories: &no, MembersCanCreatePages: &no, MembersCanForkPrivateRepos: &no},
		actions:    "none",
		teamExists: true,
	}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if state.patched != nil {
		t.Errorf("nothing should be patched when already hardened, got %v", state.patched)
	}
	if state.actionsSet != "" || state.createdTeam != "" {
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
	state := &fakeSetupState{
		role:          "admin",
		settings:      gh.OrgSettings{DefaultRepositoryPermission: "write", MembersCanCreateRepositories: &yes, MembersCanForkPrivateRepos: &yes},
		actions:       "all",
		ignorePatches: true,
	}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`still "write" after the change`,
		"member repository creation",
		"private repository forking",
		`still "all" after the change`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected a post-condition warning containing %q:\n%s", want, out)
		}
	}
}

func TestSetupDryRunMakesNoChanges(t *testing.T) {
	state := &fakeSetupState{role: "admin"}
	o := newSetupOpts(t, state, "cs101-spring26", "staff", true)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if state.patched != nil || state.actionsSet != "" || state.createdTeam != "" {
		t.Error("dry-run must not mutate the org")
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should be labeled:\n%s", buf.String())
	}
}
