// Package unit performs the id->username join that turns a roster and (for group
// assignments) a teams file into the list of repositories to create.
//
// This is the privacy-sensitive core: it reads the roster and teams data in
// memory and returns only GitHub usernames and team/assignment names. Nothing it
// returns is student PII beyond the GitHub handles that must already be public to
// grant repo access.
package unit

import (
	"fmt"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
)

// Unit is one repository to create: Key is the repo-name suffix (a GitHub
// username for individual assignments, a team name for group assignments) and
// Members are the GitHub usernames that get push access.
type Unit struct {
	Key     string
	Members []string
}

// Resolve builds the unit list for an assignment.
//
// For an individual assignment the unit list is the roster (one unit per
// student, keyed by username); a teams file is rejected. For a group assignment
// the unit list comes from the teams file (one unit per team, keyed by team
// name) with members resolved through the roster; a teams file is required.
//
// It returns a Report of non-fatal findings (enrolled students on no team). A
// team that references an identifier absent from the roster is a fatal error,
// returned with no units.
func Resolve(typ config.AssignmentType, r *roster.Roster, t *teams.Teams) ([]Unit, Report, error) {
	switch typ {
	case config.TypeIndividual:
		if t != nil {
			return nil, Report{}, fmt.Errorf("individual assignment does not take a teams file")
		}
		return resolveIndividual(r), Report{}, nil
	case config.TypeGroup:
		if t == nil {
			return nil, Report{}, fmt.Errorf("group assignment requires a teams file")
		}
		return resolveGroup(r, t)
	default:
		return nil, Report{}, fmt.Errorf("unknown assignment type %q", typ)
	}
}
