package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// status classifies the outcome of one idempotent action.
type status int

const (
	statusAlready  status = iota // already in the desired state
	statusChanged                // this run changed it
	statusReported               // informational; nothing to change
	statusWarning                // needs attention (e.g. a manual step)
)

func (s status) symbol() string {
	switch s {
	case statusChanged:
		return "changed"
	case statusAlready:
		return "already"
	case statusWarning:
		return "warning"
	default:
		return "noted"
	}
}

// result is one line of an action report.
type result struct {
	label  string
	status status
	detail string
}

// printResults writes the results as an aligned table.
func printResults(w io.Writer, results []result) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.status.symbol(), r.label, r.detail)
	}
	tw.Flush()
}

// printSteps writes a titled bullet list, or nothing when there are no steps.
func printSteps(w io.Writer, title string, steps []string) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, s := range steps {
		fmt.Fprintf(w, "  - %s\n", s)
	}
}

// printManualSteps lists actions the tool cannot perform via the API.
func printManualSteps(w io.Writer, steps []string) {
	printSteps(w, "Manual steps (cannot be done via the API on this tier):", steps)
}

// printOptionalHardening lists member-privilege restrictions the tool cannot set
// (they exist only in the web UI) and that are the instructor's to apply or not.
func printOptionalHardening(w io.Writer, steps []string) {
	printSteps(w, "Optional hardening (instructor's discretion — Settings → Member privileges, web UI only):", steps)
}
