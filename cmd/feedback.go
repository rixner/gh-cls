package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/rixner/gh-cls/unit"
	"github.com/spf13/cobra"
)

// feedbackMarkerPrefix tags a comment this tool posted. The full marker embeds a
// hash of the file's contents, so a re-run skips a comment whose exact text is
// already present (idempotent) while edited feedback — carrying a new hash —
// posts a fresh comment. It is an HTML comment, hidden in GitHub's rendered view.
const feedbackMarkerPrefix = "<!-- gh-cls-feedback v1 sha256:"

// Statuses a single repo's feedback post can end in (besides a failure).
const (
	feedbackPosted   = "posted"
	feedbackUpToDate = "up-to-date"
)

// feedbackClient is the narrow set of GitHub operations feedback needs. Unlike
// the mutating commands it only reads and comments, but it still requires org
// ownership (see run): commenting on every student's repo is an instructor-wide
// action, not a per-repo one.
type feedbackClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error)
	FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error)
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.Comment, error)
	AddComment(ctx context.Context, owner, repo string, number int, body string) (string, error)
}

// feedbackOpts carries the resolved flags and dependencies for `gh cls feedback`.
type feedbackOpts struct {
	g         *globalOpts
	dir       string
	roster    string
	teams     string
	force     bool
	dryRun    bool
	newClient func(context.Context) (feedbackClient, error)
}

