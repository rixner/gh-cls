package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/unit"
	"github.com/spf13/cobra"
)

// thawClient is the narrow set of GitHub operations thaw needs.
type thawClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error
	ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
	UpdateRepoInvitation(ctx context.Context, owner, repo string, id int64, permission string) (bool, error)
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	GetRepoPropertyValues(ctx context.Context, org, repo string) (map[string]string, error)
	SetRepoPropertyValue(ctx context.Context, org, repo, name, value string) error
}

// thawOpts carries the resolved flags and dependencies for `gh cls thaw`.
type thawOpts struct {
	g         *globalOpts
	roster    string
	groups    string
	dryRun    bool
	newClient func(context.Context) (thawClient, error)
}

func newThawCmd(g *globalOpts) *cobra.Command {
	o := &thawOpts{
		g:         g,
		newClient: func(context.Context) (thawClient, error) { return g.client() },
	}
	cmd := &cobra.Command{
		Use:   "thaw <name> [key...]",
		Short: "Give write back to the students on an assignment's frozen repositories",
		Long: `Restore write access on the <name>-* repos after a freeze, for the students the
roster (and, for a group assignment, the groups file) puts on each repo, and
record each repo as thawed.

Thaw gives access, so unlike freeze it goes by the roster, as assign does. An
enrolled student holding read, or a pending read invitation, is raised to write.
No one else changes: a collaborator the roster does not name, a student the
roster marks as dropped, and admins all keep exactly what they have. A student
with no access at all is not invited; ` + "`gh cls audit --renew`" + ` re-issues it, and
because the repo is recorded thawed it grants write.

A repo whose key matches no roster or groups entry, or whose students have all
dropped, stays frozen and is listed.

Naming one or more student/group keys restricts the run to just those repos
(<name>-<key>), which is how an individual extension is granted: freeze the
whole assignment at the deadline, thaw one student's repo for an extension,
and freeze it again when the extension expires. Every named key must have a
repo and an enrolled student or group, or the run aborts before any change.`,
		Example: `  gh cls thaw hw1 --roster roster.csv
  gh cls thaw hw1 alice --roster roster.csv
  gh cls thaw project --roster roster.csv --groups groups.yml`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0], args[1:])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.groups, "groups", "g", "", "path to the groups file (required for group, rejected for individual)")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would change without doing it")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// thawTarget is one repo thaw acts on and the enrolled students it grants to.
type thawTarget struct {
	repo    string
	members []string
}

// thawResult records what thaw changed on one repo. noAccess counts enrolled
// students holding neither access nor a live invitation, which thaw does not
// issue and audit --renew does.
type thawResult struct {
	repo     string
	changed  int
	invites  int
	noAccess int
	err      error
}

