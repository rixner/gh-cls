package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// templateClient is the narrow set of GitHub operations template needs.
type templateClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	SetRepoTemplate(ctx context.Context, owner, name string) error
	GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	DeleteRepo(ctx context.Context, org, name string) error
}

// templateOpts carries the resolved flags and dependencies for `gh cls template`.
type templateOpts struct {
	g          *globalOpts
	source     string
	markSource bool
	force      bool
	dryRun     bool
	newClient  func(context.Context) (templateClient, error)
	sleep      func(time.Duration)
}

func newTemplateCmd(g *globalOpts) *cobra.Command {
	o := &templateOpts{
		g:         g,
		newClient: func(context.Context) (templateClient, error) { return gh.New() },
		sleep:     time.Sleep,
	}
	cmd := &cobra.Command{
		Use:   "template <repo>",
		Short: "Build a squashed, single-commit template repository",
		Long: `Create <repo> as a single-commit copy of a source repository, with none of
the source's history, and mark it a template repository so assign can generate
student repos from it. A bare <repo> is created in the configured org; pass
owner/name to create it elsewhere.

This is an optional helper: assign clones whatever template an assignment names,
so any existing template repository works just as well. Generation runs purely
against the GitHub API — no local clone, no git binary, no separate credentials.`,
		Example: "  gh cls template hw1-template --source cs101-staff/hw1-dev",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.source, "source", "s", "", "source repo to squash (owner/name, required)")
	f.BoolVar(&o.markSource, "mark-source", false, "mark the source a template repository if it is not already")
	f.BoolVarP(&o.force, "force", "F", false, "overwrite <repo> if it already exists")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "describe what would be created without doing it")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}

func (o *templateOpts) run(ctx context.Context, out io.Writer, repoArg string) error {
	// Output: the template repo to create. A bare name lands in the configured org.
	dstOwner, dstName, err := splitRepo(qualifyTemplate(repoArg, o.g.org))
	if err != nil {
		return err
	}
	dst := dstOwner + "/" + dstName

	// Source: the repo to squash. Always an explicit owner/name.
	srcOwner, srcName, err := splitRepo(o.source)
	if err != nil {
		return fmt.Errorf("--source: %w", err)
	}
	source := srcOwner + "/" + srcName

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN — no changes will be made\n\n")
		fmt.Fprintf(out, "Would create %s from %s:\n", dst, source)
		if o.force {
			fmt.Fprintf(out, "  - overwrite %s if it already exists\n", dst)
		}
		if o.markSource {
			fmt.Fprintf(out, "  - mark %s a template repository if it is not already\n", source)
		}
		fmt.Fprintf(out, "  - generate a private %s from %s as a single commit (no source history)\n", dst, source)
		fmt.Fprintf(out, "  - mark %s a template repository\n", dst)
		return nil
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, dstOwner); err != nil {
		return err
	}

	// Verify the source exists before anything destructive: with --force the
	// existing <repo> is deleted below, and it must never be destroyed only to
	// find the source it would be rebuilt from is gone.
	srcRepo, exists, err := client.GetRepo(ctx, srcOwner, srcName)
	if err != nil {
		return fmt.Errorf("reading source %s: %w", source, err)
	}
	if !exists {
		return fmt.Errorf("source %s not found", source)
	}

	// The source must have content to generate from.
	branch := srcRepo.DefaultBranch
	if branch == "" {
		return fmt.Errorf("source %s has no commits to generate from; add a commit first", source)
	}
	if ok, err := client.BranchExists(ctx, srcOwner, srcName, branch); err != nil {
		return fmt.Errorf("checking source %s for content: %w", source, err)
	} else if !ok {
		return fmt.Errorf("source %s has no commits on its default branch %q; add a commit first", source, branch)
	}

	// Generating from a repo requires it to be a template repository. We never
	// silently flip someone's source repo: it is a checked pre-condition, opted
	// into with --mark-source.
	if !srcRepo.IsTemplate {
		if !o.markSource {
			return fmt.Errorf("source %s is not a template repository; mark it in the GitHub UI, or re-run with --mark-source to set it", source)
		}
		if err := client.SetRepoTemplate(ctx, srcOwner, srcName); err != nil {
			return fmt.Errorf("marking source %s a template repository: %w", source, err)
		}
	}

	deletedExisting := false
	if _, exists, err := client.GetRepo(ctx, dstOwner, dstName); err != nil {
		return fmt.Errorf("checking for existing %s: %w", dst, err)
	} else if exists {
		if !o.force {
			return fmt.Errorf("%s already exists; pass -F/--force to overwrite", dst)
		}
		if err := client.DeleteRepo(ctx, dstOwner, dstName); err != nil {
			return fmt.Errorf("deleting existing %s: %w", dst, err)
		}
		deletedExisting = true
	}

	// Template generation copies the source's files as a single fresh commit on
	// its default branch, exposing none of the source's history.
	if err := client.GenerateFromTemplate(ctx, srcOwner, srcName, dstOwner, dstName, true, false); err != nil {
		if deletedExisting {
			// The previous repo was already deleted for --force and could not be
			// rebuilt: there is now no template at all. Say so loudly.
			return fmt.Errorf("generating %s from %s failed AFTER the previous %s was deleted for --force: it is now gone and could not be rebuilt; fix the cause and re-run: %w", dst, source, dst, err)
		}
		return fmt.Errorf("generating %s from %s: %w", dst, source, err)
	}

	// From here the new repo exists. If finishing it fails, it is unusable (empty,
	// or not actually a template), so roll it back rather than leave a broken one.
	if err := o.finishTemplate(ctx, client, dstOwner, dstName); err != nil {
		if delErr := client.DeleteRepo(ctx, dstOwner, dstName); delErr != nil {
			return fmt.Errorf("%w; additionally, rolling back %s failed — delete it manually before retrying: %v", err, dst, delErr)
		}
		return fmt.Errorf("%w (rolled back %s; re-run to try again)", err, dst)
	}

	fmt.Fprintf(out, "Created %s — single commit generated from %s, marked a template repository.\n", dst, source)
	return nil
}

