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

// printManualSteps lists actions the tool cannot perform via the API.
func printManualSteps(w io.Writer, steps []string) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintln(w, "\nManual steps (cannot be done via the API on this tier):")
	for _, s := range steps {
		fmt.Fprintf(w, "  - %s\n", s)
	}
}
