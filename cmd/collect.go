package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	gh2 "github.com/cli/go-gh/v2"
	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/groups"
	"github.com/rixner/gh-cls/roster"
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
	// GetRef resolves a ref to a SHA, so an unpinned run knows the commit it is
	// collecting before it runs any git at all.
	GetRef(ctx context.Context, owner, repo, ref string) (string, error)
	// CompareCommits tells a rewrite from an ordinary update when the clone has
	// no history to answer with locally.
	CompareCommits(ctx context.Context, owner, repo, base, head string) (status string, found bool, err error)
}

// gitRunner is the seam over the git and `gh repo clone` operations collect
// performs, so tests can fake them without touching disk or the network.
type gitRunner interface {
	CloneExists(dir string) bool
	// Clone makes a new clone. full asks for the whole history and every branch
	// rather than the single commit a snapshot collection needs.
	Clone(ctx context.Context, org, repo, dir string, full bool) error
	// RemoteURL returns the URL of the clone's origin remote.
	RemoteURL(ctx context.Context, dir string) (string, error)
	// WorktreeState reports what a clone's worktree holds that its commit does
	// not. Modified tracked files stop a collection; untracked ones only earn a
	// warning, since they are usually a grader's own output sitting beside the
	// code rather than a change to it.
	WorktreeState(ctx context.Context, dir string) (worktreeState, error)
	// HeadHeldByRef reports whether any branch, tag or remote-tracking ref
	// contains HEAD. A commit no ref holds is one a grader made in the clone,
	// and moving HEAD off it leaves it reachable only from the reflog.
	HeadHeldByRef(ctx context.Context, dir string) (bool, error)
	Head(ctx context.Context, dir string) (string, error)
	TagExists(ctx context.Context, dir, tag string) (bool, error)
	// TagSHA returns the commit a tag points at, which is the state that was
	// collected under that tag however the clone has been moved since.
	TagSHA(ctx context.Context, dir, tag string) (string, error)
	// Fetch shallow-fetches ref (always a SHA) from origin.
	Fetch(ctx context.Context, dir, ref string) error
	// FetchAll brings every branch up to date in one request, and target too
	// when it is named. It is the full setting's fetch: it deepens the clone
	// when unshallow is set, prunes branches deleted on GitHub, and takes forced
	// updates, all without touching tags or local refs.
	FetchAll(ctx context.Context, dir, target string, unshallow bool) error
	// DeleteRemoteTrackingRefs clears refs/remotes/origin/*. A snapshot clone is
	// meant to hold the collected commit and nothing else; the origin/main a
	// clone arrives with points at the tip as of clone time, which in a pinned
	// collection is a commit after the deadline.
	DeleteRemoteTrackingRefs(ctx context.Context, dir string) error
	// IsShallow reports whether the clone was cut off at a depth. It decides
	// whether a full fetch may pass --unshallow, which git rejects outright on a
	// complete repository.
	IsShallow(ctx context.Context, dir string) (bool, error)
	// HasCommit reports whether a commit is already in the clone. It is what
	// keeps a depth-1 fetch away from a commit the clone already holds, which
	// would make that commit a shallow boundary and cost every tag behind it
	// its history at the next gc.
	HasCommit(ctx context.Context, dir, sha string) (bool, error)
	// IsAncestor reports whether a is an ancestor of b. It answers only when the
	// clone holds the history to decide; ok is false when it does not, and the
	// caller has to ask GitHub instead.
	IsAncestor(ctx context.Context, dir, a, b string) (yes, ok bool, err error)
	Checkout(ctx context.Context, dir, ref string) error
	CreateTag(ctx context.Context, dir, tag, sha string) error
	// CurrentBranch returns the branch HEAD is on, or "" when HEAD is detached.
	// A fresh clone is always on one, and collect deletes it so that nothing in
	// the clone can later be taken for a branch that tracks GitHub.
	CurrentBranch(ctx context.Context, dir string) (string, error)
	DeleteBranch(ctx context.Context, dir, branch string) error
	// MoveIntoPlace moves a finished clone from the staging area to its place
	// under --out. It fails with errTargetExists rather than replace anything
	// already there, which is the whole point: it is the one operation that
	// writes a path under --out, and it writes only names that do not yet exist.
	MoveIntoPlace(ctx context.Context, staging, dir string) error
	// SetConfig writes one setting into a clone. Collect uses it for
	// checkout.guess, without which `git checkout main` recreates the branch
	// from origin/main and silently yields code other than what was collected.
	SetConfig(ctx context.Context, dir, key, value string) error
	// CheckRefFormat reports whether ref is a name git will accept. It takes no
	// clone: it is asked once, before any repository is touched, so a label that
	// cannot become a tag fails the run before it has moved a worktree.
	//
	// A false with a nil error is git's considered no. A non-nil error means git
	// could not be asked at all, which is a different problem and must not reach
	// the instructor as a complaint about their label.
	CheckRefFormat(ctx context.Context, ref string) (bool, error)
}

// collectOpts carries the resolved flags and dependencies for `gh cls collect`.
type collectOpts struct {
	g         *globalOpts
	roster    string
	groups    string
	snapshot  string
	out       string
	label     string
	history   string
	dryRun    bool
	now       func() time.Time
	newClient func(context.Context) (collectClient, error)
	git       gitRunner
}

func newCollectCmd(g *globalOpts) *cobra.Command {
	o := &collectOpts{
		g:         g,
		now:       time.Now,
		newClient: func(context.Context) (collectClient, error) { return g.client() },
		git:       newPacedGit(execGit{}, cloneSpacing),
	}
	cmd := &cobra.Command{
		Use:   "collect <name>",
		Short: "Clone each student's repository locally for grading",
		Long: `Maintain one clone per student (or group) under --out, taking each repo to a
target commit and tagging it so every collection is preserved. The default target
is the repo's default-branch tip; --snapshot pins the exact SHAs recorded by a
gh cls activity --snapshot run. Re-running
the same --label tops up only repos not yet collected under it; a new label
updates the clones to the new target and tags the new state, leaving prior tags
in place so no collected state is ever lost. A label names one commit per repo
for good: asking an existing label for a different commit is refused rather than
quietly moving what that label means.

Clones are shallow by default, holding just the collected commit. --history full
keeps every commit and branch instead, and is recorded for the --out directory so
later runs match it even without the flag.

Roster-aware: it collects every <name>-* repo and reports any that are missing
(a student with no repo) or unexpected (a repo matching no roster/groups entry).
A clone whose tracked files you have modified is left untouched, and so is a
commit you made in it that no branch or tag holds, so grading edits survive;
untracked files of your own do not block a collection but are reported.

This is the one command that uses git: clones go through gh, updates through git.
See COLLECT.md for the model and the git you need.`,
		Example: `  gh cls collect hw1 --roster roster.csv --out ./hw1
  gh cls collect hw1 --roster roster.csv --out ./hw1-final --snapshot deadline.yml --label final
  gh cls collect project --groups groups.yml --out ./project`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.out, "out", "o", "", "destination directory; one clone per repo at <out>/<key> (required)")
	f.StringVarP(&o.roster, "roster", "r", "", "roster CSV (required for an individual assignment)")
	f.StringVarP(&o.groups, "groups", "g", "", "groups file (required for a group assignment)")
	f.StringVarP(&o.snapshot, "snapshot", "s", "", "snapshot file of key->commit SHA, as written by gh cls activity --snapshot; collect exactly those commits")
	f.StringVar(&o.label, "label", "", "name for this collection's tag (default: a timestamp)")
	f.StringVar(&o.history, "history", "", "how much history to keep: snapshot (the collected commit) or full (every commit and branch); recorded for --out and used by later runs")
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
	// detail explains a status that needs more than its own word, and carries
	// the fix. A refusal always has one.
	detail string
	// networkOps is how many paced git operations this repository would cost,
	// counted during a dry run so the plan can say what a real run would spend.
	networkOps int
	err        error
}