// finishTemplate confirms a freshly generated repo is populated, marks it a
// template, and verifies that flag actually took. Each step is a post-condition
// assign depends on: it generates student repos from this template, which only
// works if the template has content and is itself marked a template repository.
func (o *templateOpts) finishTemplate(ctx context.Context, client templateClient, owner, name string) error {
	// Generation is asynchronous: wait until the repo's default branch lands so
	// the template is not marked usable while still empty.
	if _, err := waitRepoReady(ctx, client, o.sleep, owner, name); err != nil {
		return err
	}
	if err := client.SetRepoTemplate(ctx, owner, name); err != nil {
		return fmt.Errorf("marking %s/%s a template: %w", owner, name, err)
	}
	repo, exists, err := client.GetRepo(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("verifying %s/%s: %w", owner, name, err)
	}
	if !exists {
		return fmt.Errorf("%s/%s disappeared right after it was created", owner, name)
	}
	if !repo.Private {
		return fmt.Errorf("%s/%s was created public but must be private (starter code must not be world-readable)", owner, name)
	}
	if !repo.IsTemplate {
		return fmt.Errorf("%s/%s was not marked a template repository (the change did not take effect)", owner, name)
	}
	return nil
}

// qualifyTemplate gives a bare template name (no owner) the configured org, so
// "hw1-template" means "<org>/hw1-template" — the common in-org case. A reference
// that already names an owner ("owner/name") is returned unchanged, so a template
// may live in another org.
func qualifyTemplate(ref, org string) string {
	if strings.Contains(ref, "/") {
		return ref
	}
	return org + "/" + ref
}

// splitRepo parses an "owner/name" reference.
func splitRepo(ref string) (owner, name string, err error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repository %q: want owner/name", ref)
	}
	return parts[0], parts[1], nil
}
