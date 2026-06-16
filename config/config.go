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
	// StaffTeam is the slug of the course's staff team. It is required, but may
	// have no members: setup ensures the team exists, assign grants it access to
	// each repo, and the staff command manages its membership. Requiring it up
	// front means a TA added later inherits access to every already-created
	// assignment, which is impossible if the team did not exist when the repos
	// were made.
	StaffTeam   string                `yaml:"staff_team"`
	Assignments map[string]Assignment `yaml:"assignments"`
}

// Validate rejects a config that lacks the required org or staff_team, or that
// has a malformed assignment entry. It is intentionally lenient about an empty
// template; that is enforced when an assignment is actually resolved.
func (c *Config) Validate() error {
	if c.Org == "" {
		return fmt.Errorf("missing required \"org\" key; the config must set at least:\n\n  org: your-semester-org\n  staff_team: your-staff-team-slug")
	}
	if c.StaffTeam == "" {
		return fmt.Errorf("missing required \"staff_team\" key; name the staff team's slug:\n\n  staff_team: your-staff-team-slug\n\nThe team may have no members yet — setup creates it and assign grants it access to every repo, so a TA added later inherits access to all existing assignments.")
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