const (
	collectStatusCollected = "collected"
	collectStatusUpdated   = "updated"
	collectStatusUpToDate  = "up-to-date"
	collectStatusDirty     = "skipped (local changes)"
	collectStatusNoSHA     = "skipped (not in the snapshot)"
	// collectStatusRefused is a repository collect will not touch because doing
	// so would make something permanently wrong, as against one it merely could
	// not finish. It exits non-zero: it points at a mistake to correct.
	collectStatusRefused = "refused"
	// collectStatusStranded is a clone whose HEAD no ref holds: a grader
	// committed here, and checking out anything else would leave that commit
	// reachable only from the reflog, which expires.
	collectStatusStranded = "skipped (HEAD held by no ref)"
	// collectStatusInTheWay is a clone where a file the target commit tracks is
	// sitting untracked or ignored in the worktree. Git refuses the checkout and
	// changes nothing, so the grader's file survives.
	collectStatusInTheWay = "skipped (files in the way)"
	// collectStatusEmpty is a repository with no commits. It clones fine and has
	// no HEAD, which used to leave a directory behind that failed every later run.
	collectStatusEmpty = "skipped (empty repository)"
	// The two outcomes a dry run reports in place of acting. Every other status
	// a dry run prints is one a real run would print too, because the checks
	// that produce them are read-only and are the same checks.
	collectStatusWouldCollect = "would collect"
	collectStatusWouldUpdate  = "would update"
)

// gitCLSDir is collect's own directory under --out, holding the staging area a
// new clone is assembled in. Collect creates, rewrites and removes what it keeps
// there freely; the one constraint is that nothing it does there may affect
// anything outside it.
const gitCLSDir = ".gh-cls"

// dirExists reports whether path is present, whatever it is.
func dirExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// worktreeState is what a clone's worktree holds beyond its commit. Ignored
// files are deliberately not counted: git does not list them without an extra
// walk of the whole tree, and the checkout refusing to overwrite one (V15) is
// what actually protects them.
type worktreeState struct {
	modified  int // tracked files changed, staged or not
	untracked int
}

// collectLine renders one repo's outcome for the progress stream. These are the
// lines the summary used to print once every clone had finished, which on a full
// class meant several silent minutes; the wording is unchanged, only when they
// appear. The statuses carry their own parenthetical reasons and so are too
// uneven to align, which is why collect asks for no outcome column.
func collectLine(r collectResult) (outcome, target string) {
	switch {
	case r.err != nil:
		return "FAILED", fmt.Sprintf("%s: %v", r.repo, r.err)
	case r.detail != "":
		return r.status, r.repo + ": " + oneLine(r.detail)
	default:
		return r.status, r.repo
	}
}

// oneLine collapses whitespace so a detail stays on the single progress line it
// is printed on. Git's messages carry embedded newlines and tabs, the list of
// files it refused to overwrite among them, which would otherwise break the
// stream into ragged fragments.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (o *collectOpts) run(ctx context.Context, out io.Writer, name string) error {
	policy, err := o.g.cfg.Resolve(name, config.Overrides{})
	if err != nil {
		return err
	}

	expected, err := o.expectedKeys(policy.Type, name)
	if err != nil {
		return err
	}

	var pinned map[string]string
	if o.snapshot != "" {
		if pinned, err = parseSnapshot(o.snapshot); err != nil {
			return err
		}
	}

	label := o.label
	if label == "" {
		label = o.now().Format("20060102-150405")
	}
	tag := collectTagPrefix + label
	// A label git cannot spell as a tag used to fail every repository at the
	// tagging step, by which point every worktree had already moved and nothing
	// had been recorded. Ask git first, while a refusal still costs nothing.
	ok, err := o.git.CheckRefFormat(ctx, "refs/tags/"+tag)
	if err != nil {
		return fmt.Errorf("checking whether %q is a usable tag name: %w", tag, err)
	}
	if !ok {
		return fmt.Errorf("--label %q cannot be used in a tag name: git will not accept %q; "+
			"use a label without spaces, without the characters ~^:?*[\\, without \"..\", and not ending in \".lock\"",
			label, tag)
	}

	// Settle the history setting before any network call, so a bad value costs
	// nothing.
	history, historySource, historyChangedFrom, err := resolveHistory(o.out, o.history)
	if err != nil {
		return err
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	all, err := client.ListOrgReposByPrefix(ctx, o.g.org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w", name, err)
	}
	all = filterAssignmentRepos(o.g.cfg, name, all)

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

	// A snapshot file for another assignment matches nothing here, and every
	// repository would be skipped as "not in the snapshot": a full run that
	// collects nobody and reads as if that were the answer. Refuse it instead,
	// before anything is cloned.
	unmatched := unmatchedSnapshotKeys(pinned, present)
	if len(pinned) > 0 && len(unmatched) == len(pinned) {
		return fmt.Errorf("snapshot file %s names %d key(s), none of which match a %s-* repository: %s; "+
			"this looks like another assignment's snapshot file",
			o.snapshot, len(pinned), name, strings.Join(unmatched, ", "))
	}

	fmt.Fprintf(out, "Collecting %s into %s (tag %s)\n", name, o.out, tag)
	fmt.Fprintf(out, "history: %s (%s)\n", history, historySource)
	switch {
	case historyChangedFrom == historySnapshot:
		fmt.Fprintf(out, "  clones here are deepened to their whole history and get every branch.\n"+
			"  History that is no longer on GitHub cannot come back, and is reported per repository.\n")
	case historyChangedFrom == historyFull:
		fmt.Fprintf(out, "  history already in these clones is kept, new collections have none,\n"+
			"  and other branches stop being updated: their refs stay where the last full run left them.\n")
	case history == historyFull:
		fmt.Fprintf(out, "  other branches show GitHub's state as of this run, which can be later than the collected commits.\n")
	}
	reportReconcile(out, items, missing)
	if len(unmatched) > 0 {
		fmt.Fprintf(out, "note: %d snapshot key(s) match no repository, so nothing is pinned for them:\n  %s\n",
			len(unmatched), strings.Join(unmatched, "\n  "))
	}

	if len(items) == 0 && !o.dryRun {
		fmt.Fprintf(out, "\nno repositories to collect\n")
		return nil
	}

	// Read before the dry run branches, since both need it and reading changes
	// nothing. A missing manifest is an empty record, not an error.
	manifest := filepath.Join(o.out, "collected.csv")
	// What this label already records. A deleted tag is refilled from it rather
	// than from the current tip, and a tag that disagrees with it is refused.
	recorded, err := readManifestSHAs(manifest, label)
	if err != nil {
		return err
	}
	// The last collection of each repository under any label, which is what a
	// new target is compared against to tell an ordinary update from a rewrite.
	previous, err := readManifestLatest(manifest)
	if err != nil {
		return err
	}

	if o.dryRun {
		fmt.Fprintf(out, "\nDRY RUN: nothing is cloned, fetched, tagged or written\n")
		prog := newProgress(out, len(items), 0)
		results := runConcurrentProgress(ctx, o.g.concurrency, items, func(ctx context.Context, it repoItem) collectResult {
			return o.collectOne(ctx, client, o.g.org, name, tag, label, history, pinned, recorded, previous, it, true)
		}, func(r collectResult) { prog.item(collectLine(r)) })
		reportCollectDryRun(out, label, results)
		return nil
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", o.out, err)
	}
	// Claim the directory before touching anything in it. This comes after the
	// dry run has returned, so a dry run still writes nothing at all.
	lock, err := acquireRunLock(o.out)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := lock.release(); rerr != nil {
			fmt.Fprintf(out, "warning: %v\n", rerr)
		}
	}()

	// Record the setting for later runs, before any clone work, so a run that
	// forgets the flag and a colleague's run that never had it both match the
	// clones already here. Only a run that named a setting writes one: a flagless
	// run must not stamp the default onto a directory an older version made.
	if o.history != "" {
		if err := saveHistoryMode(o.out, history); err != nil {
			return err
		}
	}

	// Clear anything a killed run left staged. This is collect's own directory,
	// and the path is built from the resolved --out plus fixed segments, never
	// from a student key, so nothing a student or grader names can steer it.
	if err := os.RemoveAll(filepath.Join(o.out, gitCLSDir, "staging")); err != nil {
		return fmt.Errorf("clearing %s: %w", filepath.Join(o.out, gitCLSDir, "staging"), err)
	}

	prog := newProgress(out, len(items), 0) // statuses carry their own reasons; see collectLine
	results := runConcurrentProgress(ctx, o.g.concurrency, items, func(ctx context.Context, it repoItem) collectResult {
		return o.collectOne(ctx, client, o.g.org, name, tag, label, history, pinned, recorded, previous, it, false)
	}, func(r collectResult) { prog.item(collectLine(r)) })

	if err := o.writeManifest(label, results); err != nil {
		return err
	}
	return reportCollect(out, label, results, missing)
}

