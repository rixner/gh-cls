package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/internal/ghtest"
)

// fakeStaffState configures a ghtest.Fake for staff tests and captures what
// it observed.
type fakeStaffState struct {
	role       string
	teamExists bool
	members    []string // current team members
	addState   string   // state AddTeamMembership returns ("active" if empty)

	added   []string
	removed []string
}

func (s *fakeStaffState) fake() *ghtest.Fake {
	fk := &ghtest.Fake{}
	fk.OrgRoleFunc = func(context.Context, string) (string, error) { return s.role, nil }
	fk.GetTeamFunc = func(context.Context, string, string) (*gh.Team, bool, error) {
		if !s.teamExists {
			return nil, false, nil
		}
		return &gh.Team{ID: 1}, true, nil
	}
	fk.ListTeamMembersFunc = func(context.Context, string, string) ([]string, error) {
		return s.members, nil
	}
	fk.AddTeamMembershipFunc = func(_ context.Context, _, _, user string) (string, error) {
		s.added = append(s.added, user)
		if s.addState != "" {
			return s.addState, nil
		}
		return "active", nil
	}
	fk.RemoveTeamMembershipFunc = func(_ context.Context, _, _, user string) error {
		s.removed = append(s.removed, user)
		return nil
	}
	return fk
}

const tasCSV = `identifier,username
ta-1,ada
ta-2,newta
`

func newStaffOpts(t *testing.T, state *fakeStaffState, tasContent string, dryRun bool) *staffOpts {
	t.Helper()
	tasPath := filepath.Join(t.TempDir(), "tas.csv")
	if err := os.WriteFile(tasPath, []byte(tasContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fk := state.fake()
	return &staffOpts{
		g:         &globalOpts{org: "cs101-spring26", staffTeam: "staff"},
		tas:       tasPath,
		dryRun:    dryRun,
		newClient: func(context.Context) (staffClient, error) { return fk, nil },
	}
}

func TestStaffAddsAndWarnsWithoutPrune(t *testing.T) {
	// Default (no --prune): add the listed TA who is absent, leave the unlisted
	// member in place, and warn about it pointing at --prune.
	state := &fakeStaffState{role: "admin", teamExists: true, members: []string{"ada", "oldta"}}
	o := newStaffOpts(t, state, tasCSV, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(state.added) != 1 || state.added[0] != "newta" {
		t.Errorf("added = %v, want [newta]", state.added)
	}
	if len(state.removed) != 0 {
		t.Errorf("without --prune nothing should be removed, got %v", state.removed)
	}
	out := buf.String()
	for _, want := range []string{"+ newta", "warning:", "oldta", "--prune"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStaffPruneRemovesAndNamesThem(t *testing.T) {
	state := &fakeStaffState{role: "admin", teamExists: true, members: []string{"ada", "oldta"}}
	o := newStaffOpts(t, state, tasCSV, false)
	o.prune = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(state.added) != 1 || state.added[0] != "newta" {
		t.Errorf("added = %v, want [newta]", state.added)
	}
	if len(state.removed) != 1 || state.removed[0] != "oldta" {
		t.Errorf("removed = %v, want [oldta]", state.removed)
	}
	out := buf.String()
	// The removed member is named (so a mistake is easy to undo) and counted.
	for _, want := range []string{"+ newta", "- oldta", "removed", "1 added, 1 removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning:") {
		t.Errorf("--prune should remove, not warn:\n%s", out)
	}
}

func TestStaffCaseInsensitiveNoChange(t *testing.T) {
	// "Ada" in the file and "ada" on the team are the same login (GitHub logins
	// are case-insensitive), so nothing should change.
	state := &fakeStaffState{role: "admin", teamExists: true, members: []string{"ada"}}
	o := newStaffOpts(t, state, "identifier,username\nta-1,Ada\n", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(state.added) != 0 || len(state.removed) != 0 {
		t.Errorf("a case-only difference must be no change; added=%v removed=%v", state.added, state.removed)
	}
	if !strings.Contains(buf.String(), "already in sync") {
		t.Errorf("want already-in-sync:\n%s", buf.String())
	}
}

func TestStaffDryRunMakesNoChanges(t *testing.T) {
	state := &fakeStaffState{role: "admin", teamExists: true, members: []string{"oldta"}}
	o := newStaffOpts(t, state, tasCSV, true)
	o.prune = true // exercise both add and remove previews

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(state.added) != 0 || len(state.removed) != 0 {
		t.Error("dry-run must not modify membership")
	}
	out := buf.String()
	for _, want := range []string{"DRY RUN", "would add", "would remove"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStaffPendingInviteReported(t *testing.T) {
	state := &fakeStaffState{role: "admin", teamExists: true, addState: "pending"}
	o := newStaffOpts(t, state, "identifier,username\nta-1,newta\n", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "invited") {
		t.Errorf("a pending add should be reported as invited:\n%s", buf.String())
	}
}

func TestStaffOwnerGuard(t *testing.T) {
	state := &fakeStaffState{role: "member", teamExists: true}
	o := newStaffOpts(t, state, tasCSV, false)

	if err := o.run(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if len(state.added) != 0 || len(state.removed) != 0 {
		t.Error("no membership changes should occur when the owner guard fails")
	}
}

func TestStaffTeamNotFound(t *testing.T) {
	state := &fakeStaffState{role: "admin", teamExists: false}
	o := newStaffOpts(t, state, tasCSV, false)

	if err := o.run(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing staff team should error, got %v", err)
	}
}
