package unit

// Report carries non-fatal findings from resolving a group assignment.
type Report struct {
	// UnassignedIDs lists enrolled students (roster identifiers) on no team.
	// This is often intentional (a student excused from the group work) but is
	// surfaced as a warning so it is never silent.
	UnassignedIDs []string
}