// expectedKeys returns the lower-cased->display key set the assignment's type
// defines (usernames from the roster for individual, group names from the groups
// file for group), validating that the right file was given.
func (o *collectOpts) expectedKeys(typ config.AssignmentType, name string) (map[string]string, error) {
	switch typ {
	case config.TypeIndividual:
		if o.roster == "" {
			return nil, fmt.Errorf("assignment %q is individual: --roster is required", name)
		}
		if o.groups != "" {
			return nil, fmt.Errorf("assignment %q is individual: --groups is not allowed", name)
		}
		r, err := roster.ParseFile(o.roster)
		if err != nil {
			return nil, err
		}
		return r.UsersByLowercase(), nil
	case config.TypeGroup:
		if o.groups == "" {
			return nil, fmt.Errorf("assignment %q is a group assignment: --groups is required", name)
		}
		if o.roster != "" {
			return nil, fmt.Errorf("assignment %q is a group assignment: --roster is not allowed (group names are the keys)", name)
		}
		g, err := groups.ParseFile(o.groups)
		if err != nil {
			return nil, err
		}
		keys := make(map[string]string, g.Len())
		for _, n := range g.Names() {
			keys[strings.ToLower(n)] = n
		}
		return keys, nil
	default:
		return nil, fmt.Errorf("assignment %q has an unknown type %q", name, typ)
	}
}