func newFeedbackCmd(g *globalOpts) *cobra.Command {
	o := &feedbackOpts{
		g:         g,
		newClient: func(context.Context) (feedbackClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "feedback <name>",
		Short: "Post graded feedback files as comments on each repo's feedback issue or PR",
		Long: `Post one feedback file per student (or team) as a comment on that repo's
feedback issue or pull request — the artifact assign created, named by the
assignment's feedback policy. Each file in --dir is named <key>.md or <key>.txt,
where <key> is the GitHub username (individual) or team name (group), matching
the <name>-<key> repository.

The directory must hold exactly one file per student/team. A missing file
(forgotten feedback) or a file matching no student (a typo) is reported by name
and aborts, unless --force posts the matching subset and skips the rest. Posting
is idempotent: a re-run only posts feedback not already present, so a partial
run or a --force subset can be completed by re-running. Editing a file posts a
new comment; existing comments are never changed.`,
		Example: `  gh cls feedback hw1 --dir ./hw1-feedback --roster roster.csv
  gh cls feedback project --dir ./proj-feedback --roster roster.csv --teams teams.yml --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.dir, "dir", "d", "", "directory of feedback files, one per student/team named <key>.md or <key>.txt (required)")
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.teams, "teams", "T", "", "path to the teams file (required for group, rejected for individual)")
	f.BoolVarP(&o.force, "force", "F", false, "post the matching subset even when the directory is not exactly one file per student/team")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would be posted without doing it")
	_ = cmd.MarkFlagRequired("dir")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// feedbackFile is one feedback file read from the directory: key is the basename
// without extension (the original spelling, for display), name is the full
// filename, and body is the contents to post.
type feedbackFile struct {
	key  string
	name string
	body string
}

// matchedUnit pairs a unit with the feedback file that will be posted to it.
type matchedUnit struct {
	unit unit.Unit
	file feedbackFile
}

// feedbackResult records the outcome of posting (or skipping) one repo.
type feedbackResult struct {
	repo   string
	status string // feedbackPosted or feedbackUpToDate
	url    string
	err    error
}

func (o *feedbackOpts) run(ctx context.Context, out io.Writer, name string) error {
	org := o.g.org
	policy, err := o.g.cfg.Resolve(name, config.Overrides{})
	if err != nil {
		return err
	}
	if policy.Feedback == config.FeedbackNone {
		return fmt.Errorf("assignment %q has no feedback artifact: set assignments.%s.feedback to pr or issue and run `gh cls assign %s` to create it before posting feedback", name, name, name)
	}

	// Type/inputs consistency (mirrors assign): a group assignment needs the
	// teams file to know its units; an individual one must not get one.
	switch policy.Type {
	case config.TypeGroup:
		if o.teams == "" {
			return fmt.Errorf("assignment %q is a group assignment: --teams is required", name)
		}
	case config.TypeIndividual:
		if o.teams != "" {
			return fmt.Errorf("assignment %q is an individual assignment: --teams is not allowed", name)
		}
	}

	r, err := roster.ParseFile(o.roster)
	if err != nil {
		return err
	}
	var tm *teams.Teams
	if policy.Type == config.TypeGroup {
		if tm, err = teams.ParseFile(o.teams); err != nil {
			return err
		}
	}
	units, report, err := unit.Resolve(policy.Type, r, tm)
	if err != nil {
		return err
	}
	for _, id := range report.UnassignedIDs {
		fmt.Fprintf(out, "warning: enrolled student %s is on no team\n", id)
	}

	// Read every file before any API call, so a malformed directory aborts before
	// a single comment is posted.
	files, ignored, err := readFeedbackDir(o.dir)
	if err != nil {
		return err
	}
	matched, missing, unmatched := matchFiles(units, files)

	// The coverage report is printed before anything is posted — the "is there
	// feedback for everyone?" message — naming every gap explicitly.
	printCoverage(out, name, matched, missing, unmatched, ignored)

	if len(missing) > 0 || len(unmatched) > 0 {
		if !o.force {
			return fmt.Errorf("feedback directory is not exactly one file per student/team: %d missing, %d unmatched (see above); fix it, or pass -F/--force to post the %d matching file(s) and skip the rest", len(missing), len(unmatched), len(matched))
		}
		fmt.Fprintf(out, "\n--force: posting the %d matching file(s); the %d missing and %d unmatched above are skipped\n", len(matched), len(missing), len(unmatched))
	}
	if len(matched) == 0 {
		return fmt.Errorf("no feedback file matches a student/team in %s; nothing to post", o.dir)
	}

	if o.dryRun {
		return reportFeedbackDryRun(out, org, name, policy.Feedback, matched)
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	results := runConcurrent(ctx, o.g.concurrency, matched, func(ctx context.Context, m matchedUnit) feedbackResult {
		return o.post(ctx, client, org, name, policy.Feedback, m)
	})
	return reportFeedback(out, results, missing, unmatched)
}

// readFeedbackDir collects the .md/.txt files in dir, keyed by the lower-cased
// basename (GitHub keys are case-insensitive). It rejects an unreadable or
// whitespace-only file and two files that map to the same key, and returns the
// names of files it ignored (other extensions) so the caller can report them.
// Subdirectories are skipped.
func readFeedbackDir(dir string) (map[string]feedbackFile, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading feedback directory %s: %w", dir, err)
	}
	files := make(map[string]feedbackFile)
	var ignored []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".md" && ext != ".txt" {
			ignored = append(ignored, name)
			continue
		}
		key := strings.TrimSuffix(name, filepath.Ext(name))
		lkey := strings.ToLower(key)
		if prev, ok := files[lkey]; ok {
			return nil, nil, fmt.Errorf("two feedback files map to the same student/team %q: %s and %s; remove one", key, prev.name, name)
		}
		path := filepath.Join(dir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading feedback file %s: %w", path, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return nil, nil, fmt.Errorf("feedback file %s is empty; a blank comment is not worth posting", path)
		}
		files[lkey] = feedbackFile{key: key, name: name, body: string(content)}
	}
	return files, ignored, nil
}

// matchFiles pairs each unit with its feedback file by lower-cased key. It
// returns the matches in unit order, the keys of units with no file, and the
// names of files matching no unit.
func matchFiles(units []unit.Unit, files map[string]feedbackFile) (matched []matchedUnit, missing, unmatched []string) {
	used := make(map[string]bool, len(files))
	for _, u := range units {
		lkey := strings.ToLower(u.Key)
		if f, ok := files[lkey]; ok {
			used[lkey] = true
			matched = append(matched, matchedUnit{unit: u, file: f})
		} else {
			missing = append(missing, u.Key)
		}
	}
	for lkey, f := range files {
		if !used[lkey] {
			unmatched = append(unmatched, f.name)
		}
	}
	sort.Strings(unmatched)
	return matched, missing, unmatched
}

// printCoverage prints the match/missing/unmatched breakdown, naming every gap.
func printCoverage(out io.Writer, name string, matched []matchedUnit, missing, unmatched, ignored []string) {
	fmt.Fprintf(out, "Feedback for %s: %d matched, %d missing, %d unmatched\n", name, len(matched), len(missing), len(unmatched))
	if len(missing) > 0 {
		fmt.Fprintf(out, "\nmissing feedback (no file) for %d student/team(s):\n  %s\n", len(missing), strings.Join(missing, "\n  "))
	}
	if len(unmatched) > 0 {
		fmt.Fprintf(out, "\nunmatched file(s) — no student/team has that name (typo?):\n  %s\n", strings.Join(unmatched, "\n  "))
	}
	if len(ignored) > 0 {
		fmt.Fprintf(out, "\nignored %d non-.md/.txt file(s): %s\n", len(ignored), strings.Join(ignored, ", "))
	}
}

// post comments one feedback file onto a repo's feedback artifact, skipping the
// post when an identical comment (same content marker) is already there.
func (o *feedbackOpts) post(ctx context.Context, client feedbackClient, org, name, mode string, m matchedUnit) feedbackResult {
	repo := name + "-" + m.unit.Key
	res := feedbackResult{repo: repo}

	if _, exists, err := client.GetRepo(ctx, org, repo); err != nil {
		res.err = fmt.Errorf("checking %s: %w", repo, err)
		return res
	} else if !exists {
		res.err = fmt.Errorf("repository %s not found; run `gh cls assign %s` to create it before posting feedback", repo, name)
		return res
	}

	number, found, err := findArtifact(ctx, client, org, repo, mode)
	if err != nil {
		res.err = fmt.Errorf("locating feedback %s in %s: %w", artifactNoun(mode), repo, err)
		return res
	}
	if !found {
		res.err = fmt.Errorf("no feedback %s in %s; run `gh cls assign %s` so the feedback %s exists before posting", artifactNoun(mode), repo, name, artifactNoun(mode))
		return res
	}

	marker := feedbackMarker(m.file.body)
	comments, err := client.ListIssueComments(ctx, org, repo, number)
	if err != nil {
		res.err = fmt.Errorf("reading comments on %s: %w", repo, err)
		return res
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			res.status = feedbackUpToDate
			return res
		}
	}

	url, err := client.AddComment(ctx, org, repo, number, marker+"\n\n"+m.file.body)
	if err != nil {
		res.err = fmt.Errorf("posting feedback to %s: %w", repo, err)
		return res
	}
	res.status = feedbackPosted
	res.url = url
	return res
}

// findArtifact returns the number of the repo's feedback artifact for the mode.
// The artifact state is unused here (posting a comment works on any state).
func findArtifact(ctx context.Context, client feedbackClient, org, repo, mode string) (int, bool, error) {
	switch mode {
	case feedbackIssue:
		n, _, found, err := client.FindIssueByTitle(ctx, org, repo, feedbackTitle)
		return n, found, err
	case feedbackPR:
		n, _, found, err := client.FindPRByBase(ctx, org, repo, feedbackBranch)
		return n, found, err
	default:
		return 0, false, fmt.Errorf("unknown feedback mode %q", mode)
	}
}

// feedbackMarker is the per-content idempotency marker embedded in each comment.
func feedbackMarker(body string) string {
	sum := sha256.Sum256([]byte(body))
	return feedbackMarkerPrefix + hex.EncodeToString(sum[:]) + " -->"
}

// artifactNoun names the feedback artifact for the mode, for messages.
func artifactNoun(mode string) string {
	if mode == feedbackPR {
		return "pull request"
	}
	return "issue"
}

// reportFeedbackDryRun lists what a real run would post, making no API calls.
func reportFeedbackDryRun(out io.Writer, org, name, mode string, matched []matchedUnit) error {
	fmt.Fprintf(out, "\nDRY RUN — no comments will be posted\n")
	fmt.Fprintf(out, "Would comment on the feedback %s of %d repo(s) in %s:\n", artifactNoun(mode), len(matched), org)
	for _, m := range matched {
		fmt.Fprintf(out, "  %s-%s  <-  %s\n", name, m.unit.Key, m.file.name)
	}
	return nil
}

// reportFeedback prints per-repo outcomes and a summary, and returns an error if
// any post failed. Posted and failed repos are listed individually; up-to-date
// repos (the common case on a re-run) are summarized, not enumerated.
func reportFeedback(out io.Writer, results []feedbackResult, missing, unmatched []string) error {
	var posted, upToDate, failed int
	fmt.Fprintln(out)
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
			fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
		case r.status == feedbackUpToDate:
			upToDate++
		default:
			posted++
			fmt.Fprintf(out, "  posted %s -> %s\n", r.repo, r.url)
		}
	}
	fmt.Fprintf(out, "\n%d posted, %d up-to-date, %d failed\n", posted, upToDate, failed)
	if len(missing) > 0 || len(unmatched) > 0 {
		fmt.Fprintf(out, "note: skipped %d student/team(s) with no file and %d unmatched file(s) (see above)\n", len(missing), len(unmatched))
	}
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed", failed)
	}
	return nil
}
