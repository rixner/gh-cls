package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/git"
	"github.com/spf13/cobra"
)

// defaultSquashMessage is the commit message of the single flattened commit in
// a derived template, used unless -m overrides it.
const defaultSquashMessage = "Provided starter code"

// templateClient is the narrow set of GitHub operations template needs.
type templateClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	CreateOrgRepo(ctx context.Context, org, name string, private bool) (*gh.Repo, error)
	SetRepoTemplate(ctx context.Context, org, name string) error
	DeleteRepo(ctx context.Context, org, name string) error
}

// squashFunc flattens a source repo into a single-commit push to pushURL.
type squashFunc func(ctx context.Context, srcURL, pushURL, message string) (string, error)

// templateOpts carries the resolved flags and dependencies for `gh cls template`.
type templateOpts struct {
	g         *globalOpts
	source    string
	message   string
	force     bool
	dryRun    bool
	newClient func(context.Context) (templateClient, error)
	squash    squashFunc
}

func newTemplateCmd(g *globalOpts) *cobra.Command {
	o := &templateOpts{
		g:         g,
		newClient: func(context.Context) (templateClient, error) { return gh.New() },
		squash:    git.Squash,
	}
	cmd := &cobra.Command{
		Use:   "template <name>",
		Short: "Prepare a squashed template for an assignment",
		Long: `Derive a single-commit template repo (<name>-template) in the semester org
from the maintained source template, so the source template's development
history is never exposed to students. Run once per assignment, before assign.

Requires git on PATH; clone and push use your existing git credentials.`,
		Example: "  gh cls template hw1",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.source, "template", "t", "", "source template repo (owner/name), overriding config")
	f.StringVarP(&o.message, "message", "m", defaultSquashMessage, "commit message for the flattened commit")
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
		fmt.Fprintf(out, "  - shallow-clone %s and flatten to one commit (%q)\n", source, o.message)
		if o.force {
			fmt.Fprintf(out, "  - overwrite %s/%s if it already exists\n", org, derived)
		}
		fmt.Fprintf(out, "  - create private %s/%s and mark it a template repository\n", org, derived)
		return nil
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
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

	srcRepo, exists, err := client.GetRepo(ctx, srcOwner, srcName)
	if err != nil {
		return fmt.Errorf("reading source template %s: %w", source, err)
	}
	if !exists {
		return fmt.Errorf("source template %s not found", source)
	}

	newRepo, err := client.CreateOrgRepo(ctx, org, derived, true)
	if err != nil {
		return fmt.Errorf("creating %s/%s: %w", org, derived, err)
	}

	branch, err := o.squash(ctx, srcRepo.CloneURL, newRepo.CloneURL, o.message)
	if err != nil {
		return fmt.Errorf("squashing template: %w", err)
	}

	if err := client.SetRepoTemplate(ctx, org, derived); err != nil {
		return fmt.Errorf("marking %s/%s as a template: %w", org, derived, err)
	}

	fmt.Fprintf(out, "Created %s/%s — single commit on %s, marked as a template repository.\n", org, derived, branch)
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
