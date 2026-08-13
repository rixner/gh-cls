package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/groups"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/unit"
)

// loadUnits enforces the roster/groups flag rules for the assignment type,
// parses the files, and resolves the unit list. rosterPath is always
// required; groupsPath is required for group and rejected for individual.
func loadUnits(name string, typ config.AssignmentType, rosterPath, groupsPath string) ([]unit.Unit, unit.Report, *roster.Roster, error) {
	switch typ {
	case config.TypeGroup:
		if groupsPath == "" {
			return nil, unit.Report{}, nil, fmt.Errorf("assignment %q is a group assignment: --groups is required", name)
		}
	case config.TypeIndividual:
		if groupsPath != "" {
			return nil, unit.Report{}, nil, fmt.Errorf("assignment %q is an individual assignment: --groups is not allowed", name)
		}
	}

	r, err := roster.ParseFile(rosterPath)
	if err != nil {
		return nil, unit.Report{}, nil, err
	}
	var g *groups.Groups
	if typ == config.TypeGroup {
		if g, err = groups.ParseFile(groupsPath); err != nil {
			return nil, unit.Report{}, nil, err
		}
	}
	units, report, err := unit.Resolve(typ, r, g)
	if err != nil {
		return nil, unit.Report{}, nil, err
	}
	return units, report, r, nil
}

// printUnitWarnings prints the roster/groups consistency warnings from a
// unit.Report: enrolled students in no group, and students in more than one.
func printUnitWarnings(out io.Writer, report unit.Report) {
	for _, id := range report.UnassignedIDs {
		fmt.Fprintf(out, "warning: enrolled student %s is in no group\n", id)
	}
	for _, m := range report.MultiGroup {
		fmt.Fprintf(out, "warning: student %s is in more than one group: %s\n", m.ID, strings.Join(m.Groups, ", "))
	}
}
