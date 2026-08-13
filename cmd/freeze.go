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
	ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
	UpdateRepoInvitation(ctx context.Context, owner, repo string, id int64, permission string) error
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	GetRepoPropertyValues(ctx context.Context, org, repo string) (map[string]string, error)
	SetRepoPropertyValue(ctx context.Context, org, repo, name, value string) error
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

Pending invitations are downgraded too. A student who has not yet accepted is
not a collaborator, but their invitation carries the write access it was issued
with; leaving it alone would let them accept after the deadline and push.
--undo restores those to write. Expired invitations are left alone, since they
can no longer be accepted (use `+"`gh cls audit --renew`"+` to re-issue one).

Naming one or more student/team keys restricts the operation to just those
repos (<name>-<key>), for granting or ending an individual extension: freeze
the whole assignment at the deadline, then --undo one student's repo for an
extension and re-freeze it when the extension expires. Keys match repo names
case-insensitively; if any named key has no repo the run aborts before touching
anything.

--undo is not a true inverse: freeze stores no prior state, so it grants push
to every non-admin direct collaborator, including any who were deliberately
read-only before the freeze.`,
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

// freezeResult records how many collaborators and pending invitations changed on
// one repo. The two are counted separately: a changed invitation is a student who
// still has to accept before it means anything, which reads differently in the
// summary from a grant that took effect immediately.
type freezeResult struct {
	repo    string
	changed int
	invites int
	err     error
}

func (o *freezeOpts) run(ctx context.Context, out io.Writer, name string, keys []string) error {
	org := o.g.org

	if _, ok := o.g.cfg.Assignments[name]; !ok {
		return fmt.Errorf("assignment %q not found in config", name)
	}

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
	all = filterAssignmentRepos(o.g.cfg, name, all)
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

	// Pre-condition: the property freeze records its state in must already exist.
	// Checked here, before the first repo is touched, so an org that never ran
	// setup fails with one clear message instead of freezing some repos and then
	// failing to record any of it.
	if _, ok, err := client.GetPropertyDefinition(ctx, org, frozenProperty); err != nil {
		return fmt.Errorf("checking the %q organization property on %s: %w", frozenProperty, org, err)
	} else if !ok {
		return fmt.Errorf("the %q organization property does not exist on %s, so this freeze could not be recorded and `gh cls audit --renew` would later re-grant write; run `gh cls setup` first", frozenProperty, org)
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
	return reportFreeze(out, o.dryRun, o.undo, results)
}

// processRepo downgrades (or restores) one repo's non-admin direct collaborators
// and its pending invitations. Admins are always left untouched.
func (o *freezeOpts) processRepo(ctx context.Context, client freezeClient, org, repo string) freezeResult {
	res := freezeResult{repo: repo}

	// The record is written before a freeze takes access away, and after an undo
	// gives it back. Both orderings leave the same safe intermediate state if the
	// run dies midway: recorded frozen while still writable. That way a later
	// `audit --renew` withholds write from a repo whose lock is incomplete, rather
	// than handing out write on a repo that is already locked.
	if !o.dryRun && !o.undo {
		if err := o.record(ctx, client, org, repo, freezeFrozen); err != nil {
			res.err = err
			return res
		}
	}

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

	// A student who has not accepted yet holds no access, so they are not in the
	// list above -- but their invitation still carries write, and accepting it
	// after the deadline would grant push. Downgrade the invitation itself, or the
	// freeze has a hole exactly where the least-engaged students are.
	invitations, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("listing pending invitations of %s: %w", repo, err)
		return res
	}
	for _, inv := range invitations {
		target := o.inviteTarget(inv)
		if target == "" {
			continue
		}
		res.invites++
		if o.dryRun {
			continue
		}
		if err := client.UpdateRepoInvitation(ctx, org, repo, inv.ID, target); err != nil {
			res.err = fmt.Errorf("setting %s's pending invitation on %s to %s: %w", inv.Invitee.Login, repo, target, err)
			return res
		}
	}

	if !o.dryRun && o.undo {
		if err := o.record(ctx, client, org, repo, freezeThawed); err != nil {
			res.err = err
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

// verifyResult re-reads a repo's direct collaborators and pending invitations and
// confirms the end state the operation intended: after a freeze neither a
// non-admin nor an acceptable invitation retains write; after an undo every
// non-admin holds push and every live invitation confers write again.
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
		} else if c.AboveRead() {
			return fmt.Errorf("freeze of %s did not take: %s still has write access", repo, c.Login)
		}
	}

	invitations, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying pending invitations of %s after the change: %w", repo, err)
	}
	for _, inv := range invitations {
		if inv.Expired {
			continue // cannot be accepted, so it grants nothing either way
		}
		if o.undo {
			if !inv.ConfersPush() {
				return fmt.Errorf("unfreeze of %s did not take: %s's pending invitation still lacks write", repo, inv.Invitee.Login)
			}
		} else if inv.AboveRead() {
			return fmt.Errorf("freeze of %s did not take: %s's pending invitation still confers write, so accepting it after the deadline would grant push", repo, inv.Invitee.Login)
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
	if c.AboveRead() {
		return "pull"
	}
	return ""
}

// record writes a repo's freeze state to the organization custom property and
// confirms it read back. The record is what audit --renew consults to decide
// whether to restore read or write, so a write that silently did not take would
// reopen the assignment for exactly the students renew touches.
func (o *freezeOpts) record(ctx context.Context, client freezeClient, org, repo string, want freezeState) error {
	if err := client.SetRepoPropertyValue(ctx, org, repo, frozenProperty, string(want)); err != nil {
		return fmt.Errorf("recording %s as %s on %s: %w", repo, want.describe(), frozenProperty, err)
	}
	values, err := client.GetRepoPropertyValues(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying the %s record on %s: %w", frozenProperty, repo, err)
	}
	if got := freezeState(values[frozenProperty]); got != want {
		return fmt.Errorf("the %s record on %s reads %q after being set to %q; `gh cls audit --renew` would restore the wrong access, so fix the property before relying on this freeze", frozenProperty, repo, got.describe(), want.describe())
	}
	return nil
}

// inviteTarget returns the permission to set on a pending invitation, or "" to
// leave it unchanged. It mirrors target over the invitation vocabulary. An
// expired invitation is always left alone: it can no longer be accepted, so it
// is not a way past a freeze and there is nothing for undo to restore.
func (o *freezeOpts) inviteTarget(i gh.Invitation) string {
	if i.Expired {
		return ""
	}
	if o.undo {
		if i.ConfersPush() {
			return "" // already restored
		}
		return gh.InvitationWrite
	}
	if i.AboveRead() {
		return gh.InvitationRead
	}
	return ""
}

// reportFreeze summarizes the run and returns an error if any repo failed.
func reportFreeze(out io.Writer, dryRun, undo bool, results []freezeResult) error {
	var changed, invites, failed int
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		changed += r.changed
		invites += r.invites
	}
	word := "changed"
	if dryRun {
		word = "would change"
	}
	fmt.Fprintf(out, "%s %d collaborator grant(s) across %d repo(s)\n", word, changed, len(results)-failed)
	// Called out separately: these students hold no access yet, so the line reports
	// what they will get when they accept, not a change they can already see.
	if invites > 0 {
		state := "downgraded to read, so accepting after the deadline cannot push"
		if undo {
			state = "restored to write"
		}
		fmt.Fprintf(out, "  plus %d pending invitation(s) %s\n", invites, state)
	}
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
