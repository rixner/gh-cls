package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/rixner/gh-cls/unit"
)

// loadUnits enforces the roster/teams flag rules for the assignment type,
// parses the files, and resolves the unit list. rosterPath is always
// required; teamsPath is required for group and rejected for individual.
func loadUnits(name string, typ config.AssignmentType, rosterPath, teamsPath string) ([]unit.Unit, unit.Report, *roster.Roster, error) {
	switch typ {
	case config.TypeGroup:
		if teamsPath == "" {
			return nil, unit.Report{}, nil, fmt.Errorf("assignment %q is a group assignment: --teams is required", name)
		}
	case config.TypeIndividual:
		if teamsPath != "" {
			return nil, unit.Report{}, nil, fmt.Errorf("assignment %q is an individual assignment: --teams is not allowed", name)
		}
	}

	r, err := roster.ParseFile(rosterPath)
	if err != nil {
		return nil, unit.Report{}, nil, err
	}
	var tm *teams.Teams
	if typ == config.TypeGroup {
		if tm, err = teams.ParseFile(teamsPath); err != nil {
			return nil, unit.Report{}, nil, err
		}
	}
	units, report, err := unit.Resolve(typ, r, tm)
	if err != nil {
		return nil, unit.Report{}, nil, err
	}
	return units, report, r, nil
}

// printUnitWarnings prints the roster/teams consistency warnings from a
// unit.Report: enrolled students on no team, and students on more than one.
func printUnitWarnings(out io.Writer, report unit.Report) {
	for _, id := range report.UnassignedIDs {
		fmt.Fprintf(out, "warning: enrolled student %s is on no team\n", id)
	}
	for _, m := range report.MultiTeam {
		fmt.Fprintf(out, "warning: student %s is on more than one team: %s\n", m.ID, strings.Join(m.Teams, ", "))
	}
}
