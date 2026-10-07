package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// archiveClient is the narrow set of GitHub operations archive needs.
type archiveClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	ArchiveRepo(ctx context.Context, owner, name string) error
}

// archiveOpts carries the resolved flags and dependencies for `gh cls archive`.
type archiveOpts struct {
	g         *globalOpts
	all       bool
	dryRun    bool
	newClient func(context.Context) (archiveClient, error)
}

func newArchiveCmd(g *globalOpts) *cobra.Command {
	o := &archiveOpts{
		g:         g,
		newClient: func(context.Context) (archiveClient, error) { return g.client() },
	}
	cmd := &cobra.Command{
		Use:   "archive (<name>... | --all)",
		Short: "Archive the student repositories of one or more assignments",
		Long: `Archive every student repository of the named assignments, or of every
assignment in the config with --all, typically at the end of the semester. An
archived repository is read-only for everyone, staff and admins included, but
everyone who could read it still can, so students keep their work.

Every assignment's <name>-* repos are listed before the first one is archived,
so a typo or an unreadable listing stops the run before it changes anything.
Template repositories are skipped, as everywhere else. A repo already archived
is left alone, so a run that stopped partway is finished by re-running it, and
each repo is re-read afterwards to confirm it is archived.

Archiving is undone per repository in its settings on GitHub. While a repo is
archived nothing can be pushed to it and no issue, pull request or comment can
be added, so feedback cannot post to it; unarchive it first.`,
		Example: `  gh cls archive --all -n
  gh cls archive --all
  gh cls archive hw1 project`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.all, "all", false, "archive the repos of every assignment in the config")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would be archived without doing it")
	return cmd
}

// archiveResult records the outcome of archiving one repo.
type archiveResult struct {
	repo string
	err  error
}

func (o *archiveOpts) run(ctx context.Context, out io.Writer, names []string) error {
	org := o.g.org
	// Archiving the whole course is never the default: with nothing named, the run
	// asks for --all rather than guessing, and naming both is ambiguous.
	switch {
	case o.all && len(names) > 0:
		return fmt.Errorf("name assignments or pass --all, not both")
	case !o.all && len(names) == 0:
		return fmt.Errorf("name the assignment(s) to archive, or pass --all to archive every assignment in the config")
	case o.all:
		for n := range o.g.cfg.Assignments {
			names = append(names, n)
		}
		sort.Strings(names)
	default:
		for _, n := range names {
			if _, ok := o.g.cfg.Assignments[n]; !ok {
				return fmt.Errorf("assignment %q not found in config; nothing was archived", n)
			}
		}
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// Every listing is read before anything is archived, so a failed listing or a
	// named assignment with no repos stops the run while it has changed nothing.
	var pending, empty []string
	already := 0
	for _, n := range names {
		repos, err := client.ListOrgReposByPrefix(ctx, org, n+"-")
		if err != nil {
			return fmt.Errorf("listing %s-* repositories: %w; nothing was archived", n, err)
		}
		found := 0
		for _, r := range filterAssignmentRepos(o.g.cfg, n, repos) {
			if r.IsTemplate {
				continue
			}
			found++
			if r.Archived {
				already++
				continue
			}
			pending = append(pending, r.Name)
		}
		if found == 0 {
			empty = append(empty, n)
		}
	}
	// A named assignment with no repos is almost always a typo or the wrong
	// config. Under --all it is just an assignment not yet handed out.
	if len(empty) > 0 && !o.all {
		return fmt.Errorf("no student repositories found for %s in %s; check the assignment name and your config's org; nothing was archived", strings.Join(empty, ", "), org)
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sArchiving %d repo(s) across %d assignment(s) in %s\n", prefix, len(pending), len(names), org)
	if already > 0 {
		fmt.Fprintf(out, "note: %d repo(s) already archived, left alone\n", already)
	}
	if len(empty) > 0 {
		fmt.Fprintf(out, "note: no repos for %s\n", strings.Join(empty, ", "))
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "nothing to archive")
		return nil
	}

	acted := "archived"
	if o.dryRun {
		acted = "would archive"
	}
	prog := newProgress(out, len(pending), len("would archive"))
	results := runConcurrentProgress(ctx, o.g.concurrency, pending, func(ctx context.Context, repo string) archiveResult {
		if o.dryRun {
			return archiveResult{repo: repo}
		}
		return archiveResult{repo: repo, err: archiveOne(ctx, client, org, repo)}
	}, func(r archiveResult) { prog.item(failedOr(r.err, acted), r.repo) })
	return reportArchive(out, acted, results)
}

// archiveOne archives one repo and confirms it took: a success response is not
// proof, so the repo is re-read.
func archiveOne(ctx context.Context, client archiveClient, org, repo string) error {
	if err := client.ArchiveRepo(ctx, org, repo); err != nil {
		return fmt.Errorf("archiving %s: %w", repo, err)
	}
	r, ok, err := client.GetRepo(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s is archived: %w", repo, err)
	}
	if !ok {
		return fmt.Errorf("verifying %s is archived: the repository is gone", repo)
	}
	if !r.Archived {
		return fmt.Errorf("archiving %s did not take: it still reads as not archived; re-run `gh cls archive`", repo)
	}
	return nil
}

// reportArchive summarizes the run and returns an error if any repo failed.
func reportArchive(out io.Writer, acted string, results []archiveResult) error {
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}
	fmt.Fprintf(out, "%s %d repo(s), %d failed\n", acted, len(results)-failed, failed)
	if failed > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
			}
		}
		return fmt.Errorf("%d repo(s) failed to archive; re-run `gh cls archive` to finish, since repos already archived are skipped", failed)
	}
	return nil
}
