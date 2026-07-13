package unit

// Report carries non-fatal findings from resolving a group assignment. The
// finding is reported, not enforced, here: each command decides how to treat it.
// assign aborts before creating any repos (unless --force), while audit reports
// it and continues.
type Report struct {
	// UnassignedIDs lists enrolled students (roster identifiers) on no team.
	// This can be intentional (a student excused from the group work), so it is a
	// finding rather than a hard error at resolution time.
	UnassignedIDs []string

	// MultiTeam lists students (roster identifiers) that appear on more than one
	// team, each with the teams they appear on in file order. A student belongs on
	// exactly one team, so this is almost always a teams-file mistake.
	MultiTeam []MultiTeamMembership
}

// MultiTeamMembership is one student found on more than one team.
type MultiTeamMembership struct {
	ID    string
	Teams []string
}
