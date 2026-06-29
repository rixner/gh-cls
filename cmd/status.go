package cmd

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// statusClient is the set of GitHub operations status needs. It only reads, so
// status does not require org ownership: a TA can run it. The collaborator and
// feedback lookups are used only by --detail.
type statusClient interface {
	GetTeam(ctx context.Context, org, slug string) (*gh.Team, bool, error)
	ListTeamMembers(ctx context.Context, org, slug string) ([]string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error)
	FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error)
}

// statusOpts carries the resolved flags and dependencies for `gh cls status`.
type statusOpts struct {
	g         *globalOpts
	detail    bool
	out       string
	now       func() time.Time
	newClient func(context.Context) (statusClient, error)
}

func newStatusCmd(g *globalOpts) *cobra.Command {
	o := &statusOpts{
		g:         g,
		now:       time.Now,
		newClient: func(context.Context) (statusClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "status [name]",
		Short: "Show what exists in the org: the staff team and each assignment's repositories",
		Long: `Report the current state of the course organization without changing anything:
the staff team and how many members it has, and for each assignment (or just
<name>) how many student repositories exist and their visibility, flagging any
that contradict the assignment's policy.

With --detail, scan each repository for its freeze state (write vs read for
non-admins) and its feedback issue/PR state (open, closed, or missing), printing
per-assignment counts and writing a per-repo CSV. --detail costs one or two API
calls per repository, so it scales with class size; the default summary does not.

Reads only, so it needs no org-owner role and is safe to run anytime.`,
		Example: `  gh cls status
  gh cls status hw1
  gh cls status hw1 --detail`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return o.run(cmd.Context(), cmd.OutOrStdout(), name)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.detail, "detail", false, "scan each repo for freeze and feedback state, writing a per-repo CSV")
	f.StringVarP(&o.out, "out", "o", "", "CSV path for --detail (default: a timestamped file in the current directory); never overwrites")
	return cmd
}

func (o *statusOpts) run(ctx context.Context, out io.Writer, name string) error {
	org := o.g.org
	names, err := o.resolveNames(name)
	if err != nil {
		return err
	}

	// --out implies the scan, and is the only way to ask for it besides --detail.
	detail := o.detail || o.out != ""

	// Fail fast (before any API call) when an explicit --out already exists: status
	// never overwrites a file. The O_EXCL create in writeCSV is the authoritative
	// guard; this just avoids a wasted scan.
	if detail && o.out != "" {
		if _, err := os.Stat(o.out); err == nil {
			return fmt.Errorf("refusing to overwrite %s; remove it or choose a different --out", o.out)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("checking --out %s: %w", o.out, err)
		}
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Org: %s\n", org)
	if err := o.printStaffLine(ctx, client, out); err != nil {
		return err
	}

	if detail {
		return o.runDetail(ctx, out, org, names, client)
	}
	return o.runSummary(ctx, out, org, names, client)
}

// resolveNames returns the assignment(s) to report: a named one (which must be in
// the config) or all of them in a stable order.
func (o *statusOpts) resolveNames(name string) ([]string, error) {
	if name != "" {
		if _, ok := o.g.cfg.Assignments[name]; !ok {
			return nil, fmt.Errorf("assignment %q not found in config", name)
		}
		return []string{name}, nil
	}
	names := make([]string, 0, len(o.g.cfg.Assignments))
	for n := range o.g.cfg.Assignments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// printStaffLine reports whether the staff team exists and its size.
func (o *statusOpts) printStaffLine(ctx context.Context, client statusClient, out io.Writer) error {
	_, exists, err := client.GetTeam(ctx, o.g.org, o.g.staffTeam)
	if err != nil {
		return fmt.Errorf("checking staff team %q: %w", o.g.staffTeam, err)
	}
	if !exists {
		fmt.Fprintf(out, "Staff team: %s (not found; run `gh cls setup`)\n", o.g.staffTeam)
		return nil
	}
	members, err := client.ListTeamMembers(ctx, o.g.org, o.g.staffTeam)
	if err != nil {
		return fmt.Errorf("listing staff team %q members: %w", o.g.staffTeam, err)
	}
	fmt.Fprintf(out, "Staff team: %s (%s)\n", o.g.staffTeam, plural(len(members), "member"))
	return nil
}

// --- cheap summary path -----------------------------------------------------

// assignmentStatus is the cheap (no per-repo scan) state of one assignment.
type assignmentStatus struct {
	name       string
	typ        string
	repos      int
	private    int
	public     int
	wantPublic bool
	err        error
}

func (o *statusOpts) runSummary(ctx context.Context, out io.Writer, org string, names []string, client statusClient) error {
	results := runConcurrent(ctx, o.g.concurrency, names, func(ctx context.Context, n string) assignmentStatus {
		return o.assignmentStatus(ctx, client, org, n)
	})

	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ASSIGNMENT\tTYPE\tREPOS\tVISIBILITY")
	for _, s := range results {
		if s.err != nil {
			fmt.Fprintf(tw, "%s\t%s\t?\t(unreadable)\n", s.name, s.typ)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.name, s.typ, s.repos, visibilitySummary(s))
	}
	tw.Flush()

	var failed int
	for _, s := range results {
		if s.err != nil {
			failed++
			fmt.Fprintf(out, "  FAILED %s: %v\n", s.name, s.err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d assignment(s) could not be read", failed)
	}
	return nil
}

// assignmentStatus counts the student repositories for one assignment, excluding
// the in-org template repository (as freeze does).
func (o *statusOpts) assignmentStatus(ctx context.Context, client statusClient, org, n string) assignmentStatus {
	policy, err := o.g.cfg.Resolve(n, config.Overrides{})
	if err != nil {
		return assignmentStatus{name: n, err: err}
	}
	s := assignmentStatus{name: n, typ: string(policy.Type), wantPublic: policy.Public}

	all, err := client.ListOrgReposByPrefix(ctx, org, n+"-")
	if err != nil {
		s.err = err
		return s
	}
	for _, r := range all {
		if r.IsTemplate {
			continue
		}
		s.repos++
		if r.Private {
			s.private++
		} else {
			s.public++
		}
	}
	return s
}

// visibilitySummary describes a repo count's private/public split and flags any
// visibility that contradicts the assignment's policy.
func visibilitySummary(s assignmentStatus) string {
	if s.repos == 0 {
		return "(none)"
	}
	var parts []string
	if s.private > 0 {
		parts = append(parts, fmt.Sprintf("%d private", s.private))
	}
	if s.public > 0 {
		parts = append(parts, fmt.Sprintf("%d public", s.public))
	}
	summary := strings.Join(parts, ", ")
	if (s.wantPublic && s.private > 0) || (!s.wantPublic && s.public > 0) {
		want := "private"
		if s.wantPublic {
			want = "public"
		}
		summary += fmt.Sprintf("  [policy: %s]", want)
	}
	return summary
}

// --- detail path ------------------------------------------------------------

// repoDetail is the scanned state of one student repository.
type repoDetail struct {
	assignment string
	repo       string
	key        string
	private    bool
	wantPublic bool
	frozen     string // frozen | writable | mixed | none
	feedback   string // open | closed | missing | none
	err        error
}

func (d repoDetail) visibility() string {
	if d.private {
		return "private"
	}
	return "public"
}

func (d repoDetail) expectedVisibility() string {
	if d.wantPublic {
		return "public"
	}
	return "private"
}

func (d repoDetail) row() []string {
	frozen, feedback := d.frozen, d.feedback
	if d.err != nil {
		frozen, feedback = "error", "error"
	}
	return []string{d.assignment, d.repo, d.key, d.visibility(), d.expectedVisibility(), frozen, feedback}
}

func (o *statusOpts) runDetail(ctx context.Context, out io.Writer, org string, names []string, client statusClient) error {
	type work struct {
		assignment string
		policy     config.Policy
		repo       gh.Repo
	}
	var items []work
	var problems []string
	for _, n := range names {
		policy, err := o.g.cfg.Resolve(n, config.Overrides{})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", n, err))
			continue
		}
		all, err := client.ListOrgReposByPrefix(ctx, org, n+"-")
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: listing repositories: %v", n, err))
			continue
		}
		for _, r := range all {
			if r.IsTemplate {
				continue
			}
			items = append(items, work{n, policy, r})
		}
	}

	details := runConcurrent(ctx, o.g.concurrency, items, func(ctx context.Context, w work) repoDetail {
		return o.scanRepo(ctx, client, org, w.assignment, w.policy, w.repo)
	})

	// Write the per-repo CSV first (it is what the run produces); a failure to
	// write aborts before printing a summary that promises a file that is not there.
	var csvNote string
	if len(details) > 0 {
		path, err := o.writeCSV(org, names, details)
		if err != nil {
			return err
		}
		csvNote = fmt.Sprintf("\nwrote per-repo detail to\n  %s  (%s)\n", path, plural(len(details), "row"))
	}

	printDetailSummary(out, o.g.cfg, names, details)
	if csvNote != "" {
		fmt.Fprint(out, csvNote)
	}

	var failed int
	for _, d := range details {
		if d.err != nil {
			failed++
		}
	}
	if failed > 0 || len(problems) > 0 {
		fmt.Fprintln(out)
		for _, p := range problems {
			fmt.Fprintf(out, "  FAILED %s\n", p)
		}
		for _, d := range details {
			if d.err != nil {
				fmt.Fprintf(out, "  FAILED %s: %v\n", d.repo, d.err)
			}
		}
		return fmt.Errorf("%d repo(s) and %d assignment(s) could not be read", failed, len(problems))
	}
	return nil
}

// scanRepo reads one repository's freeze and feedback state.
func (o *statusOpts) scanRepo(ctx context.Context, client statusClient, org, assignment string, policy config.Policy, r gh.Repo) repoDetail {
	d := repoDetail{
		assignment: assignment,
		repo:       r.Name,
		key:        strings.TrimPrefix(r.Name, assignment+"-"),
		private:    r.Private,
		wantPublic: policy.Public,
	}
	collabs, err := client.ListDirectCollaborators(ctx, org, r.Name)
	if err != nil {
		d.err = fmt.Errorf("listing collaborators: %w", err)
		return d
	}
	d.frozen = classifyFrozen(collabs)

	state, err := feedbackArtifactState(ctx, client, org, r.Name, policy.Feedback)
	if err != nil {
		d.err = fmt.Errorf("reading feedback artifact: %w", err)
		return d
	}
	d.feedback = state
	return d
}

// classifyFrozen reports a repo's freeze state from its direct collaborators,
// using the same write/read distinction freeze applies (push/maintain/triage are
// write; admins are ignored, since staff keep access through a freeze).
func classifyFrozen(collabs []gh.Collaborator) string {
	var write, read int
	for _, c := range collabs {
		if c.Permissions.Admin {
			continue
		}
		switch {
		case c.Permissions.Push || c.Permissions.Maintain || c.Permissions.Triage:
			write++
		case c.Permissions.Pull:
			read++
		}
	}
	switch {
	case write == 0 && read == 0:
		return "none"
	case write > 0 && read > 0:
		return "mixed"
	case write > 0:
		return "writable"
	default:
		return "frozen"
	}
}

// feedbackArtifactState reports the state of a repo's feedback artifact for the
// assignment's mode: open/closed, "missing" if the artifact is absent, or "none"
// when the assignment configures no feedback.
func feedbackArtifactState(ctx context.Context, client statusClient, org, repo, mode string) (string, error) {
	switch mode {
	case feedbackIssue:
		_, state, found, err := client.FindIssueByTitle(ctx, org, repo, feedbackTitle)
		if err != nil {
			return "", err
		}
		if !found {
			return "missing", nil
		}
		return state, nil
	case feedbackPR:
		_, state, found, err := client.FindPRByBase(ctx, org, repo, feedbackBranch)
		if err != nil {
			return "", err
		}
		if !found {
			return "missing", nil
		}
		return state, nil
	default:
		return "none", nil
	}
}

// printDetailSummary prints a per-assignment block of freeze and feedback counts.
func printDetailSummary(out io.Writer, cfg *config.Config, names []string, details []repoDetail) {
	byAssignment := make(map[string][]repoDetail, len(names))
	for _, d := range details {
		byAssignment[d.assignment] = append(byAssignment[d.assignment], d)
	}
	for _, n := range names {
		ds := byAssignment[n]
		a := cfg.Assignments[n]
		fmt.Fprintf(out, "\n%s (%s): %s\n", n, a.Type, frozenSummary(ds))
		if a.Feedback == config.FeedbackNone {
			fmt.Fprintf(out, "  feedback: not configured\n")
		} else {
			fmt.Fprintf(out, "  feedback: %s\n", feedbackSummary(ds))
		}
	}
}

// frozenSummary renders the repo count and a breakdown of freeze states, listing
// only the non-zero categories (so a partial freeze's "mixed" stands out).
func frozenSummary(ds []repoDetail) string {
	if len(ds) == 0 {
		return "0 repos"
	}
	var frozen, writable, mixed, none, errored int
	for _, d := range ds {
		if d.err != nil {
			errored++
			continue
		}
		switch d.frozen {
		case "frozen":
			frozen++
		case "writable":
			writable++
		case "mixed":
			mixed++
		default:
			none++
		}
	}
	var parts []string
	for _, b := range []struct {
		n     int
		label string
	}{{frozen, "frozen"}, {writable, "writable"}, {mixed, "mixed"}, {none, "no-collaborators"}, {errored, "error"}} {
		if b.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", b.n, b.label))
		}
	}
	return fmt.Sprintf("%s | %s", plural(len(ds), "repo"), strings.Join(parts, ", "))
}

// feedbackSummary renders the open/closed/missing breakdown of feedback artifacts.
func feedbackSummary(ds []repoDetail) string {
	var open, closed, missing, errored int
	for _, d := range ds {
		if d.err != nil {
			errored++
			continue
		}
		switch d.feedback {
		case "open":
			open++
		case "closed":
			closed++
		case "missing":
			missing++
		}
	}
	s := fmt.Sprintf("%d open, %d closed, %d missing", open, closed, missing)
	if errored > 0 {
		s += fmt.Sprintf(", %d error", errored)
	}
	return s
}

// writeCSV writes the per-repo rows to a never-overwritten file and returns its
// path. A write failure removes the partial file so no half-written CSV is left.
func (o *statusOpts) writeCSV(org string, names []string, details []repoDetail) (string, error) {
	f, path, err := o.createCSV(csvLabel(org, names))
	if err != nil {
		return "", err
	}
	w := csv.NewWriter(f)
	werr := w.Write([]string{"assignment", "repo", "key", "visibility", "expected_visibility", "frozen", "feedback"})
	for _, d := range details {
		if werr != nil {
			break
		}
		werr = w.Write(d.row())
	}
	w.Flush()
	if werr == nil {
		werr = w.Error()
	}
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(path)
		if werr != nil {
			return "", fmt.Errorf("writing %s: %w", path, werr)
		}
		return "", fmt.Errorf("closing %s: %w", path, cerr)
	}
	return path, nil
}

// createCSV opens the destination file without ever overwriting one. An explicit
// --out that exists is an error; the auto name carries a second-resolution
// timestamp and, on a collision (a same-second re-run), rolls to -2, -3, ... The
// O_EXCL create is the authoritative, race-safe uniqueness check.
func (o *statusOpts) createCSV(label string) (*os.File, string, error) {
	if o.out != "" {
		f, err := os.OpenFile(o.out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return nil, "", fmt.Errorf("refusing to overwrite %s; remove it or choose a different --out", o.out)
			}
			return nil, "", fmt.Errorf("creating %s: %w", o.out, err)
		}
		return f, o.out, nil
	}
	base := fmt.Sprintf("gh-cls-status-%s-%s", label, o.now().Format("20060102-150405"))
	for i := 1; i <= 1000; i++ {
		name := base + ".csv"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.csv", base, i)
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("creating %s: %w", name, err)
		}
	}
	return nil, "", fmt.Errorf("could not find a free filename like %s.csv after 1000 tries; clean up old status files", base)
}

// csvLabel names the auto file after the single assignment, or the org for a
// whole-course run.
func csvLabel(org string, names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return org
}

// plural renders a count with its noun, adding "s" for any count other than one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
