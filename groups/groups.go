// Package groups loads a per-assignment group-membership file: a YAML mapping of
// group name to a list of student identifiers.
//
// A group is a set of students who work together and share one repository, which
// is what a "group" assignment in the config means. It is unrelated to a GitHub
// team; the only team this tool manages is the course staff team.
//
// Like the roster, a groups file is per-semester PII keyed by student identifier
// (never GitHub handles) and must never be committed. This package only reads it
// into memory.
package groups

// Groups is an in-memory view of the membership file, retaining group names in
// file order for stable iteration.
type Groups struct {
	names   []string
	members map[string][]string
}

// Names returns the group names in the order they appeared in the file. The
// returned slice is owned by Groups and must not be mutated.
func (g *Groups) Names() []string { return g.names }

// Members returns the student identifiers in a group, or nil if no such group.
// The returned slice is owned by Groups and must not be mutated.
func (g *Groups) Members(group string) []string { return g.members[group] }

// Len reports how many groups are defined.
func (g *Groups) Len() int { return len(g.names) }
