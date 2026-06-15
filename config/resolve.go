package config

import "fmt"

// Policy is the fully-resolved per-assignment settings a command acts on.
type Policy struct {
	Type             AssignmentType
	Template         string
	Public           bool
	BranchProtection bool
	Feedback         string
}

// Overrides carries command-line policy values. A nil pointer means the user did
// not set that flag, so the config value stands. Template is not overridable: it
// is a property of the assignment, read only from config.
type Overrides struct {
	Public           *bool
	BranchProtection *bool
	Feedback         *string
}

// Resolve produces the effective Policy for an assignment by applying the
// precedence: command-line override, then the assignment's config entry, then
// the built-in default (private, no protection, no feedback). It does not require
// a template: only assign needs one, and it validates that itself.
func (c *Config) Resolve(name string, ov Overrides) (Policy, error) {
	a, ok := c.Assignments[name]
	if !ok {
		return Policy{}, fmt.Errorf("assignment %q not found in config", name)
	}

	p := Policy{
		Type:             a.Type,
		Template:         a.Template,
		Public:           boolOr(a.Public, false),
		BranchProtection: boolOr(a.BranchProtection, false),
		Feedback:         a.Feedback,
	}
	if ov.Public != nil {
		p.Public = *ov.Public
	}
	if ov.BranchProtection != nil {
		p.BranchProtection = *ov.BranchProtection
	}
	if ov.Feedback != nil {
		p.Feedback = *ov.Feedback
	}
	return p, nil
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}
