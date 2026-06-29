package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh2 "github.com/cli/go-gh/v2"
	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// collectTagPrefix namespaces collect's tags so they never collide with a
// student's own tags.
const collectTagPrefix = "gh-cls/collect/"

// collectClient is the narrow GitHub surface collect reads: just the repo list.
// Cloning and fetching go through git/gh, not the REST client.
type collectClient interface {
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
}

// gitRunner is the seam over the git and `gh repo clone` operations collect
// performs, so tests can fake them without touching disk or the network.
type gitRunner interface {
	CloneExists(dir string) bool
	Clone(ctx context.Context, org, repo, dir string) error
	WorktreeClean(ctx context.Context, dir string) (bool, error)
	Head(ctx context.Context, dir string) (string, error)
	TagExists(ctx context.Context, dir, tag string) (bool, error)
	// Fetch shallow-fetches ref (a branch name or a SHA) from origin, reporting
	// whether the update rewrote history (a forced, non-fast-forward update).
	Fetch(ctx context.Context, dir, ref string) (forced bool, err error)
	Checkout(ctx context.Context, dir, ref string) error
	CreateTag(ctx context.Context, dir, tag, sha string) error
}

// collectOpts carries the resolved flags and dependencies for `gh cls collect`.
type collectOpts struct {
	g         *globalOpts
	roster    string
	teams     string
	commits   string
	out       string
	label     string
	dryRun    bool
	now       func() time.Time
	newClient func(context.Context) (collectClient, error)
	git       gitRunner
}

func newCollectCmd(g *globalOpts) *cobra.Command {
	o := &collectOpts{
		g:         g,
		now:       time.Now,
		newClient: func(context.Context) (collectClient, error) { return gh.New() },
		git:       execGit{},
	}
	cmd := &cobra.Command{
		Use:   "collect <name>",
		Short: "Clone each student's repository locally for grading",
		Long: `Maintain one shallow clone per student (or team) under --out, taking each repo
to a target commit and tagging it so every collection is preserved. The default
target is the repo's default-branch tip; --commits pins exact SHAs. Re-running
the same --label tops up only repos not yet collected under it; a new label
updates the clones to the new target and tags the new state, leaving prior tags
in place so no collected state is ever lost.

Roster-aware: it collects every <name>-* repo and reports any that are missing
(a student with no repo) or unexpected (a repo matching no roster/teams entry).
A clone with local changes is left untouched, so grading-script edits survive.

This is the one command that uses git: clones go through gh, updates through git.
See COLLECT.md for the model and the git you need.`,
		Example: `  gh cls collect hw1 --roster roster.csv --out ./hw1
  gh cls collect hw1 --roster roster.csv --out ./hw1-final --commits deadline.yml --label final
  gh cls collect project --teams teams.yml --out ./project`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.out, "out", "o", "", "destination directory; one clone per repo at <out>/<key> (required)")
	f.StringVarP(&o.roster, "roster", "r", "", "roster CSV (required for an individual assignment)")
	f.StringVarP(&o.teams, "teams", "T", "", "teams file (required for a group assignment)")
	f.StringVar(&o.commits, "commits", "", "YAML of key->commit SHA; collect exactly those commits")
	f.StringVar(&o.label, "label", "", "name for this collection's tag (default: a timestamp)")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "resolve and reconcile without cloning anything")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// repoItem is one student repository to collect.
type repoItem struct {
	key           string
	lkey          string
	repo          string
	defaultBranch string
	unexpected    bool
}

// collectResult records the outcome of collecting one repository.
type collectResult struct {
	key    string
	repo   string
	sha    string
	ref    string // the default branch, or "(pinned)" in pinned mode
	status string
	forced bool
	err    error
}

const (
	collectStatusCollected = "collected"
	collectStatusUpdated   = "updated"
	collectStatusUpToDate  = "up-to-date"
	collectStatusDirty     = "skipped (local changes)"
	collectStatusNoSHA     = "skipped (no pinned SHA)"
)

