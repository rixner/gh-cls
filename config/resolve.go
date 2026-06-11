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

// Overrides carries command-line values. A nil pointer (or empty Template) means
// the user did not set that flag, so the config value stands.
type Overrides struct {
	Template         string
	Public           *bool
	BranchProtection *bool
	Feedback         *string
}

// Resolve produces the effective Policy for an assignment by applying the
// precedence: command-line override, then the assignment's config entry, then
// the built-in default (private, no protection, no feedback).
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
	if ov.Template != "" {
		p.Template = ov.Template
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

	if p.Template == "" {
		return Policy{}, fmt.Errorf("assignment %q: no template configured (set assignments.%s.template or pass --template)", name, name)
	}
	return p, nil
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}