// collectOne clones or updates one repository to its target commit and tags it.
// plan true stops before the first thing that would change anything, reporting
// what the run would do instead of doing it. Every check above those points is
// read-only, so a dry run reaches its answer through the same code a real run
// does rather than a second description of it that could drift.
func (o *collectOpts) collectOne(ctx context.Context, client collectClient, orgName, name, tag, label string,
	history historyMode, snapshot, recorded map[string]string, previous map[string]manifestEntry,
	it repoItem, plan bool) collectResult {
	res := collectResult{key: it.key, repo: it.repo, ref: it.defaultBranch}
	dir := filepath.Join(o.out, it.key)

	// <out>/.gh-cls is collect's own. A repository whose key lands there would
	// have its clone and collect's staging area fight over one directory.
	if strings.EqualFold(it.key, gitCLSDir) {
		res.status = collectStatusRefused
		res.detail = fmt.Sprintf("the key %q is the name collect uses for its own directory under --out; "+
			"rename the repository, or collect it into a different --out", it.key)
		return res
	}

	pinned := snapshot != nil
	var sha string
	if pinned {
		res.ref = "(pinned)"
		s, ok := snapshot[it.lkey]
		if !ok {
			res.status = collectStatusNoSHA
			return res
		}
		sha = s
	}

	// What this label already names for this repository, if anything: the
	// snapshot's pin, or the commit the manifest recorded under this label. A
	// label names one commit per repository, permanently, so whatever is found
	// on disk below has to agree with it.
	expected, expectedFrom := "", ""
	if pinned {
		expected, expectedFrom = sha, "the snapshot"
	}
	if row := recorded[it.repo]; row != "" {
		if expected != "" && !strings.EqualFold(expected, row) {
			res.status = collectStatusRefused
			res.detail = fmt.Sprintf("the snapshot says %s, but collected.csv already recorded %s under label %s; collect it under a new label",
				shortSHA(expected), shortSHA(row), label)
			return res
		}
		if expected == "" {
			expected, expectedFrom = row, "collected.csv"
		}
	}

	// The commit to land on, or empty to take the default branch's tip. A
	// recorded commit is used even unpinned: re-running a label whose tag or
	// clone was deleted must recover what that label named, not whatever the
	// student has pushed since.
	target := sha
	if target == "" {
		target = expected
	}
	if target == "" {
		// Unpinned, and this label has collected nothing for this repository
		// yet: the default branch's tip, read when collect reaches the repo,
		// which is what an unpinned collect has always meant. Resolving it here,
		// before any git runs, is what lets every fetch name a commit.
		tip, err := client.GetRef(ctx, orgName, it.repo, "heads/"+it.defaultBranch)
		if err != nil {
			if gh.IsNotFound(err) {
				res.status = collectStatusEmpty
				res.detail = fmt.Sprintf("GitHub has no %s branch for it (a repository with no commits yet), "+
					"so there is nothing to collect", it.defaultBranch)
				return res
			}
			res.err = fmt.Errorf("reading the tip of %s: %w", it.repo, err)
			return res
		}
		target = tip
	}

	if !o.git.CloneExists(dir) {
		// Something that is not a clone is in the way. Collect never moves or
		// deletes anything under --out, empty or not: judging a directory
		// disposable is the instructor's call, not collect's.
		if dirExists(dir) {
			res.status = collectStatusRefused
			res.detail = fmt.Sprintf("%s already exists and is not a clone collect can use; "+
				"remove it or choose another --out", dir)
			return res
		}
		if plan {
			res.status = collectStatusWouldCollect
			res.detail = "a new clone at " + shortSHA(target)
			res.networkOps = 1
			return res
		}
		return o.cloneInto(ctx, orgName, tag, target, dir, history, it, res)
	}

	// A directory with a .git but no readable HEAD is a clone that never
	// finished: a run killed mid-clone, or the empty-repository clone an older
	// version left behind. Either way collect neither removes nor moves it.
	head, err := o.git.Head(ctx, dir)
	if err != nil {
		res.status = collectStatusRefused
		res.detail = fmt.Sprintf("%s has no readable HEAD, so it is an incomplete clone "+
			"(a killed run, or a repository that had no commits when it was first collected); "+
			"delete the directory and re-run", dir)
		return res
	}

	// Existing clone. It is only this repository's clone if its origin says so:
	// an --out directory reused across assignments (or renamed by hand) holds a
	// clone of some other repo, and fetching into it would grade the wrong code
	// under this repo's name in the manifest.
	origin, err := o.git.RemoteURL(ctx, dir)
	if err != nil {
		res.err = fmt.Errorf("reading the origin of the existing clone %s: %w", dir, err)
		return res
	}
	if !originNames(origin, orgName, it.repo) {
		res.err = fmt.Errorf("existing clone %s has origin %s, which is not %s/%s; collect into a different --out directory, or remove %s and re-run",
			dir, origin, orgName, it.repo, dir)
		return res
	}

	if has, err := o.git.TagExists(ctx, dir, tag); err != nil {
		res.err = fmt.Errorf("checking tag on %s: %w", it.repo, err)
		return res
	} else if has {
		// Read the tag rather than HEAD: this SHA goes into the manifest when the
		// row is missing from it, and a grading checkout may have moved HEAD since
		// the collection. The error is no longer discarded for the same reason.
		tagged, err := o.git.TagSHA(ctx, dir, tag)
		if err != nil {
			res.err = fmt.Errorf("reading the %s tag on %s: %w", tag, it.repo, err)
			return res
		}
		// The tag is what this label collected. Asking the same label for a
		// different commit was reported as up to date, which left the instructor
		// believing the new commit had been collected; collecting it instead
		// would move what the label means. Neither is acceptable, so refuse.
		if expected != "" && !strings.EqualFold(expected, tagged) {
			res.status = collectStatusRefused
			res.detail = fmt.Sprintf("label %s holds %s, but %s says %s; collect it under a new label",
				label, shortSHA(tagged), expectedFrom, shortSHA(expected))
			return res
		}
		res.status = collectStatusUpToDate
		res.sha = tagged
		return res
	}
	state, err := o.git.WorktreeState(ctx, dir)
	if err != nil {
		res.err = fmt.Errorf("checking %s for local changes: %w", it.repo, err)
		return res
	}
	if state.modified > 0 {
		res.status = collectStatusDirty
		res.detail = fmt.Sprintf("%d tracked file(s) modified; commit, stash or discard them, then re-run", state.modified)
		return res
	}

	// A commit no ref holds is one a grader made in this clone. Checking out
	// anything else would leave it reachable only from the reflog, which expires,
	// so the clone is left alone and the grader is told how to keep it. HEAD
	// already sitting on the target is not at risk: nothing is left behind.
	if !strings.EqualFold(head, target) {
		held, err := o.git.HeadHeldByRef(ctx, dir)
		if err != nil {
			res.err = fmt.Errorf("checking what holds HEAD of %s: %w", it.repo, err)
			return res
		}
		if !held {
			res.status = collectStatusStranded
			res.detail = fmt.Sprintf("HEAD is at %s, which no branch or tag holds (a commit made in this clone?); "+
				"collecting would strand it. To keep it: git -C %s tag <name>", shortSHA(head), dir)
			return res
		}
	}

	if plan {
		// Whether a fetch would be needed is itself a read.
		has, hasErr := o.git.HasCommit(ctx, dir, target)
		if hasErr != nil {
			res.err = fmt.Errorf("checking whether %s already holds %s: %w", it.repo, shortSHA(target), hasErr)
			return res
		}
		res.status = collectStatusWouldUpdate
		switch {
		case history == historyFull:
			res.detail = "to " + shortSHA(target) + ", fetching every branch"
			res.networkOps = 1
		case has:
			res.detail = "to " + shortSHA(target) + ", already in the clone, so no fetch"
		default:
			res.detail = "to " + shortSHA(target) + ", fetching it first"
			res.networkOps = 1
		}
		return res
	}

	if err := o.bringUpToDate(ctx, dir, target, history); err != nil {
		res.err = fmt.Errorf("fetching %s in %s: %w", shortSHA(target), it.repo, err)
		return res
	}
	if err := o.git.Checkout(ctx, dir, target); err != nil {
		// Git refuses rather than overwrite a file it would clobber, and changes
		// nothing when it does (V14, V15). That is a repository to come back to
		// once the file is moved, not a failed run.
		if filesInTheWay(err) {
			res.status = collectStatusInTheWay
			res.detail = fmt.Sprintf("a file the target commit tracks is sitting in the worktree untracked or ignored; "+
				"move it aside and re-run (%v)", err)
			return res
		}
		res.err = fmt.Errorf("checking out %s in %s: %w", shortSHA(target), it.repo, err)
		return res
	}
	var notes []string
	if prev, ok := previous[it.repo]; ok && !strings.EqualFold(prev.sha, target) {
		if n := o.rewriteNote(ctx, client, dir, orgName, it, prev, target); n != "" {
			notes = append(notes, n)
		}
	}
	if state.untracked > 0 {
		notes = append(notes, fmt.Sprintf("collected with %d untracked file(s) still in the worktree", state.untracked))
	}
	// A clone with more history than this directory asks for: unshallowed by
	// hand, or copied in from a full-history directory. It is left exactly as it
	// is, since nothing here is worth throwing history away for, but silence
	// would leave the instructor thinking the directory is uniform.
	if history == historySnapshot {
		switch shallow, shallowErr := o.git.IsShallow(ctx, dir); {
		case shallowErr != nil:
			notes = append(notes, fmt.Sprintf("could not tell how much history this clone holds: %v", shallowErr))
		case !shallow:
			notes = append(notes, "this clone holds more history than this directory's snapshot setting, and is left as it is; "+
				"--history full would keep it on purpose")
		}
	}
	res.detail = strings.Join(notes, "; ")
	return o.tagHead(ctx, dir, tag, collectStatusUpdated, res)
}