func (o *collectOpts) run(ctx context.Context, out io.Writer, name string) error {
	policy, err := o.g.cfg.Resolve(name, config.Overrides{})
	if err != nil {
		return err
	}

	expected, err := o.expectedKeys(policy.Type, name)
	if err != nil {
		return err
	}

	var commits map[string]string
	if o.commits != "" {
		if commits, err = parseCommits(o.commits); err != nil {
			return err
		}
	}

	label := o.label
	if label == "" {
		label = o.now().Format("20060102-150405")
	}
	tag := collectTagPrefix + label

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	all, err := client.ListOrgReposByPrefix(ctx, o.g.org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w", name, err)
	}

	var items []repoItem
	present := make(map[string]bool)
	for _, r := range all {
		if r.IsTemplate {
			continue
		}
		key := strings.TrimPrefix(r.Name, name+"-")
		lkey := strings.ToLower(key)
		present[lkey] = true
		_, ok := expected[lkey]
		items = append(items, repoItem{key: key, lkey: lkey, repo: r.Name, defaultBranch: r.DefaultBranch, unexpected: !ok})
	}

	var missing []string
	for lk, disp := range expected {
		if !present[lk] {
			missing = append(missing, disp)
		}
	}
	sort.Strings(missing)

	fmt.Fprintf(out, "Collecting %s into %s (tag %s)\n", name, o.out, tag)
	reportReconcile(out, items, missing)

	if o.dryRun {
		fmt.Fprintf(out, "\nDRY RUN — nothing cloned\n")
		for _, it := range items {
			fmt.Fprintf(out, "  would collect %s -> %s/%s\n", it.repo, o.out, it.key)
		}
		return nil
	}

	if len(items) == 0 {
		fmt.Fprintf(out, "\nno repositories to collect\n")
		return nil
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", o.out, err)
	}

	results := runConcurrent(ctx, o.g.concurrency, items, func(ctx context.Context, it repoItem) collectResult {
		return o.collectOne(ctx, o.g.org, name, tag, commits, it)
	})

	if err := o.writeManifest(label, results); err != nil {
		return err
	}
	return reportCollect(out, results, missing)
}

// expectedKeys returns the lower-cased->display key set the assignment's type
// defines (usernames from the roster for individual, team names from the teams
// file for group), validating that the right file was given.
func (o *collectOpts) expectedKeys(typ config.AssignmentType, name string) (map[string]string, error) {
	switch typ {
	case config.TypeIndividual:
		if o.roster == "" {
			return nil, fmt.Errorf("assignment %q is individual: --roster is required", name)
		}
		if o.teams != "" {
			return nil, fmt.Errorf("assignment %q is individual: --teams is not allowed", name)
		}
		r, err := roster.ParseFile(o.roster)
		if err != nil {
			return nil, err
		}
		return r.UsersByLowercase(), nil
	case config.TypeGroup:
		if o.teams == "" {
			return nil, fmt.Errorf("assignment %q is a group assignment: --teams is required", name)
		}
		if o.roster != "" {
			return nil, fmt.Errorf("assignment %q is a group assignment: --roster is not allowed (team names are the keys)", name)
		}
		tm, err := teams.ParseFile(o.teams)
		if err != nil {
			return nil, err
		}
		keys := make(map[string]string, tm.Len())
		for _, n := range tm.Names() {
			keys[strings.ToLower(n)] = n
		}
		return keys, nil
	default:
		return nil, fmt.Errorf("assignment %q has an unknown type %q", name, typ)
	}
}