func (o *thawOpts) run(ctx context.Context, out io.Writer, name string, keys []string) error {
	org := o.g.org
	policy, err := o.g.cfg.Resolve(name, config.Overrides{})
	if err != nil {
		return err
	}
	units, report, _, err := loadUnits(name, policy.Type, o.roster, o.groups)
	if err != nil {
		return err
	}
	printUnitWarnings(out, report)

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
	var repos []gh.Repo
	for _, r := range all {
		if !r.IsTemplate {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 {
		return fmt.Errorf("no student repositories named %s-* found in %s; check the assignment name and your config's org", name, org)
	}
	if len(keys) > 0 {
		if repos, err = selectRepos(name, org, repos, keys); err != nil {
			return err
		}
	}

	targets, leftFrozen := thawTargets(name, repos, units)
	// A named key is a deliberate extension, so one that cannot be granted is an
	// error rather than a line in the report: the instructor asked for write on
	// that repo and would otherwise believe they had given it.
	if len(keys) > 0 && len(leftFrozen) > 0 {
		return fmt.Errorf("cannot thaw %s: no enrolled student or group in the roster/groups is on it (a key in neither file, or students who all dropped); nothing was changed", strings.Join(leftFrozen, ", "))
	}
	if len(targets) == 0 {
		return fmt.Errorf("no %s-* repository belongs to an enrolled student or group in %s; check the roster and groups files", name, o.roster)
	}

	// Pre-condition, as in freeze: the property thaw records its state in must
	// exist before the first repo is touched.
	if _, ok, err := client.GetPropertyDefinition(ctx, org, frozenProperty); err != nil {
		return fmt.Errorf("checking the %q organization property on %s: %w", frozenProperty, org, err)
	} else if !ok {
		return fmt.Errorf("the %q organization property does not exist on %s, so this thaw could not be recorded; run `gh cls setup` first", frozenProperty, org)
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sThawing %d repo(s) in %s\n", prefix, len(targets), org)
	if len(leftFrozen) > 0 {
		fmt.Fprintf(out, "leaving %d repo(s) frozen, with no enrolled student or group on them:\n  %s\n", len(leftFrozen), strings.Join(leftFrozen, "\n  "))
	}

	acted := "thawed"
	if o.dryRun {
		acted = "would thaw"
	}
	prog := newProgress(out, len(targets), 10) // "would thaw"
	results := runConcurrentProgress(ctx, o.g.concurrency, targets, func(ctx context.Context, t thawTarget) thawResult {
		return o.processRepo(ctx, client, org, t)
	}, func(r thawResult) {
		outcome := acted
		if r.changed+r.invites == 0 {
			outcome = "no change"
		}
		prog.item(failedOr(r.err, outcome), r.repo)
	})
	return reportThaw(out, name, o.dryRun, results)
}

// thawTargets pairs each repo with the enrolled students of its unit. A repo
// with no unit (its key is in neither file) or whose unit has no enrolled
// student is returned in leftFrozen instead: thaw grants only to the students
// the roster puts on a repo, and those repos have none.
func thawTargets(name string, repos []gh.Repo, units []unit.Unit) (targets []thawTarget, leftFrozen []string) {
	byKey := make(map[string]unit.Unit, len(units))
	for _, u := range units {
		byKey[strings.ToLower(u.Key)] = u
	}
	for _, r := range repos {
		u, ok := byKey[strings.ToLower(strings.TrimPrefix(r.Name, name+"-"))]
		if !ok || len(u.Members) == 0 {
			leftFrozen = append(leftFrozen, r.Name)
			continue
		}
		targets = append(targets, thawTarget{repo: r.Name, members: u.Members})
	}
	return targets, leftFrozen
}

// processRepo raises one repo's enrolled students to write and records it
// thawed. Invitations go first, for the reason freeze gives: a student who
// accepts mid-run moves from the invitation list to the collaborator list, and
// the collaborator pass afterwards then governs them.
func (o *thawOpts) processRepo(ctx context.Context, client thawClient, org string, t thawTarget) thawResult {
	res := thawResult{repo: t.repo}
	enrolled := make(map[string]bool, len(t.members))
	for _, m := range t.members {
		enrolled[strings.ToLower(m)] = true
	}

	invitations, err := client.ListRepoInvitations(ctx, org, t.repo)
	if err != nil {
		res.err = fmt.Errorf("listing pending invitations of %s: %w", t.repo, err)
		return res
	}
	for _, inv := range invitations {
		if inv.Expired || inv.ConfersPush() || !enrolled[strings.ToLower(inv.Invitee.Login)] {
			continue
		}
		if o.dryRun {
			res.invites++
			continue
		}
		stillPending, err := client.UpdateRepoInvitation(ctx, org, t.repo, inv.ID, gh.InvitationWrite)
		if err != nil {
			res.err = fmt.Errorf("setting %s's pending invitation on %s to write: %w", inv.Invitee.Login, t.repo, err)
			return res
		}
		if stillPending {
			res.invites++
		}
	}

	collaborators, err := client.ListDirectCollaborators(ctx, org, t.repo)
	if err != nil {
		res.err = fmt.Errorf("listing collaborators of %s: %w", t.repo, err)
		return res
	}
	for _, c := range collaborators {
		if c.Permissions.Admin || c.CanPush() || !enrolled[strings.ToLower(c.Login)] {
			continue
		}
		res.changed++
		if o.dryRun {
			continue
		}
		if err := client.AddCollaborator(ctx, org, t.repo, c.Login, "push"); err != nil {
			res.err = fmt.Errorf("granting %s push on %s: %w", c.Login, t.repo, err)
			return res
		}
	}
	if o.dryRun {
		return res
	}

	// Recorded after access is given back, so a run that dies midway leaves the
	// repo recorded frozen while partly writable, the safe side: audit --renew
	// then withholds write rather than handing it out.
	if err := recordFreezeState(ctx, client, org, t.repo, freezeThawed); err != nil {
		res.err = err
		return res
	}
	res.noAccess, res.err = verifyThawed(ctx, client, org, t.repo, enrolled)
	return res
}

// verifyThawed is thaw's post-condition: every enrolled student who is a
// collaborator holds push and every live invitation of theirs confers write. It
// returns how many enrolled students hold neither, which is not a failure of the
// thaw but is reported so they can be renewed.
func verifyThawed(ctx context.Context, client thawClient, org, repo string, enrolled map[string]bool) (int, error) {
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return 0, fmt.Errorf("verifying %s after thawing: %w", repo, err)
	}
	seen := make(map[string]bool, len(enrolled))
	for _, c := range collaborators {
		l := strings.ToLower(c.Login)
		if !enrolled[l] {
			continue
		}
		seen[l] = true
		if !c.CanPush() {
			return 0, fmt.Errorf("thaw of %s did not take: %s still lacks push; re-run `gh cls thaw`", repo, c.Login)
		}
	}
	invitations, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return 0, fmt.Errorf("verifying pending invitations of %s after thawing: %w", repo, err)
	}
	for _, inv := range invitations {
		l := strings.ToLower(inv.Invitee.Login)
		if inv.Expired || !enrolled[l] {
			continue
		}
		seen[l] = true
		if !inv.ConfersPush() {
			return 0, fmt.Errorf("thaw of %s did not take: %s's pending invitation still lacks write; re-run `gh cls thaw`", repo, inv.Invitee.Login)
		}
	}
	return len(enrolled) - len(seen), nil
}

// reportThaw summarizes the run and returns an error if any repo failed.
func reportThaw(out io.Writer, name string, dryRun bool, results []thawResult) error {
	var changed, invites, noAccess, failed int
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		changed += r.changed
		invites += r.invites
		noAccess += r.noAccess
	}
	word := "changed"
	if dryRun {
		word = "would change"
	}
	fmt.Fprintf(out, "%s %d collaborator grant(s) across %d repo(s)\n", word, changed, len(results)-failed)
	if invites > 0 {
		fmt.Fprintf(out, "  plus %d pending invitation(s) restored to write\n", invites)
	}
	if noAccess > 0 {
		fmt.Fprintf(out, "note: %d enrolled student(s) hold no access or live invitation; re-issue it with `gh cls audit %s --roster <file> --renew`, which grants write on a thawed repo\n", noAccess, name)
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
