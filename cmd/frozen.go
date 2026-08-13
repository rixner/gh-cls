package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rixner/gh-cls/gh"
)

// frozenProperty is the organization custom property in which freeze records
// whether a repository is past its deadline.
//
// The record exists because freeze state cannot be inferred. A repository whose
// only student has no access yet (expired or never issued) has no permission to
// read the state from, and an individual assignment's repo has exactly one such
// student, so inference goes blind precisely where audit --renew must decide
// what access to restore. Scoping the question to the whole assignment does not
// help either: `freeze <name> <key> --undo` grants an extension on one repo, so a
// mixed assignment is a normal state, not an anomaly.
//
// A custom property is the right home for it. It is not a git ref, so no push
// can touch it; its schema is declared org-wide with values_editable_by set to
// organization actors, so no repository-level role can rewrite it; it is
// available on free organizations, so it does not put the deadline behind a paid
// plan; and it updates one named property at a time, so there is no
// read-modify-write race.
const frozenProperty = "gh-cls-frozen"

const frozenPropertyDescription = "Set by gh cls freeze: true once the assignment deadline has passed"

// freezeState is a repository's recorded freeze intent. The values are exactly
// what the custom property stores, so no translation is needed at the API edge.
type freezeState string

const (
	// freezeUnset is a repository that has never been stamped: created before this
	// record existed, or never frozen. It is deliberately distinct from
	// freezeThawed so "never frozen" is never mistaken for "extension granted".
	freezeUnset freezeState = ""
	// freezeThawed is a repository deliberately made writable again, by --undo.
	freezeThawed freezeState = "false"
	// freezeFrozen is a repository frozen at its deadline.
	freezeFrozen freezeState = "true"
)

// grantPermission returns the collaborator permission a student should be given
// on a repository in this state. Only a recorded freeze withholds write; an
// unstamped repository predates the record or was never frozen, and in both
// cases the assignment is open.
func (s freezeState) grantPermission() string {
	if s == freezeFrozen {
		return "pull"
	}
	return "push"
}

// describe renders the state for a report.
func (s freezeState) describe() string {
	switch s {
	case freezeFrozen:
		return "frozen"
	case freezeThawed:
		return "thawed"
	default:
		return "not recorded"
	}
}

// frozenStates reduces an org-wide custom property listing to each repository's
// freeze state. Repositories with no recorded value are simply absent from the
// map and read as freezeUnset, which is the correct default.
func frozenStates(values map[string]map[string]string) map[string]freezeState {
	out := make(map[string]freezeState, len(values))
	for repo, props := range values {
		if v, ok := props[frozenProperty]; ok && v != "" {
			out[repo] = freezeState(v)
		}
	}
	return out
}

// propertyReader is the read side of the freeze record, shared by the commands
// that only consult it.
type propertyReader interface {
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	ListRepoPropertyValues(ctx context.Context, org string) (map[string]map[string]string, error)
}

// checkGrantRace re-reads the freeze record after a run that granted write and
// returns the repos that are recorded frozen despite having just been granted
// push. That means a freeze landed while this run was in flight, on a repo this
// run had already read as unfrozen.
//
// This detects, it does not prevent. Closing the window would need an atomic
// compare-and-set across two separate GitHub resources (the property and the
// collaborator grant), which the API does not offer, so the honest guarantee is
// that a concurrent freeze is never silent: the run fails and names the repos to
// re-freeze. Callers must run this after their grants, not before.
func checkGrantRace(ctx context.Context, client propertyReader, org string, grantedWrite []string) ([]string, error) {
	if len(grantedWrite) == 0 {
		return nil, nil
	}
	states, err := readFrozenStates(ctx, client, org)
	if err != nil {
		return nil, fmt.Errorf("re-reading the freeze record to check for a concurrent freeze: %w", err)
	}
	var reopened []string
	for _, repo := range grantedWrite {
		if states[repo] == freezeFrozen {
			reopened = append(reopened, repo)
		}
	}
	sort.Strings(reopened)
	return reopened, nil
}

// grantRaceError renders the failure for repos a concurrent freeze reopened,
// naming the command that puts them back.
func grantRaceError(name string, reopened []string) error {
	return fmt.Errorf("a freeze landed on %s while this run was granting access, so %s may now be writable past the deadline:\n  %s\nre-run `gh cls freeze %s` to re-assert the lock, and avoid running two gh-cls commands against one org at the same time",
		strings.Join(reopened, ", "), plural(len(reopened), "repository"), strings.Join(reopened, "\n  "), name)
}

// readFrozenStates returns each repository's recorded freeze state, or an error
// naming the fix when the property has not been declared. A missing declaration
// is reported rather than treated as "nothing is frozen": that default would let
// a command re-grant write across an assignment whose deadline has passed, which
// is the exact failure this record exists to prevent.
func readFrozenStates(ctx context.Context, client propertyReader, org string) (map[string]freezeState, error) {
	if _, ok, err := client.GetPropertyDefinition(ctx, org, frozenProperty); err != nil {
		return nil, fmt.Errorf("reading the %q organization property on %s: %w", frozenProperty, org, err)
	} else if !ok {
		return nil, fmt.Errorf("the %q organization property does not exist on %s, so no repository's freeze state can be read; run `gh cls setup` to declare it", frozenProperty, org)
	}
	values, err := client.ListRepoPropertyValues(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reading %q values across %s: %w", frozenProperty, org, err)
	}
	return frozenStates(values), nil
}