// collectOne clones or updates one repository to its target commit and tags it.
func (o *collectOpts) collectOne(ctx context.Context, orgName, name, tag string, commits map[string]string, it repoItem) collectResult {
	res := collectResult{key: it.key, repo: it.repo, ref: it.defaultBranch}
	dir := filepath.Join(o.out, it.key)

	pinned := commits != nil
	var sha string
	if pinned {
		res.ref = "(pinned)"
		s, ok := commits[it.lkey]
		if !ok {
			res.status = collectStatusNoSHA
			return res
		}
		sha = s
	}

	if !o.git.CloneExists(dir) {
		if err := o.git.Clone(ctx, orgName, it.repo, dir); err != nil {
			res.err = fmt.Errorf("cloning %s: %w", it.repo, err)
			return res
		}
		if pinned {
			if _, err := o.git.Fetch(ctx, dir, sha); err != nil {
				res.err = fmt.Errorf("fetching %s in %s: %w", sha, it.repo, err)
				return res
			}
			if err := o.git.Checkout(ctx, dir, sha); err != nil {
				res.err = fmt.Errorf("checking out %s in %s: %w", sha, it.repo, err)
				return res
			}
		}
		return o.tagHead(ctx, dir, tag, collectStatusCollected, res)
	}

	// Existing clone.
	if has, err := o.git.TagExists(ctx, dir, tag); err != nil {
		res.err = fmt.Errorf("checking tag on %s: %w", it.repo, err)
		return res
	} else if has {
		res.status = collectStatusUpToDate
		res.sha, _ = o.git.Head(ctx, dir)
		return res
	}
	if clean, err := o.git.WorktreeClean(ctx, dir); err != nil {
		res.err = fmt.Errorf("checking %s for local changes: %w", it.repo, err)
		return res
	} else if !clean {
		res.status = collectStatusDirty
		return res
	}

	ref, checkoutRef := sha, sha
	if !pinned {
		ref, checkoutRef = it.defaultBranch, "FETCH_HEAD"
	}
	forced, err := o.git.Fetch(ctx, dir, ref)
	if err != nil {
		res.err = fmt.Errorf("fetching %s in %s: %w", ref, it.repo, err)
		return res
	}
	res.forced = forced
	if err := o.git.Checkout(ctx, dir, checkoutRef); err != nil {
		res.err = fmt.Errorf("checking out %s in %s: %w", ref, it.repo, err)
		return res
	}
	return o.tagHead(ctx, dir, tag, collectStatusUpdated, res)
}

// tagHead reads HEAD, tags it with the collection tag, and records the status.
func (o *collectOpts) tagHead(ctx context.Context, dir, tag, status string, res collectResult) collectResult {
	head, err := o.git.Head(ctx, dir)
	if err != nil {
		res.err = fmt.Errorf("reading HEAD of %s: %w", res.repo, err)
		return res
	}
	if err := o.git.CreateTag(ctx, dir, tag, head); err != nil {
		res.err = fmt.Errorf("tagging %s in %s: %w", tag, res.repo, err)
		return res
	}
	res.sha = head
	res.status = status
	return res
}

// writeManifest appends a row per newly collected/updated repo to
// <out>/collected.csv, so the graded SHAs live in one place.
func (o *collectOpts) writeManifest(label string, results []collectResult) error {
	var rows [][]string
	stamp := o.now().Format(time.RFC3339)
	for _, r := range results {
		if r.err != nil || (r.status != collectStatusCollected && r.status != collectStatusUpdated) {
			continue
		}
		rows = append(rows, []string{label, r.key, r.repo, r.sha, r.ref, stamp})
	}
	if len(rows) == 0 {
		return nil
	}
	path := filepath.Join(o.out, "collected.csv")
	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening manifest %s: %w", path, err)
	}
	w := csv.NewWriter(f)
	if isNew {
		_ = w.Write([]string{"label", "key", "repo", "sha", "ref", "time"})
	}
	for _, row := range rows {
		_ = w.Write(row)
	}
	w.Flush()
	werr := w.Error()
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("writing manifest %s: %w", path, werr)
	}
	if cerr != nil {
		return fmt.Errorf("closing manifest %s: %w", path, cerr)
	}
	return nil
}

