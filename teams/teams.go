// Package teams loads a per-assignment team-membership file: a YAML mapping of
// team name to a list of student identifiers.
//
// Like the roster, a teams file is per-semester PII keyed by student identifier
// (never GitHub handles) and must never be committed. This package only reads it
// into memory.
package teams

// Teams is an in-memory view of the membership file, retaining team names in
// file order for stable iteration.
type Teams struct {
	names   []string
	members map[string][]string
}

// Names returns the team names in the order they appeared in the file. The
// returned slice is owned by Teams and must not be mutated.
func (t *Teams) Names() []string { return t.names }

// Members returns the student identifiers on a team, or nil if no such team.
// The returned slice is owned by Teams and must not be mutated.
func (t *Teams) Members(team string) []string { return t.members[team] }

// Len reports how many teams are defined.
func (t *Teams) Len() int { return len(t.names) }
