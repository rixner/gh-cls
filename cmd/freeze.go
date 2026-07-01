package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// freezeClient is the narrow set of GitHub operations freeze needs.
type freezeClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error
}

// freezeOpts carries the resolved flags and dependencies for `gh cls freeze`.
type freezeOpts struct {
	g         *globalOpts
	undo      bool
	dryRun    bool
	newClient func(context.Context) (freezeClient, error)
}

func newFreezeCmd(g *globalOpts) *cobra.Command {
	o := &freezeOpts{
		g:         g,
		newClient: func(context.Context) (freezeClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "freeze <name> [key...]",
		Short: "Freeze (or unfreeze) an assignment's repositories",
		Long: `Downgrade every non-admin direct collaborator on the <name>-* repos from
write to read, a hard repo-wide deadline freeze. --undo restores push. The
operation reads each repo's current collaborators and never consults the
roster, so a drifted roster cannot let a student escape the freeze.

Naming one or more student/team keys restricts the operation to just those
repos (<name>-<key>), for granting or ending an individual extension: freeze
the whole assignment at the deadline, then --undo one student's repo for an
extension and re-freeze it when the extension expires. Keys match repo names
case-insensitively; if any named key has no repo the run aborts before touching
anything.`,
		Example: `  gh cls freeze hw1
  gh cls freeze hw1 --undo
  gh cls freeze hw1 alice --undo
  gh cls freeze hw1 alice`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0], args[1:])
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&o.undo, "undo", "u", false, "reverse a freeze: restore push to non-admin direct collaborators")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would change without doing it")
	return cmd
}

// selectRepos narrows repos to the ones named by keys, matching <name>-<key>
// case-insensitively (as collect does). Every key must resolve to exactly one
// listed repo; any that does not aborts the whole run with the missing keys
// named, so the operation changes nothing on a typo or wrong assignment.
func selectRepos(name, org string, repos []gh.Repo, keys []string) ([]gh.Repo, error) {
	byKey := make(map[string]gh.Repo, len(repos))
	for _, r := range repos {
		lkey := strings.ToLower(strings.TrimPrefix(r.Name, name+"-"))
		byKey[lkey] = r
	}
	var selected []gh.Repo
	var missing []string
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		lkey := strings.ToLower(k)
		if seen[lkey] {
			continue // a key repeated on the command line is one repo
		}
		seen[lkey] = true
		r, ok := byKey[lkey]
		if !ok {
			missing = append(missing, k)
			continue
		}
		selected = append(selected, r)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no repositories named %s-%s in %s; check the key(s) and assignment name", name, strings.Join(missing, ", "+name+"-"), org)
	}
	return selected, nil
}

// freezeResult records how many collaborators changed on one repo.
type freezeResult struct {
	repo    string
	changed int
	err     error
}

func (o *freezeOpts) run(ctx context.Context, out io.Writer, name string, keys []string) error {
	org := o.g.org

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	all, err := client.ListOrgReposByPrefix(ctx, org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w", name, err)
	}
	// A template repository can match the <name>-* prefix (e.g. hw1-template) but
	// is not student work — never freeze it. Skipping every template repo keeps
	// freeze decoupled from which template an assignment names.
	var repos []gh.Repo
	for _, r := range all {
		if !r.IsTemplate {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 {
		// At a deadline, zero matches almost always means a mistyped assignment
		// name or the wrong config — not "nothing to do". Fail loudly so a freeze
		// is never silently a no-op.
		return fmt.Errorf("no student repositories named %s-* found in %s; check the assignment name and your config's org", name, org)
	}

	// Restricting to named keys is an individual-extension operation. Resolve the
	// keys to repos and abort before any mutation if one has no repo, so a
	// mistyped key never silently freezes (or spares) nothing.
	if len(keys) > 0 {
		repos, err = selectRepos(name, org, repos, keys)
		if err != nil {
			return err
		}
	}

	verb := "Freezing"
	if o.undo {
		verb = "Unfreezing"
	}
	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%s%s %d repo(s) in %s\n", prefix, verb, len(repos), org)

	results := runConcurrent(ctx, o.g.concurrency, repos, func(ctx context.Context, r gh.Repo) freezeResult {
		return o.processRepo(ctx, client, org, r.Name)
	})
	return reportFreeze(out, o.dryRun, results)
}

// processRepo downgrades (or restores) one repo's non-admin direct
// collaborators. Admins are always left untouched.
func (o *freezeOpts) processRepo(ctx context.Context, client freezeClient, org, repo string) freezeResult {
	res := freezeResult{repo: repo}
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("listing collaborators of %s: %w", repo, err)
		return res
	}
	for _, c := range collaborators {
		if c.Permissions.Admin {
			continue // staff/instructor keep access through the freeze
		}
		target := o.target(c)
		if target == "" {
			continue
		}
		res.changed++
		if o.dryRun {
			continue
		}
		if err := client.AddCollaborator(ctx, org, repo, c.Login, target); err != nil {
			res.err = fmt.Errorf("setting %s on %s: %w", c.Login, repo, err)
			return res
		}
	}

	// Post-condition: re-read and confirm the gate actually moved. The freeze is
	// the deadline lock, so it is never reported done on the strength of the write
	// call alone — a 200 is not proof the permission changed.
	if !o.dryRun {
		if err := o.verifyResult(ctx, client, org, repo); err != nil {
			res.err = err
			return res
		}
	}
	return res
}

// verifyResult re-reads a repo's direct collaborators and confirms the end state
// the operation intended: after a freeze no non-admin retains write; after an
// undo every non-admin holds push.
func (o *freezeOpts) verifyResult(ctx context.Context, client freezeClient, org, repo string) error {
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s after the change: %w", repo, err)
	}
	for _, c := range collaborators {
		if c.Permissions.Admin {
			continue
		}
		if o.undo {
			if !c.Permissions.Push {
				return fmt.Errorf("unfreeze of %s did not take: %s still lacks push", repo, c.Login)
			}
		} else if c.Permissions.Push || c.Permissions.Maintain || c.Permissions.Triage {
			return fmt.Errorf("freeze of %s did not take: %s still has write access", repo, c.Login)
		}
	}
	return nil
}

// target returns the permission to set for a non-admin collaborator, or "" to
// leave them unchanged. Freeze downgrades write access to read; undo restores
// push to anyone not already holding it.
func (o *freezeOpts) target(c gh.Collaborator) string {
	if o.undo {
		if c.Permissions.Push {
			return "" // already restored
		}
		return "push"
	}
	if c.Permissions.Push || c.Permissions.Maintain || c.Permissions.Triage {
		return "pull"
	}
	return ""
}

// reportFreeze summarizes the run and returns an error if any repo failed.
func reportFreeze(out io.Writer, dryRun bool, results []freezeResult) error {
	var changed, failed int
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		changed += r.changed
	}
	word := "changed"
	if dryRun {
		word = "would change"
	}
	fmt.Fprintf(out, "%s %d collaborator grant(s) across %d repo(s)\n", word, changed, len(results)-failed)
	if failed > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
			}
		}
		return fmt.Errorf("%d repo(s) failed", failed)
	}
	return nil
}