// ensureCommit brings target into the clone, and does nothing when it is already
// there.
//
// That condition is the whole point. A depth-1 fetch naming a commit the clone
// already holds makes it a shallow boundary: every tag whose history runs
// through it loses that history, and the next gc deletes the commits (V1). Since
// every target is resolved to a SHA first, a fetch only ever asks for something
// missing, which is the harmless case (V2).
func (o *collectOpts) bringUpToDate(ctx context.Context, dir, target string, history historyMode) error {
	has, err := o.git.HasCommit(ctx, dir, target)
	if err != nil {
		return err
	}
	if history == historySnapshot {
		if has {
			return nil
		}
		return o.git.Fetch(ctx, dir, target)
	}

	// The full setting fetches on every run even when the target is already
	// here, because that is how the other branches stay current; the cost is one
	// paced request per repository. --unshallow goes only to a clone still cut
	// off at a depth, since git refuses it outright on a complete repository.
	shallow, err := o.git.IsShallow(ctx, dir)
	if err != nil {
		return err
	}
	name := ""
	if !has {
		name = target
	}
	return o.git.FetchAll(ctx, dir, name, shallow)
}

// rewriteNote describes how target stands to the commit a previous collection
// recorded, or "" when it is an ordinary descendant and there is nothing to say.
//
// It replaces reading git's "(forced update)" text, which was wrong both ways:
// it fired on every ordinary update of a shallow clone, because the old tip's
// ancestry is not local so git cannot tell a fast-forward from a rewrite, and it
// never fired on a pinned run, because fetching a SHA updates no tracking ref.
func (o *collectOpts) rewriteNote(ctx context.Context, client collectClient, dir, orgName string, it repoItem, prev manifestEntry, target string) string {
	rewritten := fmt.Sprintf("history was rewritten since %s, which is still tagged at %s",
		prev.label, shortSHA(prev.sha))
	older := fmt.Sprintf("this target is older than %s, which holds %s", prev.label, shortSHA(prev.sha))

	// Ask the clone first: it costs no request, and a full clone can answer.
	if descends, ok, err := o.git.IsAncestor(ctx, dir, prev.sha, target); err == nil && ok {
		if descends {
			return ""
		}
		if behind, ok2, err2 := o.git.IsAncestor(ctx, dir, target, prev.sha); err2 == nil && ok2 && behind {
			return older
		}
		return rewritten
	}

	// A shallow clone holds no ancestry, so GitHub is the only thing that knows.
	status, found, err := client.CompareCommits(ctx, orgName, it.repo, prev.sha, target)
	if err != nil {
		// A note is not worth failing a collection that otherwise succeeded.
		return fmt.Sprintf("could not tell whether history was rewritten since %s: %v", prev.label, err)
	}
	if !found {
		return fmt.Sprintf("history was rewritten since %s: the commit it recorded (%s) is gone from GitHub",
			prev.label, shortSHA(prev.sha))
	}
	switch status {
	case "ahead", "identical":
		return ""
	case "behind":
		return older
	default:
		return rewritten
	}
}

// cloneInto builds a new clone somewhere else and moves it into place only once
// it is complete and tagged, so <out>/<key> never exists in a half-made state.
//
// This is what makes a failed first collection leave nothing behind. Cloning
// straight into <out>/<key> put the tip on disk, untagged and often
// post-deadline, and a grader browsing the directory saw what looked like a
// collection; a run killed mid-clone left a partial directory that every later
// run then treated as a clone. Neither can happen to a directory that only
// appears once it is finished.
func (o *collectOpts) cloneInto(ctx context.Context, orgName, tag, target, dir string, history historyMode,
	it repoItem, res collectResult) collectResult {
	staging := filepath.Join(o.out, gitCLSDir, "staging", it.key)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		res.err = fmt.Errorf("creating the staging directory for %s: %w", it.repo, err)
		return res
	}
	// Anything already here is a leftover from a killed run, in collect's own
	// directory. Removing it affects nothing outside .gh-cls.
	if err := os.RemoveAll(staging); err != nil {
		res.err = fmt.Errorf("clearing the staging directory for %s: %w", it.repo, err)
		return res
	}
	// Any return before the move leaves nothing behind.
	defer func() { _ = os.RemoveAll(staging) }()

	if err := o.git.Clone(ctx, orgName, it.repo, staging, history == historyFull); err != nil {
		res.err = fmt.Errorf("cloning %s: %w", it.repo, err)
		return res
	}

	// A repository with no commits clones fine and has no HEAD. It is reported
	// and gets no directory at all, rather than leaving an empty clone that
	// fails on every later run.
	if _, err := o.git.Head(ctx, staging); err != nil {
		res.status = collectStatusEmpty
		res.detail = "the repository has no commits yet, so there is nothing to collect"
		return res
	}

	// Read the branch before detaching, while there still is one.
	branch, err := o.git.CurrentBranch(ctx, staging)
	if err != nil {
		res.err = fmt.Errorf("reading the branch of the new clone of %s: %w", it.repo, err)
		return res
	}

	if err := o.bringUpToDate(ctx, staging, target, history); err != nil {
		res.err = fmt.Errorf("fetching %s in %s: %w", shortSHA(target), it.repo, err)
		return res
	}
	if err := o.git.Checkout(ctx, staging, target); err != nil {
		res.err = fmt.Errorf("checking out %s in %s: %w", shortSHA(target), it.repo, err)
		return res
	}
	if branch != "" {
		if err := o.git.DeleteBranch(ctx, staging, branch); err != nil {
			res.err = fmt.Errorf("removing the branch %s left by the new clone of %s: %w", branch, it.repo, err)
			return res
		}
	}
	// Without this, `git checkout main` recreates the branch from origin/main
	// and hands a grading script code other than what was collected.
	if err := o.git.SetConfig(ctx, staging, "checkout.guess", "false"); err != nil {
		res.err = fmt.Errorf("setting checkout.guess in the new clone of %s: %w", it.repo, err)
		return res
	}

	// A snapshot clone holds the collected commit and nothing else. Left in
	// place, origin/main would sit at the tip as of clone time, which in a
	// pinned collection is a commit from after the deadline, and `git checkout
	// origin/main` would show a grader code that was never collected. The full
	// setting keeps them: mirroring GitHub's branches is the point of it.
	if history == historySnapshot {
		if err := o.git.DeleteRemoteTrackingRefs(ctx, staging); err != nil {
			res.err = fmt.Errorf("clearing the remote-tracking refs of the new clone of %s: %w", it.repo, err)
			return res
		}
	}

	if res = o.tagHead(ctx, staging, tag, collectStatusCollected, res); res.err != nil {
		return res
	}

	// The move is the commit point, and it refuses rather than replace.
	if err := o.git.MoveIntoPlace(ctx, staging, dir); err != nil {
		if errors.Is(err, errTargetExists) {
			res.status = collectStatusRefused
			res.detail = fmt.Sprintf("%s appeared while %s was being collected; "+
				"remove it or choose another --out, then re-run", dir, it.repo)
			return res
		}
		res.err = fmt.Errorf("moving the new clone of %s into place: %w", it.repo, err)
		return res
	}
	return res
}

