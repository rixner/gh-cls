package unit

// Report carries non-fatal findings from resolving a group assignment. The
// finding is reported, not enforced, here: each command decides how to treat it.
// assign aborts before creating any repos (unless --force), while audit reports
// it and continues.
type Report struct {
	// UnassignedIDs lists enrolled students (roster identifiers) in no group.
	// This can be intentional (a student excused from the group work), so it is a
	// finding rather than a hard error at resolution time.
	UnassignedIDs []string

	// MultiGroup lists students (roster identifiers) that appear in more than one
	// group, each with the groups they appear in in file order. A student belongs
	// in exactly one group, so this is almost always a groups-file mistake.
	MultiGroup []MultiGroupMembership
}

// MultiGroupMembership is one student found in more than one group.
type MultiGroupMembership struct {
	ID     string
	Groups []string
}
