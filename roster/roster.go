// Package roster loads the per-semester enrollment file mapping each student's
// identifier to a GitHub username.
//
// The file holds sensitive student data. This package only ever reads it
// into memory; it exposes no function that writes the roster or anything derived
// from it. Callers must likewise keep this data out of every repository.
package roster

// Roster is an in-memory view of the enrollment file: identifier -> GitHub
// username, with identifiers retained in file order for stable iteration.
type Roster struct {
	byID map[string]string
	ids  []string
}

// Lookup returns the GitHub username for an identifier and whether it was found.
func (r *Roster) Lookup(id string) (string, bool) {
	u, ok := r.byID[id]
	return u, ok
}

// IDs returns the student identifiers in the order they appeared in the file.
// The returned slice is owned by the Roster and must not be mutated.
func (r *Roster) IDs() []string { return r.ids }

// Len reports how many students are enrolled.
func (r *Roster) Len() int { return len(r.ids) }