// originNames reports whether a clone's origin URL names org/repo. Remotes are
// written in several forms for the same repository (https://host/org/repo,
// git@host:org/repo, ssh://git@host/org/repo, each with or without a trailing
// ".git" or "/"), and GitHub owner and repository names are case-insensitive, so
// the comparison is on the normalized owner/name tail rather than the whole URL.
func originNames(origin, org, repo string) bool {
	u := strings.TrimSuffix(strings.TrimSpace(origin), "/")
	u = strings.TrimSuffix(u, ".git")
	want := org + "/" + repo
	return strings.EqualFold(u, want) ||
		strings.HasSuffix(strings.ToLower(u), strings.ToLower("/"+want)) ||
		strings.HasSuffix(strings.ToLower(u), strings.ToLower(":"+want))
}

// shortSHA abbreviates a commit for a message, leaving anything too short to
// abbreviate (a hand-edited manifest cell) exactly as it was written.
func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
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

// writeManifest appends a row to <out>/collected.csv for every repo this run
// collected under the label, so the graded SHAs live in one place.
//
// An up-to-date repo is included when the manifest has no row for it. Tags are
// created per repo as the run proceeds but the manifest is written at the end,
// so a run that dies partway leaves repos tagged and unrecorded; on the re-run
// those repos are up-to-date, and skipping them would leave a hole in the record
// of what was graded that no later run could ever fill. Rows already present are
// never written twice, which is what makes the repair safe to repeat.
func (o *collectOpts) writeManifest(label string, results []collectResult) error {
	path := filepath.Join(o.out, "collected.csv")
	recorded, err := readManifestKeys(path)
	if err != nil {
		return err
	}

	var rows [][]string
	stamp := o.now().Format(time.RFC3339)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		switch r.status {
		case collectStatusCollected, collectStatusUpdated, collectStatusUpToDate:
		default: // dirty or absent from the snapshot: nothing was collected to record
			continue
		}
		if recorded[manifestKey(label, r.repo)] {
			continue
		}
		rows = append(rows, []string{label, r.key, r.repo, r.sha, r.ref, stamp})
	}
	if len(rows) == 0 {
		return nil
	}
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

// readManifestSHAs returns the commit the manifest records for each repository
// under one label. It is read once, before any repository is touched, and is
// what makes a label mean one commit per repository however the clones have been
// disturbed since: a deleted tag is refilled from it rather than from the
// current tip, and a tag that disagrees with it is refused.
func readManifestSHAs(path, label string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening manifest %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w; fix or remove it, then re-run", path, err)
	}
	out := map[string]string{}
	for _, rec := range records {
		if len(rec) < 4 || rec[0] != label {
			continue
		}
		if sha := strings.TrimSpace(rec[3]); sha != "" {
			out[rec[2]] = sha
		}
	}
	return out, nil
}

// manifestEntry is one recorded collection: the commit, and the label it was
// collected under.
type manifestEntry struct {
	sha   string
	label string
}

// readManifestLatest returns the most recent collection recorded for each
// repository, whatever label it was under. Rows are appended as runs proceed, so
// a repository's last row is its latest collection.
func readManifestLatest(path string) (map[string]manifestEntry, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]manifestEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening manifest %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w; fix or remove it, then re-run", path, err)
	}
	out := map[string]manifestEntry{}
	for _, rec := range records {
		if len(rec) < 4 || (rec[0] == "label" && rec[2] == "repo") { // skip the header
			continue
		}
		if sha := strings.TrimSpace(rec[3]); sha != "" {
			out[rec[2]] = manifestEntry{sha: sha, label: rec[0]}
		}
	}
	return out, nil
}

// manifestKey identifies one manifest row: a repository under one label. A
// second collection of the same repo under a new label is a new row, which is
// the point of labels.
func manifestKey(label, repo string) string { return label + "\x00" + repo }

// readManifestKeys returns the (label, repo) pairs the manifest already records,
// so a row is written once and only once. A missing file is an empty set; a file
// that cannot be read or parsed is an error, since appending to a manifest whose
// contents are unknown would duplicate rows silently.
func readManifestKeys(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening manifest %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	// Rows are read only for their label and repo, so a row of another width (a
	// hand-edited file) is tolerated rather than failing the run.
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w; fix or remove it, then re-run", path, err)
	}
	keys := make(map[string]bool, len(records))
	for _, rec := range records {
		if len(rec) < 3 {
			continue
		}
		keys[manifestKey(rec[0], rec[2])] = true
	}
	return keys, nil
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
		fmt.Fprintf(out, "missing (no repo) for %d student/group(s):\n  %s\n", len(missing), strings.Join(missing, "\n  "))
	}
	if len(unexpected) > 0 {
		fmt.Fprintf(out, "unexpected (not in roster/groups), collected anyway:\n  %s\n", strings.Join(unexpected, "\n  "))
	}
}

// reportCollect summarizes the run, returning an error if any repository failed.
// Each repo's own line is streamed as it finishes (see collectLine), so this
// counts rather than re-lists them.
// reportPlan summarizes a dry run: what a real run would do, what it would
// refuse, and what it would spend. The old dry run listed every repository as
// "would collect" without opening a single clone, so it could not say what was
// already up to date, dirty, or about to be refused, which is the only thing
// worth knowing before committing a class-sized run to the network.
func reportCollectDryRun(out io.Writer, label string, results []collectResult) {
	counts := map[string]int{}
	ops := 0
	for _, r := range results {
		if r.err != nil {
			counts["failed a check"]++
			continue
		}
		counts[r.status]++
		ops += r.networkOps
	}
	statuses := make([]string, 0, len(counts))
	for s := range counts {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)

	fmt.Fprintf(out, "\nPlan for %s:\n", label)
	for _, s := range statuses {
		fmt.Fprintf(out, "  %d  %s\n", counts[s], s)
	}
	// The pacing is the whole cost of a collection: the clones and fetches are
	// serialized and spaced, so the count is the run's length.
	fmt.Fprintf(out, "  %d paced git operation(s), about %s at the current spacing\n",
		ops, (time.Duration(ops) * cloneSpacing).Round(time.Second))

	if lines := notCollected(results); len(lines) > 0 {
		fmt.Fprintf(out, "\nWould not be collected under %s (%d):\n%s\n", label, len(lines), strings.Join(lines, "\n"))
	}
}

// notCollected lists the repositories this run did not put under the label,
// each with its reason and fix. Every one still holds whatever it held before,
// which is exactly what a grader cannot see by looking at the directory.
func notCollected(results []collectResult) []string {
	var lines []string
	for _, r := range results {
		switch {
		case r.err != nil:
			lines = append(lines, fmt.Sprintf("  %s  FAILED: %s", r.repo, oneLine(r.err.Error())))
		case r.status == collectStatusCollected, r.status == collectStatusUpdated, r.status == collectStatusUpToDate,
			r.status == collectStatusWouldCollect, r.status == collectStatusWouldUpdate:
			// Collected under this label, or would be by the run being planned.
			// The planned outcomes belong here too: listing a repository a dry
			// run says it would collect under "would not be collected" is the
			// same lie in the other direction.
		default:
			line := fmt.Sprintf("  %s  %s", r.repo, r.status)
			if r.detail != "" {
				line += ": " + oneLine(r.detail)
			}
			lines = append(lines, line)
		}
	}
	sort.Strings(lines) // the repo name leads each line, so this orders by repo
	return lines
}

