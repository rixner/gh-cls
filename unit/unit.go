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
type Unit struct {
	Key     string
	Members []string
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