// reportReconcile prints the missing/unexpected reconciliation up front.
func reportReconcile(out io.Writer, items []repoItem, missing []string) {
	var unexpected []string
	for _, it := range items {
		if it.unexpected {
			unexpected = append(unexpected, it.repo)
		}
	}
	sort.Strings(unexpected)
	if len(missing) > 0 {
		fmt.Fprintf(out, "missing (no repo) for %d student/team(s):\n  %s\n", len(missing), strings.Join(missing, "\n  "))
	}
	if len(unexpected) > 0 {
		fmt.Fprintf(out, "unexpected (not in roster/teams), collected anyway:\n  %s\n", strings.Join(unexpected, "\n  "))
	}
}

// reportCollect prints per-repo outcomes and a summary, returning an error if any
// repository failed.
func reportCollect(out io.Writer, results []collectResult, missing []string) error {
	var collected, updated, upToDate, skipped, failed int
	fmt.Fprintln(out)
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
			fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
		case r.status == collectStatusCollected:
			collected++
			fmt.Fprintf(out, "  collected %s\n", r.repo)
		case r.status == collectStatusUpdated:
			updated++
			line := fmt.Sprintf("  updated %s", r.repo)
			if r.forced {
				line += " (warning: upstream history was rewritten since the last collect; the prior state keeps its tag)"
			}
			fmt.Fprintln(out, line)
		case r.status == collectStatusUpToDate:
			upToDate++
		default: // dirty / no-sha
			skipped++
			fmt.Fprintf(out, "  %s %s\n", r.status, r.repo)
		}
	}
	fmt.Fprintf(out, "\n%d collected, %d updated, %d up-to-date, %d skipped, %d failed\n",
		collected, updated, upToDate, skipped, failed)
	if len(missing) > 0 {
		fmt.Fprintf(out, "note: %d student/team(s) have no repo (see above)\n", len(missing))
	}
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed", failed)
	}
	return nil
}

// parseCommits reads a YAML map of key->commit SHA, lower-casing keys for
// matching and rejecting an empty SHA.
func parseCommits(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading commits file %s: %w", path, err)
	}
	var raw map[string]string
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing commits file %s: %w", path, err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("commits file %s: empty SHA for %q", path, k)
		}
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

// execGit is the real gitRunner: clones via gh (inheriting its auth) and runs
// git for everything else.
type execGit struct{}

func (execGit) CloneExists(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

func (execGit) Clone(ctx context.Context, orgName, repo, dir string) error {
	_, stderr, err := gh2.ExecContext(ctx, "repo", "clone", orgName+"/"+repo, dir, "--", "--depth", "1")
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (execGit) run(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (g execGit) WorktreeClean(ctx context.Context, dir string) (bool, error) {
	out, errb, err := g.run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out) == "", nil
}

func (g execGit) Head(ctx context.Context, dir string) (string, error) {
	out, errb, err := g.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

func (g execGit) TagExists(ctx context.Context, dir, tag string) (bool, error) {
	out, errb, err := g.run(ctx, dir, "tag", "-l", tag)
	if err != nil {
		return false, fmt.Errorf("git tag -l: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out) == tag, nil
}

func (g execGit) Fetch(ctx context.Context, dir, ref string) (bool, error) {
	out, errb, err := g.run(ctx, dir, "fetch", "--depth", "1", "origin", ref)
	if err != nil {
		return false, fmt.Errorf("git fetch %s: %w: %s", ref, err, strings.TrimSpace(errb))
	}
	return strings.Contains(out+errb, "forced update"), nil
}

func (g execGit) Checkout(ctx context.Context, dir, ref string) error {
	if _, errb, err := g.run(ctx, dir, "checkout", "--detach", ref); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", ref, err, strings.TrimSpace(errb))
	}
	return nil
}

func (g execGit) CreateTag(ctx context.Context, dir, tag, sha string) error {
	if _, errb, err := g.run(ctx, dir, "tag", tag, sha); err != nil {
		return fmt.Errorf("git tag %s: %w: %s", tag, err, strings.TrimSpace(errb))
	}
	return nil
}
