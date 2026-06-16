// Package config loads the reusable course-structure file: course-wide settings
// (the org and staff team) plus a per-assignment policy dictionary. The file is
// user-authored and located explicitly (-c/--config or $GH_CLS_CONFIG); the tool
// only reads it, never writes it. It holds no student PII.
package config

import "fmt"

// AssignmentType distinguishes the two unit sources: one repo per student, or
// one repo per team.
type AssignmentType string

const (
	TypeIndividual AssignmentType = "individual"
	TypeGroup      AssignmentType = "group"
)

// Feedback modes a config may request for an assignment.
const (
	FeedbackNone  = ""
	FeedbackPR    = "pr"
	FeedbackIssue = "issue"
)

// Assignment is one entry under the config's `assignments` map. Optional policy
// flags are pointers so an unset value (nil) is distinguishable from an explicit
// false during resolution.
type Assignment struct {
	Type             AssignmentType `yaml:"type"`
	Template         string         `yaml:"template"`
	Public           *bool          `yaml:"public,omitempty"`
	BranchProtection *bool          `yaml:"branch_protection,omitempty"`
	Feedback         string         `yaml:"feedback,omitempty"`
}

// Config is the parsed course-structure file.
type Config struct {
	Org string `yaml:"org"`
	// StaffTeam is optional: set it to the staff team slug to have setup ensure
	// the team and assign grant it access to each repo. Omitting it runs the
	// course with no staff team (those steps are skipped); only the staff command,
	// whose sole job is managing that team, requires it.
	StaffTeam   string                `yaml:"staff_team"`
	Assignments map[string]Assignment `yaml:"assignments"`
}

// Validate rejects a config that lacks the required org and any malformed
// assignment entries. Only org is required; staff_team is optional (see the
// field doc). It is also intentionally lenient about an empty template (a
// --template flag can supply it at run time); that is enforced when an
// assignment is actually resolved.
func (c *Config) Validate() error {
	if c.Org == "" {
		return fmt.Errorf("missing required \"org\" key; the config must set at least:\n\n  org: your-semester-org\n\n(staff_team is optional; omit it for a course with no staff team)")
	}
	for name, a := range c.Assignments {
		switch a.Type {
		case TypeIndividual, TypeGroup:
		case "":
			return fmt.Errorf("assignment %q: missing required \"type\" (individual or group)", name)
		default:
			return fmt.Errorf("assignment %q: invalid type %q (want individual or group)", name, a.Type)
		}
		switch a.Feedback {
		case FeedbackNone, FeedbackPR, FeedbackIssue:
		default:
			return fmt.Errorf("assignment %q: invalid feedback %q (want pr or issue)", name, a.Feedback)
		}
	}
	return nil
}