// collectedWithNotes lists repositories that were collected but carry something
// worth saying: a rewritten upstream history, or a grader's files still sitting
// in the worktree beside the newly checked-out code.
func collectedWithNotes(results []collectResult) []string {
	var lines []string
	for _, r := range results {
		if r.err != nil || r.detail == "" {
			continue
		}
		switch r.status {
		case collectStatusCollected, collectStatusUpdated, collectStatusUpToDate:
			lines = append(lines, fmt.Sprintf("  %s  %s", r.repo, oneLine(r.detail)))
		}
	}
	sort.Strings(lines)
	return lines
}

func reportCollect(out io.Writer, label string, results []collectResult, missing []string) error {
	var collected, updated, upToDate, skipped, refused, failed int
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
		case r.status == collectStatusCollected:
			collected++
		case r.status == collectStatusUpdated:
			updated++
		case r.status == collectStatusUpToDate:
			upToDate++
		case r.status == collectStatusRefused:
			refused++
		default: // dirty / no-sha
			skipped++
		}
	}
	fmt.Fprintf(out, "\n%d collected, %d updated, %d up-to-date, %d skipped, %d refused, %d failed\n",
		collected, updated, upToDate, skipped, refused, failed)

	// The per-repo lines stream as the run goes, so on a class-sized collection
	// the early ones have scrolled away by the end. A grader who opens a
	// student's directory sees code either way and cannot tell that this label
	// passed the repository over, so every one of them is named again here.
	if lines := notCollected(results); len(lines) > 0 {
		fmt.Fprintf(out, "\nNot collected under %s (%d):\n%s\n", label, len(lines), strings.Join(lines, "\n"))
	}
	if lines := collectedWithNotes(results); len(lines) > 0 {
		fmt.Fprintf(out, "\nCollected, with something to note (%d):\n%s\n", len(lines), strings.Join(lines, "\n"))
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "note: %d student/group(s) have no repo (see above)\n", len(missing))
	}
	// A refusal is not a failure to do the work; it is collect declining to make
	// something permanently wrong. It still exits non-zero, because it names a
	// mistake only the instructor can correct.
	switch {
	case failed > 0 && refused > 0:
		return fmt.Errorf("%d repo(s) failed, %d refused", failed, refused)
	case failed > 0:
		return fmt.Errorf("%d repo(s) failed", failed)
	case refused > 0:
		return fmt.Errorf("%d repo(s) refused", refused)
	}
	return nil
}

// parseSnapshot reads a snapshot file (a YAML map of key->commit SHA), lower-
// casing keys for matching and rejecting an empty SHA.
func parseSnapshot(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading snapshot file %s: %w", path, err)
	}
	var raw map[string]string
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing snapshot file %s: %w", path, err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			return nil, fmt.Errorf("snapshot file %s: empty SHA for %q", path, k)
		}
		if !fullCommitSHA.MatchString(v) {
			return nil, fmt.Errorf("snapshot file %s: %q for %q is not a full commit SHA; "+
				"give the whole 40-character SHA (64 in a SHA-256 repository) that gh cls activity --snapshot writes",
				path, v, k)
		}
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

