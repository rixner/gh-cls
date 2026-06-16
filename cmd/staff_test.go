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

// fakeStaffClient stands in for the GitHub operations staff uses.
type fakeStaffClient struct {
	role       string
	teamExists bool
	members    []string // current team members
	addState   string   // state AddTeamMembership returns ("active" if empty)
	added      []string
	removed    []string
}

func (f *fakeStaffClient) OrgRole(context.Context, string) (string, error) { return f.role, nil }

func (f *fakeStaffClient) GetTeam(context.Context, string, string) (*gh.Team, bool, error) {
	if !f.teamExists {
		return nil, false, nil
	}
	return &gh.Team{ID: 1}, true, nil
}

func (f *fakeStaffClient) ListTeamMembers(context.Context, string, string) ([]string, error) {
	return f.members, nil
}

func (f *fakeStaffClient) AddTeamMembership(_ context.Context, _, _, user string) (string, error) {
	f.added = append(f.added, user)
	if f.addState != "" {
		return f.addState, nil
	}
	return "active", nil
}

func (f *fakeStaffClient) RemoveTeamMembership(_ context.Context, _, _, user string) error {
	f.removed = append(f.removed, user)
	return nil
}

const tasCSV = `identifier,username
ta-1,ada
ta-2,newta
`

func newStaffOpts(t *testing.T, fake *fakeStaffClient, tasContent string, dryRun bool) *staffOpts {
	t.Helper()
	tasPath := filepath.Join(t.TempDir(), "tas.csv")
	if err := os.WriteFile(tasPath, []byte(tasContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return &staffOpts{
		g:         &globalOpts{org: "cs101-spring26", staffTeam: "staff"},
		tas:       tasPath,
		dryRun:    dryRun,
		newClient: func(context.Context) (staffClient, error) { return fake, nil },
	}
}

func TestStaffAddsAndWarnsWithoutPrune(t *testing.T) {
	// Default (no --prune): add the listed TA who is absent, leave the unlisted
	// member in place, and warn about it pointing at --prune.
	fake := &fakeStaffClient{role: "admin", teamExists: true, members: []string{"ada", "oldta"}}
	o := newStaffOpts(t, fake, tasCSV, false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 1 || fake.added[0] != "newta" {
		t.Errorf("added = %v, want [newta]", fake.added)
	}
	if len(fake.removed) != 0 {
		t.Errorf("without --prune nothing should be removed, got %v", fake.removed)
	}
	out := buf.String()
	for _, want := range []string{"+ newta", "warning:", "oldta", "--prune"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStaffPruneRemovesAndNamesThem(t *testing.T) {
	fake := &fakeStaffClient{role: "admin", teamExists: true, members: []string{"ada", "oldta"}}
	o := newStaffOpts(t, fake, tasCSV, false)
	o.prune = true

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 1 || fake.added[0] != "newta" {
		t.Errorf("added = %v, want [newta]", fake.added)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "oldta" {
		t.Errorf("removed = %v, want [oldta]", fake.removed)
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
	fake := &fakeStaffClient{role: "admin", teamExists: true, members: []string{"ada"}}
	o := newStaffOpts(t, fake, "identifier,username\nta-1,Ada\n", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 0 || len(fake.removed) != 0 {
		t.Errorf("a case-only difference must be no change; added=%v removed=%v", fake.added, fake.removed)
	}
	if !strings.Contains(buf.String(), "already in sync") {
		t.Errorf("want already-in-sync:\n%s", buf.String())
	}
}

func TestStaffDryRunMakesNoChanges(t *testing.T) {
	fake := &fakeStaffClient{role: "admin", teamExists: true, members: []string{"oldta"}}
	o := newStaffOpts(t, fake, tasCSV, true)
	o.prune = true // exercise both add and remove previews

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 0 || len(fake.removed) != 0 {
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
	fake := &fakeStaffClient{role: "admin", teamExists: true, addState: "pending"}
	o := newStaffOpts(t, fake, "identifier,username\nta-1,newta\n", false)

	var buf bytes.Buffer
	if err := o.run(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "invited") {
		t.Errorf("a pending add should be reported as invited:\n%s", buf.String())
	}
}

func TestStaffOwnerGuard(t *testing.T) {
	fake := &fakeStaffClient{role: "member", teamExists: true}
	o := newStaffOpts(t, fake, tasCSV, false)

	if err := o.run(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("non-owner should be rejected, got %v", err)
	}
	if len(fake.added) != 0 || len(fake.removed) != 0 {
		t.Error("no membership changes should occur when the owner guard fails")
	}
}

func TestStaffTeamNotFound(t *testing.T) {
	fake := &fakeStaffClient{role: "admin", teamExists: false}
	o := newStaffOpts(t, fake, tasCSV, false)

	if err := o.run(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing staff team should error, got %v", err)
	}
}
