package unit

import (
	"fmt"
	"strings"

	"github.com/rixner/gh-cls/groups"
	"github.com/rixner/gh-cls/roster"
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

// resolveGroup yields one unit per group, in groups-file order, with each
// group's identifiers resolved to usernames through the roster. An identifier
// missing from the roster is a fatal error reported across all groups at once;
// enrolled students in no group are returned as a warning.
func resolveGroup(r *roster.Roster, g *groups.Groups) ([]Unit, Report, error) {
	// groupsByID records, for each identifier, the groups it appears in (in file
	// order). It drives both findings: zero groups -> unassigned, more than one ->
	// multi-group. The groups parser guarantees an identifier is unique within a
	// group and group names are unique, so these are inherently distinct groups.
	groupsByID := make(map[string][]string)
	var missing []string
	units := make([]Unit, 0, g.Len())

	// Lower-cased identifier index, used only to turn a case-only mismatch (a
	// common copy error between the groups file and the roster) into an actionable
	// error rather than a bare "not in the roster".
	lowerID := make(map[string]string, r.Len())
	for _, id := range r.IDs() {
		lowerID[strings.ToLower(id)] = id
	}

	for _, name := range g.Names() {
		ids := g.Members(name)
		members := make([]string, 0, len(ids))
		for _, id := range ids {
			groupsByID[id] = append(groupsByID[id], name)
			username, ok := r.Lookup(id)
			if !ok {
				entry := fmt.Sprintf("group %s: %s", name, id)
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
		return nil, Report{}, fmt.Errorf("groups reference identifiers not in the roster:\n  %s",
			strings.Join(missing, "\n  "))
	}

	var unassigned []string
	var multi []MultiGroupMembership
	for _, id := range r.IDs() {
		switch on := groupsByID[id]; {
		case len(on) == 0:
			unassigned = append(unassigned, id)
		case len(on) > 1:
			multi = append(multi, MultiGroupMembership{ID: id, Groups: on})
		}
	}
	return units, Report{UnassignedIDs: unassigned, MultiGroup: multi}, nil
}
