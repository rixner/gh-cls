package unit_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/groups"
	"github.com/rixner/gh-cls/roster"
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

func mustGroups(t *testing.T, src string) *groups.Groups {
	t.Helper()
	g, err := groups.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return g
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

func TestResolveIndividualRejectsGroups(t *testing.T) {
	_, _, err := unit.Resolve(config.TypeIndividual, mustRoster(t), mustGroups(t, "a: [student-001]\n"))
	if err == nil {
		t.Fatal("individual assignment with a groups file should error")
	}
}

func TestResolveGroup(t *testing.T) {
	src := "group-alpha: [student-001, student-003]\ngroup-beta: [student-002, student-004]\ngroup-gamma: [student-005]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustGroups(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("no warnings expected, got %+v", rep)
	}
	want := []unit.Unit{
		{Key: "group-alpha", Members: []string{"ada", "grace"}},
		{Key: "group-beta", Members: []string{"alan", "katherine"}},
		{Key: "group-gamma", Members: []string{"margaret"}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Errorf("units = %+v\nwant %+v", units, want)
	}
}

func TestResolveGroupUnknownIdentifier(t *testing.T) {
	src := "group-alpha: [student-001, student-999]\n"
	units, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustGroups(t, src))
	if err == nil {
		t.Fatal("a group referencing an unknown identifier must be a hard error")
	}
	if units != nil {
		t.Error("no units should be returned on a hard error")
	}
	if !strings.Contains(err.Error(), "student-999") {
		t.Errorf("error should name the offending identifier: %v", err)
	}
}

func TestResolveGroupCaseMismatchHint(t *testing.T) {
	// The roster has "student-001"; the groups file uses "Student-001". Identifiers
	// are case-sensitive, so this is a hard error, but it should hint at the
	// near-match rather than just say the identifier is missing.
	src := "group-alpha: [Student-001]\n"
	_, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustGroups(t, src))
	if err == nil {
		t.Fatal("a case-mismatched identifier must be a hard error")
	}
	if !strings.Contains(err.Error(), "student-001") || !strings.Contains(err.Error(), "case-sensitive") {
		t.Errorf("error should hint at the case mismatch, got: %v", err)
	}
}

func TestResolveGroupUnassignedWarns(t *testing.T) {
	// student-004 and student-005 are in no group.
	src := "group-alpha: [student-001, student-003]\ngroup-beta: [student-002]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustGroups(t, src))
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

func TestResolveGroupMultiGroup(t *testing.T) {
	// student-001 is in both group-alpha and group-gamma. Resolution still proceeds
	// (the finding is reported, not enforced here), and records the overlap with
	// the groups in file order.
	src := "group-alpha: [student-001, student-003]\ngroup-beta: [student-002, student-004]\ngroup-gamma: [student-001, student-005]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustRoster(t), mustGroups(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 3 {
		t.Errorf("got %d units, want 3 (resolution proceeds despite the overlap)", len(units))
	}
	want := []unit.MultiGroupMembership{{ID: "student-001", Groups: []string{"group-alpha", "group-gamma"}}}
	if !reflect.DeepEqual(rep.MultiGroup, want) {
		t.Errorf("MultiGroup = %+v, want %+v", rep.MultiGroup, want)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("no unassigned expected, got %v", rep.UnassignedIDs)
	}
}

func TestResolveGroupRequiresGroups(t *testing.T) {
	if _, _, err := unit.Resolve(config.TypeGroup, mustRoster(t), nil); err == nil {
		t.Fatal("group assignment without a groups file should error")
	}
}

func TestResolveUnknownType(t *testing.T) {
	if _, _, err := unit.Resolve(config.AssignmentType("weekly"), mustRoster(t), nil); err == nil {
		t.Fatal("unknown assignment type should error")
	}
}

const droppedRoster = `identifier,username,access
student-001,ada
student-002,alan,read
student-003,grace,none
student-004,katherine,own
student-005,margaret
`

func mustDroppedRoster(t *testing.T) *roster.Roster {
	t.Helper()
	r, err := roster.Parse(strings.NewReader(droppedRoster))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestResolveIndividualDropped checks dropped students get no push grant and
// keep read under read and own, nothing under none.
func TestResolveIndividualDropped(t *testing.T) {
	units, _, err := unit.Resolve(config.TypeIndividual, mustDroppedRoster(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []unit.Unit{
		{Key: "ada", Members: []string{"ada"}},
		{Key: "alan", Dropped: []unit.DroppedMember{{Login: "alan", Retain: unit.RetainRead}}},
		{Key: "grace", Dropped: []unit.DroppedMember{{Login: "grace", Retain: unit.RetainNothing}}},
		{Key: "katherine", Dropped: []unit.DroppedMember{{Login: "katherine", Retain: unit.RetainRead}}},
		{Key: "margaret", Members: []string{"margaret"}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Errorf("units = %+v\nwant %+v", units, want)
	}
}

// TestResolveGroupDropped checks own removes a student from a group repo, read
// keeps read there, and the remaining members keep their grant.
func TestResolveGroupDropped(t *testing.T) {
	src := "group-alpha: [student-001, student-002]\ngroup-beta: [student-003, student-004, student-005]\n"
	units, rep, err := unit.Resolve(config.TypeGroup, mustDroppedRoster(t), mustGroups(t, src))
	if err != nil {
		t.Fatal(err)
	}
	want := []unit.Unit{
		{Key: "group-alpha", Members: []string{"ada"}, Dropped: []unit.DroppedMember{{Login: "alan", Retain: unit.RetainRead}}},
		{Key: "group-beta", Members: []string{"margaret"}, Dropped: []unit.DroppedMember{
			{Login: "grace", Retain: unit.RetainNothing},
			{Login: "katherine", Retain: unit.RetainNothing},
		}},
	}
	if !reflect.DeepEqual(units, want) {
		t.Errorf("units = %+v\nwant %+v", units, want)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("no unassigned expected, got %v", rep.UnassignedIDs)
	}
}

// TestResolveGroupDroppedNotUnassigned checks a dropped student taken out of the
// groups file is not reported as in no group, which would make assign abort.
func TestResolveGroupDroppedNotUnassigned(t *testing.T) {
	src := "group-alpha: [student-001, student-005]\ngroup-beta: [student-002]\n"
	_, rep, err := unit.Resolve(config.TypeGroup, mustDroppedRoster(t), mustGroups(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.UnassignedIDs) > 0 {
		t.Errorf("dropped students should not be unassigned, got %v", rep.UnassignedIDs)
	}
}

// TestDroppedStudents checks every marked student is listed from the roster
// alone, with what they keep on each assignment type.
func TestDroppedStudents(t *testing.T) {
	r := mustDroppedRoster(t)
	ind := unit.DroppedStudents(config.TypeIndividual, r)
	want := []unit.DroppedMember{
		{Login: "alan", Retain: unit.RetainRead},
		{Login: "grace", Retain: unit.RetainNothing},
		{Login: "katherine", Retain: unit.RetainRead},
	}
	if !reflect.DeepEqual(ind, want) {
		t.Errorf("individual = %+v\nwant %+v", ind, want)
	}
	grp := unit.DroppedStudents(config.TypeGroup, r)
	want[2].Retain = unit.RetainNothing // own removes them from a group repo
	if !reflect.DeepEqual(grp, want) {
		t.Errorf("group = %+v\nwant %+v", grp, want)
	}
	if got := unit.DroppedStudents(config.TypeIndividual, mustRoster(t)); len(got) != 0 {
		t.Errorf("a roster with no access column has no dropped students, got %+v", got)
	}
}
