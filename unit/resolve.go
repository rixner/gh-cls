package unit

import (
	"fmt"
	"strings"

	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
)

// resolveIndividual yields one unit per enrolled student, in roster order, each
// keyed by and granting push to that student's username.
func resolveIndividual(r *roster.Roster) []Unit {
	units := make([]Unit, 0, r.Len())
	for _, id := range r.IDs() {
		username, _ := r.Lookup(id) // present by construction of the roster
		units = append(units, Unit{Key: username, Members: []string{username}})
	}
	return units
}

// resolveGroup yields one unit per team, in teams-file order, with each team's
// identifiers resolved to usernames through the roster. An identifier missing
// from the roster is a fatal error reported across all teams at once; enrolled
// students on no team are returned as a warning.
func resolveGroup(r *roster.Roster, t *teams.Teams) ([]Unit, Report, error) {
	// teamsByID records, for each identifier, the teams it appears on (in file
	// order). It drives both findings: zero teams -> unassigned, more than one ->
	// multi-team. The teams parser guarantees an identifier is unique within a team
	// and team names are unique, so these are inherently distinct teams.
	teamsByID := make(map[string][]string)
	var missing []string
	units := make([]Unit, 0, t.Len())

	// Lower-cased identifier index, used only to turn a case-only mismatch (a
	// common copy error between the teams file and the roster) into an actionable
	// error rather than a bare "not in the roster".
	lowerID := make(map[string]string, r.Len())
	for _, id := range r.IDs() {
		lowerID[strings.ToLower(id)] = id
	}

	for _, name := range t.Names() {
		ids := t.Members(name)
		members := make([]string, 0, len(ids))
		for _, id := range ids {
			teamsByID[id] = append(teamsByID[id], name)
			username, ok := r.Lookup(id)
			if !ok {
				entry := fmt.Sprintf("team %s: %s", name, id)
				if canon, near := lowerID[strings.ToLower(id)]; near {
					entry += fmt.Sprintf(" (roster has %q; identifiers are case-sensitive)", canon)
				}
				missing = append(missing, entry)
				continue
			}
			members = append(members, username)
		}
		units = append(units, Unit{Key: name, Members: members})
	}

	if len(missing) > 0 {
		return nil, Report{}, fmt.Errorf("teams reference identifiers not in the roster:\n  %s",
			strings.Join(missing, "\n  "))
	}

	var unassigned []string
	var multi []MultiTeamMembership
	for _, id := range r.IDs() {
		switch on := teamsByID[id]; {
		case len(on) == 0:
			unassigned = append(unassigned, id)
		case len(on) > 1:
			multi = append(multi, MultiTeamMembership{ID: id, Teams: on})
		}
	}
	return units, Report{UnassignedIDs: unassigned, MultiTeam: multi}, nil
}
