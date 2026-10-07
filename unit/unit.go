// Package unit performs the id->username join that turns a roster and (for group
// assignments) a groups file into the list of repositories to create.
//
// This is the privacy-sensitive core: it reads the roster and groups data in
// memory and returns only GitHub usernames and group/assignment names. Nothing it
// returns is student PII beyond the GitHub handles that must already be public to
// grant repo access.
package unit

import (
	"fmt"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/groups"
	"github.com/rixner/gh-cls/roster"
)

// Unit is one repository to create: Key is the repo-name suffix (a GitHub
// username for individual assignments, a group name for group assignments) and
// Members are the GitHub usernames that get push access.
//
// Students the roster marks as dropped are not Members, so nothing that grants
// access ever reaches them. They are listed in Dropped instead, with what they
// may keep on this repository, which is what audit checks and --revoke enforces.
// An individual dropped student's unit has no Members at all.
type Unit struct {
	Key     string
	Members []string
	Dropped []DroppedMember
}

// DroppedMember is a student the roster marks as dropped, with the most access
// they may keep on one repository.
type DroppedMember struct {
	Login  string
	Retain Retain
}

// Retain is the most access a dropped student may keep on one repository.
type Retain int

const (
	// RetainRead allows read access and nothing above it.
	RetainRead Retain = iota
	// RetainNothing allows no access and no invitation.
	RetainNothing
)

// DroppedStudents lists every student the roster marks as dropped, in roster
// order, with what they may keep on an assignment of the given type. It reads no
// groups file on purpose: a dropped student is often taken out of their group,
// and audit still has to recognize them on whatever repo they hold access to.
func DroppedStudents(typ config.AssignmentType, r *roster.Roster) []DroppedMember {
	var out []DroppedMember
	for _, id := range r.IDs() {
		if a := r.Access(id); a != roster.AccessEnrolled {
			login, _ := r.Lookup(id) // present by construction of the roster
			out = append(out, DroppedMember{Login: login, Retain: retainFor(a, typ)})
		}
	}
	return out
}

// retainFor turns a roster access value into what a dropped student keeps on an
// assignment of the given type. It decides by the assignment's type, not by how
// many students a repo holds, so what "own" does is readable from the config
// alone and a later change to a groups file cannot flip it.
func retainFor(a roster.Access, typ config.AssignmentType) Retain {
	switch a {
	case roster.AccessRead:
		return RetainRead
	case roster.AccessOwn:
		if typ == config.TypeIndividual {
			return RetainRead
		}
	}
	return RetainNothing
}

// Resolve builds the unit list for an assignment.
//
// For an individual assignment the unit list is the roster (one unit per
// student, keyed by username); a groups file is rejected. For a group assignment
// the unit list comes from the groups file (one unit per group, keyed by group
// name) with members resolved through the roster; a groups file is required.
//
// It returns a Report of non-fatal findings (enrolled students in no group). A
// group that references an identifier absent from the roster is a fatal error,
// returned with no units.
func Resolve(typ config.AssignmentType, r *roster.Roster, g *groups.Groups) ([]Unit, Report, error) {
	switch typ {
	case config.TypeIndividual:
		if g != nil {
			return nil, Report{}, fmt.Errorf("individual assignment does not take a groups file")
		}
		return resolveIndividual(r), Report{}, nil
	case config.TypeGroup:
		if g == nil {
			return nil, Report{}, fmt.Errorf("group assignment requires a groups file")
		}
		return resolveGroup(r, g)
	default:
		return nil, Report{}, fmt.Errorf("unknown assignment type %q", typ)
	}
}
