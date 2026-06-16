package unit_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/rixner/gh-cls/unit"
)

const sampleRoster = `identifier,username
student-001,ada
student-002,alan
student-003,grace
student-004,katherine
student-005,margaret
`

func mustRoster(t *testing.T) *roster.Roster {
	t.Helper()
	r, err := roster.Parse(strings.NewReader(sampleRoster))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustTeams(t *testing.T, src string) *teams.Teams {
	t.Helper()
	tm, err := teams.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestResolveIndividual(t *testing.T) {
	units, rep, err := unit.Resolve(config.TypeIndividual, mustRoster(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("individual resolve should have no warnings, got %+v", rep)
	}
	want := []unit.Unit{
		{Key: "ada", Members: []string{"ada"}},
		{Key: "alan", Members: []string{"alan"}},
		{Key: "grace", Members: []string{"grace"}},
		{Key: "katherine", Members: []string{"katherine"}},
		{Key: "margaret", Members: []string{"margaret"}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Errorf("units = %+v\nwant %+v", units, want)
	}
}

func TestResolveIndividualRejectsTeams(t *testing.T) {
	_, _, err := unit.Resolve(config.TypeIndividual, mustRoster(t), mustTeams(t, "a: [student-001]\n"))
	if err == nil {
		t.Fatal("individual assignment with a teams file should error")
	}
}

func TestResolveGroup(t *testing.T) {
	src := "team-alpha: [student-001, student-003]\nteam-beta: [student-002, student-004]\nteam-gamma: [student-005]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustTeams(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("no warnings expected, got %+v", rep)
	}
	want := []unit.Unit{
		{Key: "team-alpha", Members: []string{"ada", "grace"}},
		{Key: "team-beta", Members: []string{"alan", "katherine"}},
		{Key: "team-gamma", Members: []string{"margaret"}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Errorf("units = %+v\nwant %+v", units, want)
	}
}

func TestResolveGroupUnknownIdentifier(t *testing.T) {
	src := "team-alpha: [student-001, student-999]\n"
	units, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustTeams(t, src))
	if err == nil {
		t.Fatal("a team referencing an unknown identifier must be a hard error")
	}
	if units != nil {
		t.Error("no units should be returned on a hard error")
	}
	if !strings.Contains(err.Error(), "student-999") {
		t.Errorf("error should name the offending identifier: %v", err)
	}
}

func TestResolveGroupCaseMismatchHint(t *testing.T) {
	// The roster has "student-001"; the teams file uses "Student-001". Identifiers
	// are case-sensitive, so this is a hard error — but it should hint at the
	// near-match rather than just say the identifier is missing.
	src := "team-alpha: [Student-001]\n"
	_, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustTeams(t, src))
	if err == nil {
		t.Fatal("a case-mismatched identifier must be a hard error")
	}
	if !strings.Contains(err.Error(), "student-001") || !strings.Contains(err.Error(), "case-sensitive") {
		t.Errorf("error should hint at the case mismatch, got: %v", err)
	}
}

func TestResolveGroupUnassignedWarns(t *testing.T) {
	// student-004 and student-005 are on no team.
	src := "team-alpha: [student-001, student-003]\nteam-beta: [student-002]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustTeams(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Errorf("got %d units, want 2 (resolution proceeds despite warning)", len(units))
	}
	if !reflect.DeepEqual(rep.UnassignedIDs, []string{"student-004", "student-005"}) {
		t.Errorf("UnassignedIDs = %v, want [student-004 student-005]", rep.UnassignedIDs)
	}
}

func TestResolveGroupRequiresTeams(t *testing.T) {
	if _, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), nil); err == nil {
		t.Fatal("group assignment without a teams file should error")
	}
}

func TestResolveUnknownType(t *testing.T) {
	if _, _, err := unit.Resolve(config.AssignmentType("weekly"), mustRoster(t), nil); err == nil {
		t.Fatal("unknown assignment type should error")
	}
}
