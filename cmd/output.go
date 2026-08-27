package cmd

import (
	"fmt"
	"io"
	"strconv"
	"sync"
	"text/tabwriter"
)

// syncWriter serializes writes to a command's output. A bulk run has more than
// one goroutine that prints: the worker reporting a finished repository, and the
// client reporting a rate-limit pause from whichever request ran into it. Their
// lines would otherwise interleave, and on an output that is not a file the
// concurrent writes are a race outright.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// actionStatus classifies the outcome of one idempotent action.
type actionStatus int

const (
	statusAlready  actionStatus = iota // already in the desired state
	statusChanged                      // this run changed it
	statusReported                     // informational; nothing to change
	statusWarning                      // needs attention (e.g. a manual step)
)

func (s actionStatus) symbol() string {
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
	status actionStatus
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
	printSteps(w, "Optional hardening (instructor's discretion; Settings → Member privileges, web UI only):", steps)
}

// progress prints one numbered line per item as a bulk run finishes it. A full
// class at -j 1 runs for minutes, and silence that long is indistinguishable
// from a run that is stuck; it also leaves no record of how far a run got if it
// has to be interrupted.
//
// The number counts items finished, not position in the roster: results arrive
// in completion order, which at any concurrency above 1 is not input order.
// outcome may be empty for a command whose per-item result has no one-word
// summary, leaving a line that just names what finished.
type progress struct {
	out     io.Writer
	total   int
	width   int
	outcome int
	n       int
}

// newProgress prepares the printer. outcomeWidth is the length of the longest
// outcome the caller will report, which keeps the target column straight; pass 0
// for a caller that reports no outcomes.
func newProgress(out io.Writer, total, outcomeWidth int) *progress {
	return &progress{out: out, total: total, width: len(strconv.Itoa(total)), outcome: outcomeWidth}
}

// item reports one finished item. runConcurrentProgress serializes the calls, so
// this needs no locking. Outcomes are kept to one short word: the detail behind a
// failure or a qualified status belongs in the end-of-run report, where every one
// of them is listed together.
func (p *progress) item(outcome, target string) {
	p.n++
	if outcome == "" {
		fmt.Fprintf(p.out, "  [%*d/%d] %s\n", p.width, p.n, p.total, target)
		return
	}
	fmt.Fprintf(p.out, "  [%*d/%d] %-*s %s\n", p.width, p.n, p.total, p.outcome, outcome, target)
}

// failedOr returns "FAILED" when err is set, so every command's progress line
// agrees on how a failure looks.
func failedOr(err error, outcome string) string {
	if err != nil {
		return "FAILED"
	}
	return outcome
}
