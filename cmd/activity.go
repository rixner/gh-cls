package cmd

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// activityClient is the narrow set of GitHub operations activity needs. It only
// reads, so like status it requires no org-owner role: a TA can run it.
type activityClient interface {
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListRepoActivity(ctx context.Context, owner, repo, ref string) ([]gh.Activity, error)
	GetRef(ctx context.Context, owner, repo, ref string) (string, error)
	CommitExists(ctx context.Context, owner, repo, sha string) (bool, error)
}

// activityOpts carries the resolved flags and dependencies for `gh cls activity`.
type activityOpts struct {
	g         *globalOpts
	branch    string
	since     string
	until     string
	pin       bool
	all       bool
	forced    bool
	deleted   bool
	out       string
	now       func() time.Time
	newClient func(context.Context) (activityClient, error)
}

func newActivityCmd(g *globalOpts) *cobra.Command {
	o := &activityOpts{
		g:         g,
		now:       time.Now,
		newClient: func(context.Context) (activityClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "activity <name>",
		Short: "Report pushes, force pushes and deletions on an assignment's repositories",
		Long: `Read GitHub's record of who moved which branch when, for the <name>-* repos.

With no mode flag, print a per-repo summary of what happened. -a summarizes every
recorded change by who made it, which answers "who has been pushing to this
repo"; with -o it also writes every individual change as CSV, since listing them
all on a terminal is thousands of lines. Those are push counts, not a measure of
contribution. -f lists force pushes and -d lists
branch deletions, both of which an assignment's branch-protection ruleset would
prevent, but that ruleset needs a paid plan for private repositories: on a free
organization this is how you see what you cannot block. A deletion's "before"
commit is the tip that was removed, which is often still fetchable, so the
report doubles as a way back to deleted work.

-p writes a pin file mapping each student/group key to the commit their repo was
at, for feeding to ` + "`gh cls collect --commits`" + `. That gives every student the same
deadline instant, unlike freezing and collecting, where repos are locked over
the duration of the freeze. Pair it with -f: a force push after the deadline can
orphan a pinned commit, so -p verifies every SHA it writes is still retrievable
and refuses to write a file it knows is broken.

--since and --until bound the window; --until defaults to now. Modes combine on
the terminal, but -o writes one artifact, so it takes a single mode.

Timestamps are GitHub's own record of when each change happened, not commit
dates (which the pusher controls) and not webhook receipt times (which lag).

Reads only, so it needs no org-owner role.`,
		Example: `  gh cls activity hw1
  gh cls activity hw1 -a
  gh cls activity hw1 -f -d
  gh cls activity project -p -u 2026-03-01T23:59:59-06:00 -o deadline.yml
  gh cls activity hw1 -f -s 2026-02-01T00:00:00Z`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.branch, "branch", "b", "", "branch to report on (default: each repo's default branch)")
	f.StringVarP(&o.since, "since", "s", "", "only activity at or after this RFC3339 time")
	f.StringVarP(&o.until, "until", "u", "", "only activity at or before this RFC3339 time (default: now)")
	f.BoolVarP(&o.pin, "pin", "p", false, "write each repo's commit as of --until, for collect --commits")
	f.BoolVarP(&o.all, "all", "a", false, "list every recorded change, not just force pushes and deletions")
	f.BoolVarP(&o.forced, "force-pushes", "f", false, "list force pushes")
	f.BoolVarP(&o.deleted, "deletions", "d", false, "list branch deletions")
	f.StringVarP(&o.out, "out", "o", "", "write the output to a file (YAML for -p, CSV otherwise)")
	return cmd
}

// repoActivity is one repository's recorded history, already filtered to the
// branch and window under report.
type repoActivity struct {
	repo   string
	key    string
	branch string
	// tip is the branch's current commit, used to check that GitHub's record has
	// caught up with reality.
	tip    string
	events []gh.Activity
	// postWindow holds activity after --until. A force push here can orphan the
	// commit -p just pinned, which is the case worth reporting; a force push
	// inside the window merely rewrote history before the pin and leaves it
	// valid. Use -f to see those.
	postWindow []gh.Activity
	err        error
}

func (o *activityOpts) run(ctx context.Context, out io.Writer, name string) error {
	if _, ok := o.g.cfg.Assignments[name]; !ok {
		return fmt.Errorf("assignment %q not found in config", name)
	}

	since, until, err := o.window()
	if err != nil {
		return err
	}
	modes := 0
	for _, on := range []bool{o.pin, o.all, o.forced, o.deleted} {
		if on {
			modes++
		}
	}
	// One file holds one artifact: a pin file and a force-push listing have
	// different shapes, so there is no sensible way to write both to -o.
	if o.out != "" && modes != 1 {
		return fmt.Errorf("--out writes a single artifact, so it needs exactly one of -p, -a, -f or -d (got %d)", modes)
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	repos, err := client.ListOrgReposByPrefix(ctx, o.g.org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w", name, err)
	}
	var wanted []gh.Repo
	for _, r := range filterAssignmentRepos(o.g.cfg, name, repos) {
		if !r.IsTemplate {
			wanted = append(wanted, r)
		}
	}
	if len(wanted) == 0 {
		return fmt.Errorf("no student repositories named %s-* found in %s; check the assignment name and your config's org", name, o.g.org)
	}

	results := runConcurrent(ctx, o.g.concurrency, wanted, func(ctx context.Context, r gh.Repo) repoActivity {
		return o.read(ctx, client, name, r, since, until)
	})
	sort.Slice(results, func(i, j int) bool { return results[i].repo < results[j].repo })

	var failed int
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}

	fmt.Fprintf(out, "Activity for %s-* in %s (%s)\n", name, o.g.org, describeWindow(since, until))
	switch {
	case o.pin:
		err = o.reportPin(ctx, out, client, name, results)
	default:
		err = nil
	}
	if o.all {
		o.reportAll(out, results)
	}
	if o.forced {
		o.reportEvents(out, "Force pushes", results, func(a gh.Activity) bool {
			return a.ActivityType == gh.ActivityForcePush
		})
	}
	if o.deleted {
		o.reportEvents(out, "Branch deletions", results, func(a gh.Activity) bool {
			return a.ActivityType == gh.ActivityBranchDeletion
		})
	}
	if !o.pin && !o.all && !o.forced && !o.deleted {
		reportActivitySummary(out, results)
	}
	// A branch named explicitly that exists nowhere prints a wall of zeroes that
	// looks like a broken report rather than a typo. Say which branch found
	// nothing, since that is nearly always the reason.
	if o.branch != "" && totalEvents(results) == 0 && failed == 0 {
		fmt.Fprintf(out, "\nno activity on branch %q in any %s-* repo; check the branch name\n", o.branch, name)
	}
	if err != nil {
		return err
	}

	if failed > 0 {
		fmt.Fprintln(out)
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
			}
		}
		return fmt.Errorf("%d repo(s) could not be read", failed)
	}
	return nil
}

