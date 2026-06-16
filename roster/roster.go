// Package roster loads the per-semester enrollment file mapping each student's
// identifier to a GitHub username.
//
// The file holds sensitive student data. This package only ever reads it
// into memory; it exposes no function that writes the roster or anything derived
// from it. Callers must likewise keep this data out of every repository.
package roster

import "strings"

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

// ByUsername returns the reverse mapping, GitHub username -> identifier, with
// usernames lower-cased because GitHub logins are case-insensitive. It lets a
// caller turn a username observed on GitHub (a collaborator or an invitation's
// invitee) back into the student identifier for an audit. The mapping is
// unambiguous: Parse rejects a roster that reuses a username, so no two
// identifiers can collide here.
func (r *Roster) ByUsername() map[string]string {
	rev := make(map[string]string, len(r.ids))
	for _, id := range r.ids {
		rev[strings.ToLower(r.byID[id])] = id
	}
	return rev
}

// UsersByLowercase maps each lower-cased GitHub username to its original
// spelling. The lower-cased key is the right comparison form (GitHub logins are
// case-insensitive) while the value preserves the spelling for display and API
// calls. The mapping is unambiguous because Parse rejects a reused username.
func (r *Roster) UsersByLowercase() map[string]string {
	m := make(map[string]string, len(r.ids))
	for _, id := range r.ids {
		u := r.byID[id]
		m[strings.ToLower(u)] = u
	}
	return m
}

// IDs returns the student identifiers in the order they appeared in the file.
// The returned slice is owned by the Roster and must not be mutated.
func (r *Roster) IDs() []string { return r.ids }

// Len reports how many students are enrolled.
func (r *Roster) Len() int { return len(r.ids) }
