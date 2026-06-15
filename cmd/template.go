package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// templateClient is the narrow set of GitHub operations template needs.
type templateClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	SetRepoTemplate(ctx context.Context, owner, name string) error
	GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	DeleteRepo(ctx context.Context, org, name string) error
}

// templateOpts carries the resolved flags and dependencies for `gh cls template`.
type templateOpts struct {
	g         *globalOpts
	source    string
	force     bool
	dryRun    bool
	newClient func(context.Context) (templateClient, error)
}

func newTemplateCmd(g *globalOpts) *cobra.Command {
	o := &templateOpts{
		g:         g,
		newClient: func(context.Context) (templateClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "template <name>",
		Short: "Prepare a single-commit template for an assignment",
		Long: `Derive a single-commit template repo (<name>-template) in the semester org
from the maintained source template, so the source template's development
history is never exposed to students. Run once per assignment, before assign.

The derived template is produced through GitHub's own template generation, which
copies the source's files as one fresh commit without its history. This uses only
the GitHub API — no local clone, no git binary, and no separate git credentials.`,
		Example: "  gh cls template hw1",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.source, "template", "t", "", "source template repo (owner/name), overriding config")
	f.BoolVarP(&o.force, "force", "F", false, "overwrite an existing <name>-template")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "describe what would be created without doing it")
	return cmd
}

func (o *templateOpts) run(ctx context.Context, out io.Writer, name string) error {
	cfg, _, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	org, err := resolveOrg(o.g, cfg)
	if err != nil {
		return err
	}

	source := o.source
	if source == "" {
		if a, ok := cfg.Assignments[name]; ok {
			source = a.Template
		}
	}
	if source == "" {
		return fmt.Errorf("no source template for %q: set assignments.%s.template or pass --template", name, name)
	}
	srcOwner, srcName, err := splitRepo(source)
	if err != nil {
		return err
	}

	derived := name + "-template"

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN — no changes will be made\n\n")
		fmt.Fprintf(out, "Would derive %s/%s from %s:\n", org, derived, source)
		if o.force {
			fmt.Fprintf(out, "  - overwrite %s/%s if it already exists\n", org, derived)
		}
		fmt.Fprintf(out, "  - generate a private %s/%s from %s as a single commit (no source history)\n", org, derived, source)
		fmt.Fprintf(out, "  - mark %s/%s as a template repository\n", org, derived)
		return nil
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// Verify the source exists before anything destructive happens: with --force
	// the existing template is deleted below, and a good template must never be
	// destroyed only to discover the source it would be rebuilt from is gone.
	srcRepo, exists, err := client.GetRepo(ctx, srcOwner, srcName)
	if err != nil {
		return fmt.Errorf("reading source template %s: %w", source, err)
	}
	if !exists {
		return fmt.Errorf("source template %s not found", source)
	}

	// Generating a repository from another requires the source to be marked a
	// template repository. The source is one by role, so ensure the flag is set;
	// this is a no-op when it already is.
	if !srcRepo.IsTemplate {
		if err := client.SetRepoTemplate(ctx, srcOwner, srcName); err != nil {
			return fmt.Errorf("marking source %s as a template repository (required to generate from it): %w", source, err)
		}
	}

	if _, exists, err := client.GetRepo(ctx, org, derived); err != nil {
		return fmt.Errorf("checking for existing %s/%s: %w", org, derived, err)
	} else if exists {
		if !o.force {
			return fmt.Errorf("%s/%s already exists; pass -F/--force to overwrite", org, derived)
		}
		if err := client.DeleteRepo(ctx, org, derived); err != nil {
			return fmt.Errorf("deleting existing %s/%s: %w", org, derived, err)
		}
	}

	// Template generation copies the source's files as a single fresh commit on
	// its default branch, exposing none of the source's history.
	if err := client.GenerateFromTemplate(ctx, srcOwner, srcName, org, derived, true, false); err != nil {
		return fmt.Errorf("generating %s/%s from %s: %w", org, derived, source, err)
	}

	if err := client.SetRepoTemplate(ctx, org, derived); err != nil {
		return fmt.Errorf("marking %s/%s as a template: %w", org, derived, err)
	}

	fmt.Fprintf(out, "Created %s/%s — single commit generated from %s, marked as a template repository.\n", org, derived, source)
	return nil
}

// splitRepo parses an "owner/name" reference.
func splitRepo(ref string) (owner, name string, err error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repository %q: want owner/name", ref)
	}
	return parts[0], parts[1], nil
}