// fullCommitSHA matches a complete git object name: 40 hex digits for a SHA-1
// repository, 64 for SHA-256. An abbreviation is rejected rather than resolved,
// because collect names the commit to GitHub in a fetch, which takes the whole
// name, and because a prefix that is unambiguous today need not stay so.
var fullCommitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// unmatchedSnapshotKeys returns the snapshot keys that name no repository in
// this assignment, sorted. A mistyped key collects nothing for that student and
// says nothing about it, so they are named rather than passed over.
func unmatchedSnapshotKeys(snapshot map[string]string, present map[string]bool) []string {
	var out []string
	for k := range snapshot {
		if !present[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// execGit is the real gitRunner: clones via gh (inheriting its auth) and runs
// git for everything else.
type execGit struct{}

func (execGit) CloneExists(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

func (execGit) Clone(ctx context.Context, orgName, repo, dir string, full bool) error {
	// --no-tags: a clone otherwise imports the student's tags, and a student who
	// pushes gh-cls/collect/<label> at a commit of their choosing would have it
	// read back as the collection under that label.
	args := []string{"repo", "clone", orgName + "/" + repo, dir, "--", "--no-tags"}
	if !full {
		// Without --depth the clone already carries every commit and branch.
		args = append(args, "--depth", "1")
	}
	_, stderr, err := gh2.ExecContext(ctx, args...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// FetchAll is the full setting's fetch: every branch, plus target when it is
// named, in one request.
//
// The refspec is given on the command line rather than left to the clone's
// config, because older and hand-made clones are often single-branch and would
// otherwise never see the other branches. --prune removes branches deleted on
// GitHub and forced updates are taken, both of which touch only
// refs/remotes/origin/*: tags and local branches, and so every collected commit
// and any grader branch, are untouched.
func (g execGit) FetchAll(ctx context.Context, dir, target string, unshallow bool) error {
	args := []string{"fetch", "--no-tags", "--prune"}
	if unshallow {
		// git refuses this outright on a complete repository, so it is passed
		// exactly when the clone is still cut off at a depth.
		args = append(args, "--unshallow")
	}
	args = append(args, "origin", "+refs/heads/*:refs/remotes/origin/*")
	if target != "" {
		// Named only when missing: a commit GitHub no longer has would otherwise
		// fail the whole fetch, taking the branch updates down with it.
		args = append(args, target)
	}
	if _, errb, err := g.run(ctx, dir, args...); err != nil {
		return fmt.Errorf("git fetch (all branches): %w: %s", err, strings.TrimSpace(errb))
	}
	return nil
}

// DeleteRemoteTrackingRefs removes every refs/remotes/origin/* ref.
//
// Symrefs go first and one at a time. origin/HEAD is a symref to origin/main,
// and deleting both in one `update-ref --stdin` batch is refused outright:
// "multiple updates for 'refs/remotes/origin/main' (including one via symref
// 'refs/remotes/origin/HEAD') are not allowed". A fresh clone has two or three
// of these refs, so deleting them individually costs nothing worth batching for.
//
// The remote itself is left alone: later runs still fetch through it, and a run
// that switches this directory to full history rebuilds the whole mirror.
func (g execGit) DeleteRemoteTrackingRefs(ctx context.Context, dir string) error {
	out, errb, err := g.run(ctx, dir, "for-each-ref", "--format=%(refname) %(symref)", "refs/remotes")
	if err != nil {
		return fmt.Errorf("git for-each-ref refs/remotes: %w: %s", err, strings.TrimSpace(errb))
	}
	var symbolic, plain []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		name, target, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		if strings.TrimSpace(target) != "" {
			symbolic = append(symbolic, name)
		} else {
			plain = append(plain, name)
		}
	}
	for _, ref := range append(symbolic, plain...) {
		if _, delErr, delRunErr := g.run(ctx, dir, "update-ref", "-d", ref); delRunErr != nil {
			return fmt.Errorf("git update-ref -d %s: %w: %s", ref, delRunErr, strings.TrimSpace(delErr))
		}
	}
	return nil
}

func (g execGit) IsShallow(ctx context.Context, dir string) (bool, error) {
	out, errb, err := g.run(ctx, dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, fmt.Errorf("git rev-parse --is-shallow-repository: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out) != "false", nil
}

func (execGit) run(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Pin the C locale so git's output (e.g. Fetch's "forced update" check) is
	// stable English regardless of the host's LANG/LC_ALL.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (g execGit) RemoteURL(ctx context.Context, dir string) (string, error) {
	out, errb, err := g.run(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

func (g execGit) WorktreeState(ctx context.Context, dir string) (worktreeState, error) {
	out, errb, err := g.run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return worktreeState{}, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(errb))
	}
	var st worktreeState
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "??"):
			st.untracked++
		default:
			st.modified++
		}
	}
	return st, nil
}

// HeadHeldByRef asks which refs contain HEAD. Emptiness is the answer: the
// command succeeds either way, so the exit status says nothing.
func (g execGit) HeadHeldByRef(ctx context.Context, dir string) (bool, error) {
	out, errb, err := g.run(ctx, dir, "for-each-ref", "--contains", "HEAD",
		"--format=%(refname)", "refs/heads", "refs/tags", "refs/remotes")
	if err != nil {
		return false, fmt.Errorf("git for-each-ref --contains HEAD: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out) != "", nil
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

func (g execGit) TagSHA(ctx context.Context, dir, tag string) (string, error) {
	// ^{} dereferences an annotated tag to its commit; on the lightweight tags
	// collect creates it is a no-op.
	out, errb, err := g.run(ctx, dir, "rev-parse", tag+"^{}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", tag, err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

// Fetch brings one commit into the clone. ref is always a SHA: naming a branch
// would let a depth-1 fetch land on a commit the clone already holds, which
// makes that commit a shallow boundary and costs every tag behind it its history
// at the next gc.
func (g execGit) Fetch(ctx context.Context, dir, ref string) error {
	if _, errb, err := g.run(ctx, dir, "fetch", "--no-tags", "--depth", "1", "origin", ref); err != nil {
		return fmt.Errorf("git fetch %s: %w: %s", ref, err, strings.TrimSpace(errb))
	}
	return nil
}

// HasCommit reports whether the clone already holds a commit. A non-zero exit is
// the answer "no" rather than a failure, so only git failing to run at all is an
// error.
func (g execGit) HasCommit(ctx context.Context, dir, sha string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "cat-file", "-e", sha+"^{commit}")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("git cat-file -e %s: %w: %s", sha, err, strings.TrimSpace(errb.String()))
}

// IsAncestor reports whether a is an ancestor of b.
//
// ok is false when the clone cannot answer. A shallow clone holds one commit and
// no ancestry, so merge-base would report "not an ancestor" for commits that are
// perfectly ordinary descendants: a confident wrong answer, which is worse than
// none. The caller asks GitHub instead.
func (g execGit) IsAncestor(ctx context.Context, dir, a, b string) (bool, bool, error) {
	shallow, err := g.IsShallow(ctx, dir)
	if err != nil {
		return false, false, err
	}
	if shallow {
		return false, false, nil
	}
	for _, sha := range []string{a, b} {
		has, hasErr := g.HasCommit(ctx, dir, sha)
		if hasErr != nil {
			return false, false, hasErr
		}
		if !has {
			return false, false, nil
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", a, b)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return true, true, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && ee.ExitCode() == 1 {
		return false, true, nil
	}
	return false, false, fmt.Errorf("git merge-base --is-ancestor: %w: %s", runErr, strings.TrimSpace(stderr.String()))
}

// Checkout moves HEAD to ref. --no-overwrite-ignore is what keeps a grader's
// output safe: git does not list an ignored file in `status --porcelain`, and
// without the flag it silently replaces one whose path the target commit tracks
// (V15). With it, and for untracked files by default (V14), git refuses and
// changes nothing.
func (g execGit) Checkout(ctx context.Context, dir, ref string) error {
	if _, errb, err := g.run(ctx, dir, "checkout", "--no-overwrite-ignore", "--detach", ref); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", ref, err, strings.TrimSpace(errb))
	}
	return nil
}

// filesInTheWay reports whether a checkout failed because the worktree holds
// files the target commit tracks, which git refuses rather than overwrite.
//
// This reads git's message, which the design rightly distrusts for deciding
// facts (it is how the old force-push warning went wrong). Here it only chooses
// which report the instructor sees: either way the checkout failed and the
// worktree is untouched, so a misread costs a less specific message and nothing
// else. The runner pins LC_ALL=C, so the wording is stable English.
func filesInTheWay(err error) bool {
	return err != nil && strings.Contains(err.Error(), "would be overwritten by checkout")
}

// CurrentBranch reports the branch HEAD is on. A detached HEAD is not an error
// here, it is simply no branch: symbolic-ref --quiet exits non-zero and says
// nothing, which is how the two are told apart.
func (g execGit) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, errb, err := g.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if strings.TrimSpace(errb) == "" {
			return "", nil
		}
		return "", fmt.Errorf("git symbolic-ref HEAD: %w: %s", err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

func (execGit) MoveIntoPlace(_ context.Context, staging, dir string) error {
	return renameNoReplace(staging, dir)
}

func (g execGit) DeleteBranch(ctx context.Context, dir, branch string) error {
	if _, errb, err := g.run(ctx, dir, "branch", "--delete", "--force", branch); err != nil {
		return fmt.Errorf("git branch -D %s: %w: %s", branch, err, strings.TrimSpace(errb))
	}
	return nil
}

func (g execGit) SetConfig(ctx context.Context, dir, key, value string) error {
	if _, errb, err := g.run(ctx, dir, "config", key, value); err != nil {
		return fmt.Errorf("git config %s %s: %w: %s", key, value, err, strings.TrimSpace(errb))
	}
	return nil
}

// CheckRefFormat asks git whether ref is a usable ref name. It runs outside any
// clone, since the answer does not depend on one. check-ref-format exits 1 for a
// name it rejects, which is an answer; any other failure means git could not be
// asked and is reported as an error.
func (execGit) CheckRefFormat(ctx context.Context, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", ref)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ref-format %s: %w: %s", ref, err, strings.TrimSpace(errb.String()))
}

func (g execGit) CreateTag(ctx context.Context, dir, tag, sha string) error {
	if _, errb, err := g.run(ctx, dir, "tag", tag, sha); err != nil {
		return fmt.Errorf("git tag %s: %w: %s", tag, err, strings.TrimSpace(errb))
	}
	return nil
}