// window resolves --since/--until, defaulting until to now. A zero since means
// no lower bound.
func (o *activityOpts) window() (since, until time.Time, err error) {
	if o.since != "" {
		if since, err = time.Parse(time.RFC3339, o.since); err != nil {
			return since, until, fmt.Errorf("--since %q is not an RFC3339 time (e.g. 2026-03-01T23:59:59-06:00): %w", o.since, err)
		}
	}
	until = o.now()
	if o.until != "" {
		if until, err = time.Parse(time.RFC3339, o.until); err != nil {
			return since, until, fmt.Errorf("--until %q is not an RFC3339 time (e.g. 2026-03-01T23:59:59-06:00): %w", o.until, err)
		}
	}
	if !since.IsZero() && until.Before(since) {
		return since, until, fmt.Errorf("--until %s is before --since %s", until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	return since, until, nil
}

func describeWindow(since, until time.Time) string {
	if since.IsZero() {
		return "through " + until.Format(time.RFC3339)
	}
	return since.Format(time.RFC3339) + " through " + until.Format(time.RFC3339)
}

// read fetches one repository's activity and narrows it to the reported branch
// and window.
func (o *activityOpts) read(ctx context.Context, client activityClient, name string, r gh.Repo, since, until time.Time) repoActivity {
	branch := o.branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	res := repoActivity{repo: r.Name, key: strings.TrimPrefix(r.Name, name+"-"), branch: branch}
	if branch == "" {
		res.err = fmt.Errorf("no default branch reported; pass -b to name one")
		return res
	}

	all, err := client.ListRepoActivity(ctx, o.g.org, r.Name, "refs/heads/"+branch)
	if err != nil {
		res.err = fmt.Errorf("reading activity: %w", err)
		return res
	}
	// Filter by branch again rather than trusting the ref parameter: an ignored
	// filter would silently mix other branches into the answer, and for -p that
	// would pin a commit from the wrong branch.
	for _, a := range all {
		if a.Branch() != branch {
			continue
		}
		if !since.IsZero() && a.Timestamp.Before(since) {
			continue
		}
		if a.Timestamp.After(until) {
			res.postWindow = append(res.postWindow, a)
			continue
		}
		res.events = append(res.events, a)
	}
	sort.Slice(res.events, func(i, j int) bool { return res.events[i].Timestamp.After(res.events[j].Timestamp) })

	// The tip is read for the freshness check below, and only matters to -p.
	if o.pin {
		tip, err := client.GetRef(ctx, o.g.org, r.Name, "heads/"+branch)
		if err != nil {
			res.err = fmt.Errorf("reading %s tip: %w", branch, err)
			return res
		}
		res.tip = tip
		// Freshness: GitHub documents no ingestion latency for this record, so
		// rather than assume it is current, require the branch's present tip to
		// appear in it. If the tip is missing the record is behind, and a pin taken
		// from it could name a commit that has since been superseded.
		//
		// An empty tip means the branch has no commits, so there is nothing for the
		// record to be behind on; the repo simply has nothing to pin and is
		// reported as such below.
		if tip != "" && !containsTip(all, tip) {
			res.err = fmt.Errorf("GitHub's activity record does not yet contain the current %s tip (%s), so it is behind; retry shortly", branch, short(tip))
		}
	}
	return res
}

func containsTip(all []gh.Activity, tip string) bool {
	for _, a := range all {
		if a.After == tip {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// reportPin resolves each repo's commit as of --until, verifies every one is
// still retrievable, and writes the pin file.
func (o *activityOpts) reportPin(ctx context.Context, out io.Writer, client activityClient, name string, results []repoActivity) error {
	type pinned struct{ key, repo, sha string }
	var pins []pinned
	var noActivity, orphaned []string

	for _, r := range results {
		if r.err != nil {
			continue
		}
		var sha string
		for _, a := range r.events { // newest first
			if a.SetsTip() {
				sha = a.After
				break
			}
		}
		if sha == "" {
			noActivity = append(noActivity, r.key)
			continue
		}
		// A pinned commit that has been orphaned by a force push and collected is
		// gone: writing it would produce a pin file that cannot be collected.
		ok, err := client.CommitExists(ctx, o.g.org, r.repo, sha)
		if err != nil {
			return fmt.Errorf("verifying %s on %s: %w", short(sha), r.repo, err)
		}
		if !ok {
			orphaned = append(orphaned, fmt.Sprintf("%s (%s)", r.key, short(sha)))
			continue
		}
		pins = append(pins, pinned{r.key, r.repo, sha})
	}

	fmt.Fprintf(out, "\npinned %s\n", plural(len(pins), "repo"))
	// Print the mapping itself: -o is a redirect, not the only way to see the
	// result, and a run that says "pinned 49 repos" while showing none of them
	// has produced nothing the caller can act on. Same form as the file, so a
	// line can be copied straight into a hand-edited pin file.
	for _, p := range pins {
		fmt.Fprintf(out, "  %s: %s\n", p.key, p.sha)
	}
	if len(noActivity) > 0 {
		sort.Strings(noActivity)
		fmt.Fprintf(out, "no activity in the window, so not pinned (%d): %s\n", len(noActivity), strings.Join(noActivity, ", "))
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		fmt.Fprintf(out, "NOT pinned, the commit is no longer retrievable (%d): %s\n", len(orphaned), strings.Join(orphaned, ", "))
	}

	// Force pushes are reported whether or not -f was given, split by side of the
	// pinned instant because they mean different things. Before it: history was
	// rewritten, but the tip at that instant is still the tip at that instant, so
	// the pin holds. After it: the pinned commit may be gone, which is what
	// CommitExists above tests for; this names the likely cause.
	inWindow := keysWithForcePush(results, func(r repoActivity) []gh.Activity { return r.events })
	afterPin := keysWithForcePush(results, func(r repoActivity) []gh.Activity { return r.postWindow })
	if len(inWindow) > 0 {
		fmt.Fprintf(out, "force-pushed within the window, so history was rewritten before the pin (%d): %s\n",
			len(inWindow), strings.Join(inWindow, ", "))
	}
	if len(afterPin) > 0 {
		fmt.Fprintf(out, "force-pushed AFTER the pinned instant, so a pinned commit may have been orphaned (%d): %s\n",
			len(afterPin), strings.Join(afterPin, ", "))
	}

	if o.out != "" {
		if len(pins) == 0 {
			return fmt.Errorf("nothing to pin, so %s was not written", o.out)
		}
		f, err := createNew(o.out)
		if err != nil {
			return err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# gh cls activity %s -p, commits as of %s\n", name, o.until)
		for _, p := range pins {
			fmt.Fprintf(&b, "%s: %s\n", p.key, p.sha)
		}
		if _, err := io.WriteString(f, b.String()); err != nil {
			f.Close()
			os.Remove(o.out)
			return fmt.Errorf("writing %s: %w", o.out, err)
		}
		if err := f.Close(); err != nil {
			os.Remove(o.out)
			return fmt.Errorf("closing %s: %w", o.out, err)
		}
		fmt.Fprintf(out, "wrote %s (%s)\n", o.out, plural(len(pins), "entry"))
	}
	if len(orphaned) > 0 {
		return fmt.Errorf("%d repo(s) could not be pinned because their commit is gone; re-run with -f to see the force pushes that removed it", len(orphaned))
	}
	return nil
}

// keysWithForcePush returns the sorted keys of repos with a force push among the
// activities pick selects.
func keysWithForcePush(results []repoActivity, pick func(repoActivity) []gh.Activity) []string {
	var keys []string
	for _, r := range results {
		for _, a := range pick(r) {
			if a.ActivityType == gh.ActivityForcePush {
				keys = append(keys, r.key)
				break
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// reportEvents prints (and optionally writes) the activities matching keep.
func (o *activityOpts) reportEvents(out io.Writer, title string, results []repoActivity, keep func(gh.Activity) bool) {
	rows := collectEvents(results, keep)

	// Report coverage, not just a count: a bare "0" cannot be told apart from
	// having looked at nothing. Unreadable repos are excluded from the
	// denominator and listed separately, so the number is honest.
	readable, withAny := 0, map[string]bool{}
	for _, r := range results {
		if r.err == nil {
			readable++
		}
	}
	for _, r := range rows {
		withAny[r.repo] = true
	}
	fmt.Fprintf(out, "\n%s: %d in %d of %s examined\n", title, len(rows), len(withAny), plural(readable, "repo"))
	if len(rows) > 0 {
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  WHEN\tKEY\tBRANCH\tWHAT\tWHO\tBEFORE\tAFTER")
		for _, r := range rows {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.a.Timestamp.Format(time.RFC3339), r.key, r.a.Branch(), r.a.ActivityType,
				orNone(r.a.Actor.Login), short(r.a.Before), short(r.a.After))
		}
		tw.Flush()
	}

	o.writeEventsCSV(out, rows)
}

// eventRow is one activity with the repo it belongs to.
type eventRow struct {
	key, repo string
	a         gh.Activity
}

// collectEvents gathers the activities matching keep across every repo, oldest
// first.
func collectEvents(results []repoActivity, keep func(gh.Activity) bool) []eventRow {
	var rows []eventRow
	for _, r := range results {
		for _, a := range r.events {
			if keep(a) {
				rows = append(rows, eventRow{r.key, r.repo, a})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].a.Timestamp.Before(rows[j].a.Timestamp) })
	return rows
}

// writeEventsCSV writes every individual change to --out. The terminal gets a
// summary; the file gets the full record, which is what makes it worth keeping.
func (o *activityOpts) writeEventsCSV(out io.Writer, rows []eventRow) {
	if o.out == "" {
		return
	}
	f, err := createNew(o.out)
	if err != nil {
		fmt.Fprintf(out, "FAILED writing %s: %v\n", o.out, err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"key", "repo", "branch", "timestamp", "activity_type", "actor", "before", "after"})
	for _, r := range rows {
		_ = w.Write([]string{r.key, r.repo, r.a.Branch(), r.a.Timestamp.Format(time.RFC3339),
			r.a.ActivityType, r.a.Actor.Login, r.a.Before, r.a.After})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintf(out, "FAILED writing %s: %v\n", o.out, err)
		return
	}
	fmt.Fprintf(out, "wrote %s (%s)\n", o.out, plural(len(rows), "row"))
}

// reportAll summarizes every recorded change by who made it. Listing each change
// individually would be thousands of lines across a class; the per-actor view is
// what answers "who has been pushing to this repo", and --out still receives
// every individual change.
//
// These are push counts, not a measure of contribution: one push can carry a
// term's work and twenty can carry none.
func (o *activityOpts) reportAll(out io.Writer, results []repoActivity) {
	rows := collectEvents(results, func(gh.Activity) bool { return true })

	// Tally per actor per type. Only the types that actually occur become
	// columns: a course repo rarely sees a merge queue, and six columns of zeroes
	// is the same noise as a row of zeroes.
	type tally struct {
		key, who string
		total    int
		byType   map[string]int
	}
	index := map[string]*tally{}
	var order []*tally
	present := map[string]bool{}
	for _, r := range rows {
		who := orNone(r.a.Actor.Login)
		id := r.key + "\x00" + who
		t, ok := index[id]
		if !ok {
			t = &tally{key: r.key, who: who, byType: map[string]int{}}
			index[id] = t
			order = append(order, t)
		}
		t.total++
		t.byType[r.a.ActivityType]++
		present[r.a.ActivityType] = true
	}
	// Grouped by repo, busiest actor first within each.
	sort.Slice(order, func(i, j int) bool {
		if order[i].key != order[j].key {
			return order[i].key < order[j].key
		}
		return order[i].total > order[j].total
	})

	readable, repos := 0, map[string]bool{}
	for _, r := range results {
		if r.err == nil {
			readable++
		}
	}
	actors := map[string]bool{}
	for _, r := range rows {
		repos[r.repo] = true
		actors[orNone(r.a.Actor.Login)] = true
	}
	fmt.Fprintf(out, "\nAll activity: %d by %s in %d of %s examined\n",
		len(rows), plural(len(actors), "actor"), len(repos), plural(readable, "repo"))

	if len(order) > 0 {
		types := presentTypes(present)
		header := []string{"  KEY", "WHO", "TOTAL"}
		for _, ty := range types {
			header = append(header, typeLabel(ty))
		}
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(header, "\t"))
		for _, t := range order {
			cells := []string{"  " + t.key, t.who, fmt.Sprint(t.total)}
			for _, ty := range types {
				cells = append(cells, fmt.Sprint(t.byType[ty]))
			}
			fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
		tw.Flush()
	}
	o.writeEventsCSV(out, rows)
}

// reportActivitySummary is the default view: what happened, per repo.
// totalEvents counts the activities across every repo that could be read.
func totalEvents(results []repoActivity) int {
	n := 0
	for _, r := range results {
		n += len(r.events)
	}
	return n
}

// reportActivitySummary prints one row per repo that has something to report.
// Repos with nothing are counted rather than listed: a row of zeroes for every
// student buries the ones that matter, which is the whole point of the report.
func reportActivitySummary(out io.Writer, results []repoActivity) {
	type row struct {
		key, branch, last       string
		pushes, forced, deleted int
		unreadable              bool
	}
	var rows []row
	quiet := 0
	for _, r := range results {
		if r.err != nil {
			rows = append(rows, row{key: r.key, branch: r.branch, unreadable: true})
			continue
		}
		if len(r.events) == 0 {
			quiet++
			continue
		}
		e := row{key: r.key, branch: r.branch}
		for _, a := range r.events {
			switch a.ActivityType {
			case gh.ActivityForcePush:
				e.forced++
			case gh.ActivityBranchDeletion:
				e.deleted++
			default:
				e.pushes++
			}
			if e.last == "" {
				e.last = a.Timestamp.Format(time.RFC3339)
			}
		}
		rows = append(rows, e)
	}

	if len(rows) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  KEY\tBRANCH\tPUSHES\tFORCE\tDELETIONS\tLAST")
		for _, e := range rows {
			if e.unreadable {
				fmt.Fprintf(tw, "  %s\t%s\t?\t?\t?\t(unreadable)\n", e.key, e.branch)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%s\n", e.key, e.branch, e.pushes, e.forced, e.deleted, orNone(e.last))
		}
		tw.Flush()
	}
	if quiet > 0 {
		fmt.Fprintf(out, "\n%s with no activity, not listed\n", plural(quiet, "repo"))
	}
}

// orNone renders an empty field. audit's dash() is not reusable here: it says
// "(not in roster)", which is meaningless for a timestamp or an actor.
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// typeOrder is the order activity types appear as columns: the ones an
// assignment repo actually sees, most common first.
var typeOrder = []string{
	gh.ActivityPush, gh.ActivityForcePush, gh.ActivityPRMerge,
	gh.ActivityMergeQueueMerge, gh.ActivityBranchCreation, gh.ActivityBranchDeletion,
}

// typeLabels are compact column headings. A type GitHub adds later has no entry
// and falls back to its raw name, so a new kind of change is still counted
// rather than silently dropped.
var typeLabels = map[string]string{
	gh.ActivityPush:            "PUSH",
	gh.ActivityForcePush:       "FORCE",
	gh.ActivityPRMerge:         "MERGE",
	gh.ActivityMergeQueueMerge: "MQMERGE",
	gh.ActivityBranchCreation:  "CREATE",
	gh.ActivityBranchDeletion:  "DELETE",
}

func typeLabel(t string) string {
	if l, ok := typeLabels[t]; ok {
		return l
	}
	return strings.ToUpper(t)
}

// presentTypes returns the types that occurred, in canonical order, with any
// unrecognized ones appended alphabetically.
func presentTypes(present map[string]bool) []string {
	var out []string
	for _, t := range typeOrder {
		if present[t] {
			out = append(out, t)
		}
	}
	var extra []string
	for t := range present {
		if _, known := typeLabels[t]; !known {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// createNew opens a file that must not already exist, so a report never
// silently replaces a previous one.
func createNew(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("refusing to overwrite %s; remove it or choose a different --out", path)
		}
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return f, nil
}
