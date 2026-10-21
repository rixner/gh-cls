package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rixner/gh-cls/gh"
	"github.com/spf13/cobra"
)

// maxRepoNameLen is the longest repository name GitHub accepts.
const maxRepoNameLen = 100

// retractClient is the narrow set of GitHub operations retract needs.
type retractClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
	ListRepoActivity(ctx context.Context, owner, repo, ref string) ([]gh.Activity, error)
	RemoveCollaborator(ctx context.Context, owner, repo, username string) error
	DeleteRepoInvitation(ctx context.Context, owner, repo string, id int64) error
	RenameRepo(ctx context.Context, owner, name, newName string) error
}

// retractOpts carries the resolved flags and dependencies for `gh cls retract`.
type retractOpts struct {
	g         *globalOpts
	dryRun    bool
	now       func() time.Time
	newClient func(context.Context) (retractClient, error)
}

func newRetractCmd(g *globalOpts) *cobra.Command {
	o := &retractOpts{
		g:         g,
		now:       time.Now,
		newClient: func(context.Context) (retractClient, error) { return g.client() },
	}
	cmd := &cobra.Command{
		Use:   "retract <name>",
		Short: "Take back an assignment's repositories so it can be assigned again",
		Long: `Withdraw an assignment that was handed out wrong (the wrong template, say)
so that assign can create it again. Every <name>-* student repository loses its
student access and is renamed to <name>_<date>-<key>, which keeps it as a backup
of anything the students did while clearing the names for a fresh assign.

Students lose access: every direct collaborator except admins is removed and
every pending invitation is cancelled. Admins and the staff team keep their
access, so staff can still read and copy from a backup. The backup is not
archived, so a retract is undone by deleting the new repo, renaming the backup
back, and re-running assign.

Before anything changes, every repo is read: who has access, and who has pushed
to it, which is reported per repo so you know which backups hold student work
and whom to tell. A backup name that already exists, is too long, or would fall
in a configured assignment's <name>-* namespace stops the run before it has
changed anything. Each repo is re-read afterwards to confirm the students' access
is gone and the rename took.

A repo is renamed only after its students' access is removed, so a run that
stops partway leaves the rest under <name>-* with or without access, and re-running
retract finishes them. Do not run assign until retract has finished: it would
grant access to a repo still holding the old contents.

Students' clones still point at the old name. Once assign creates the new repo
there, pushes from an old clone are rejected as unrelated history, so tell the
students to clone again.`,
		Example: `  gh cls retract hw1 -n
  gh cls retract hw1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	cmd.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would be retracted and who has pushed, without changing anything")
	return cmd
}

// retractPlan is what retract will do to one repo, read before anything changes.
type retractPlan struct {
	repo   string
	backup string
	// students are the logins whose access is removed: non-admin direct
	// collaborators and pending invitees.
	students []string
	invited  map[string]bool
	// admins are direct collaborators holding admin, which retract leaves alone.
	admins []string
	// pushes counts the recorded ref changes the students made; last is the most
	// recent of them.
	pushes int
	last   gh.Activity
	err    error
}

// retractResult records the outcome of retracting one repo.
type retractResult struct {
	plan *retractPlan
	// accessRemoved records that the students' access was confirmed gone, so a
	// failure after it is reported as a rename left to finish.
	accessRemoved bool
	err           error
}

func (o *retractOpts) run(ctx context.Context, out io.Writer, name string) error {
	org := o.g.org
	if _, ok := o.g.cfg.Assignments[name]; !ok {
		return fmt.Errorf("assignment %q not found in config; nothing was retracted", name)
	}
	backupPrefix := name + "_" + o.now().Format("20060102") + "-"

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	listed, err := client.ListOrgReposByPrefix(ctx, org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w; nothing was retracted", name, err)
	}
	var plans []*retractPlan
	var problems []string
	for _, r := range filterAssignmentRepos(o.g.cfg, name, listed) {
		if r.IsTemplate {
			continue
		}
		if r.Archived {
			problems = append(problems, fmt.Sprintf("%s is archived, so its access cannot be changed; unarchive it in its settings on GitHub", r.Name))
			continue
		}
		backup := backupPrefix + strings.TrimPrefix(r.Name, name+"-")
		if err := o.checkBackupName(backup); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		plans = append(plans, &retractPlan{repo: r.Name, backup: backup})
	}
	if len(plans) == 0 && len(problems) == 0 {
		return fmt.Errorf("no student repositories found for %s in %s; check the assignment name and your config's org; nothing was retracted", name, org)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].repo < plans[j].repo })

	// Every repo is read before the first change, so a backup name already taken
	// or an unreadable repo stops the run while it has changed nothing.
	runConcurrent(ctx, o.g.concurrency, plans, func(ctx context.Context, p *retractPlan) struct{} {
		p.err = o.read(ctx, client, org, p)
		return struct{}{}
	})
	for _, p := range plans {
		if p.err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p.repo, p.err))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("aborting retract of %s; nothing was retracted:\n  %s", name, strings.Join(problems, "\n  "))
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sRetracting %d repo(s) of %s in %s, each renamed to %s<key>\n", prefix, len(plans), name, org, backupPrefix)
	printRetractPlan(out, plans)

	if o.dryRun {
		fmt.Fprintf(out, "[dry-run] would retract %d repo(s); nothing was changed\n", len(plans))
		return nil
	}

	prog := newProgress(out, len(plans), len("retracted"))
	results := runConcurrentProgress(ctx, o.g.concurrency, plans, func(ctx context.Context, p *retractPlan) retractResult {
		return retractOne(ctx, client, org, p)
	}, func(r retractResult) { prog.item(failedOr(r.err, "retracted"), r.plan.repo) })
	return reportRetract(out, name, backupPrefix, results)
}

// checkBackupName rejects a backup name GitHub would refuse or that some
// command would mistake for student work. A name inside any configured
// assignment's <name>-* namespace would be frozen, collected and archived as a
// student repo, and found by assign as an existing one.
func (o *retractOpts) checkBackupName(backup string) error {
	if len(backup) > maxRepoNameLen {
		return fmt.Errorf("the backup name %s is %d characters, over GitHub's limit of %d", backup, len(backup), maxRepoNameLen)
	}
	for other := range o.g.cfg.Assignments {
		if strings.HasPrefix(strings.ToLower(backup), strings.ToLower(other)+"-") {
			return fmt.Errorf("the backup name %s falls under assignment %q's %s-* repositories; rename that assignment in the config", backup, other, other)
		}
	}
	return nil
}

// read fills in a plan: that its backup name is free, who will lose access, and
// what the students pushed.
func (o *retractOpts) read(ctx context.Context, client retractClient, org string, p *retractPlan) error {
	if _, exists, err := client.GetRepo(ctx, org, p.backup); err != nil {
		return fmt.Errorf("checking whether the backup name %s is free: %w", p.backup, err)
	} else if exists {
		return fmt.Errorf("the backup name %s/%s is already taken (by an earlier retract today?); rename or delete that repo on GitHub and re-run", org, p.backup)
	}

	students := map[string]bool{}
	p.invited = map[string]bool{}
	invitations, err := client.ListRepoInvitations(ctx, org, p.repo)
	if err != nil {
		return fmt.Errorf("listing pending invitations: %w", err)
	}
	for _, inv := range invitations {
		students[strings.ToLower(inv.Invitee.Login)] = true
		p.students = append(p.students, inv.Invitee.Login)
		p.invited[inv.Invitee.Login] = true
	}
	collaborators, err := client.ListDirectCollaborators(ctx, org, p.repo)
	if err != nil {
		return fmt.Errorf("listing collaborators: %w", err)
	}
	for _, c := range collaborators {
		if c.Permissions.Admin {
			p.admins = append(p.admins, c.Login)
			continue
		}
		if !students[strings.ToLower(c.Login)] {
			students[strings.ToLower(c.Login)] = true
			p.students = append(p.students, c.Login)
		}
	}
	sort.Strings(p.students)
	sort.Strings(p.admins)

	// Only the students' changes count: assign itself moves refs when it builds a
	// feedback branch, and staff may push fixes, neither of which is work a
	// student would lose.
	activity, err := client.ListRepoActivity(ctx, org, p.repo, "")
	if err != nil {
		return fmt.Errorf("reading who has pushed: %w", err)
	}
	for _, a := range activity {
		if !students[strings.ToLower(a.Actor.Login)] {
			continue
		}
		if p.pushes == 0 || a.Timestamp.After(p.last.Timestamp) {
			p.last = a
		}
		p.pushes++
	}
	return nil
}

// printRetractPlan lists, per repo, its backup name, whose access goes, and
// whether any of them pushed.
func printRetractPlan(out io.Writer, plans []*retractPlan) {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  REPO\tBACKUP\tACCESS REMOVED\tSTUDENT PUSHES")
	for _, p := range plans {
		var who []string
		for _, s := range p.students {
			if p.invited[s] {
				s += " (invited)"
			}
			who = append(who, s)
		}
		access := strings.Join(who, ", ")
		if access == "" {
			access = "nobody"
		}
		pushes := "none"
		if p.pushes > 0 {
			pushes = fmt.Sprintf("%d, last %s by %s", p.pushes, p.last.Timestamp.Format(time.RFC3339), p.last.Actor.Login)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.repo, p.backup, access, pushes)
	}
	tw.Flush()
	for _, p := range plans {
		if len(p.admins) > 0 {
			fmt.Fprintf(out, "note: %s: %s hold(s) admin, which retract leaves alone, so they keep access to the backup\n", p.repo, strings.Join(p.admins, ", "))
		}
	}
}

// retractOne removes a repo's student access, confirms it is gone, and only
// then renames the repo to its backup name and confirms that took.
func retractOne(ctx context.Context, client retractClient, org string, p *retractPlan) retractResult {
	res := retractResult{plan: p}

	// Invitations go before collaborators, for the reason freeze gives: a student
	// accepting mid-run moves from the invitation list to the collaborator list,
	// and only this order is sure to catch them in one or the other.
	invitations, err := client.ListRepoInvitations(ctx, org, p.repo)
	if err != nil {
		res.err = fmt.Errorf("listing pending invitations of %s: %w", p.repo, err)
		return res
	}
	for _, inv := range invitations {
		if err := client.DeleteRepoInvitation(ctx, org, p.repo, inv.ID); err != nil && !gh.IsNotFound(err) {
			// A 404 is an invitation accepted in flight; the collaborator pass
			// removes that student.
			res.err = fmt.Errorf("cancelling %s's pending invitation to %s: %w", inv.Invitee.Login, p.repo, err)
			return res
		}
	}
	collaborators, err := client.ListDirectCollaborators(ctx, org, p.repo)
	if err != nil {
		res.err = fmt.Errorf("listing collaborators of %s: %w", p.repo, err)
		return res
	}
	for _, c := range collaborators {
		if c.Permissions.Admin {
			continue
		}
		if err := client.RemoveCollaborator(ctx, org, p.repo, c.Login); err != nil {
			res.err = fmt.Errorf("removing %s from %s: %w", c.Login, p.repo, err)
			return res
		}
	}
	if err := verifyNoStudentAccess(ctx, client, org, p.repo); err != nil {
		res.err = err
		return res
	}
	res.accessRemoved = true

	if err := client.RenameRepo(ctx, org, p.repo, p.backup); err != nil {
		res.err = fmt.Errorf("renaming %s to %s: %w", p.repo, p.backup, err)
		return res
	}
	if _, exists, err := client.GetRepo(ctx, org, p.backup); err != nil {
		res.err = fmt.Errorf("verifying %s was renamed to %s: %w", p.repo, p.backup, err)
		return res
	} else if !exists {
		res.err = fmt.Errorf("renaming %s did not take: %s does not exist", p.repo, p.backup)
		return res
	}
	// The old name must read as free, or assign would find the backup there
	// and grant access to it instead of creating a repo.
	if _, exists, err := client.GetRepo(ctx, org, p.repo); err != nil {
		res.err = fmt.Errorf("verifying the name %s is free after the rename: %w", p.repo, err)
		return res
	} else if exists {
		res.err = fmt.Errorf("renaming %s did not free its name: a repository still answers to it", p.repo)
		return res
	}
	return res
}

// verifyNoStudentAccess re-reads a repo and confirms no non-admin collaborator
// and no acceptable invitation remains.
func verifyNoStudentAccess(ctx context.Context, client retractClient, org, repo string) error {
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s's collaborators were removed: %w", repo, err)
	}
	for _, c := range collaborators {
		if !c.Permissions.Admin {
			return fmt.Errorf("removing access to %s did not take: %s is still a collaborator", repo, c.Login)
		}
	}
	invitations, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s's invitations were cancelled: %w", repo, err)
	}
	for _, inv := range invitations {
		if !inv.Expired {
			return fmt.Errorf("cancelling invitations to %s did not take: %s's invitation is still pending", repo, inv.Invitee.Login)
		}
	}
	return nil
}

// reportRetract summarizes the run, says what to do next, and returns an error
// if any repo failed.
func reportRetract(out io.Writer, name, backupPrefix string, results []retractResult) error {
	failed, pushed := 0, 0
	for _, r := range results {
		if r.err != nil {
			failed++
		} else if r.plan.pushes > 0 {
			pushed++
		}
	}
	fmt.Fprintf(out, "retracted %d repo(s), %d failed\n", len(results)-failed, failed)
	if failed > 0 {
		for _, r := range results {
			if r.err == nil {
				continue
			}
			state := "some of its students' access may be removed; it was not renamed"
			if r.accessRemoved {
				state = "its students' access is gone; it was not renamed"
			}
			fmt.Fprintf(out, "  FAILED %s (%s): %v\n", r.plan.repo, state, r.err)
		}
		return fmt.Errorf("%d repo(s) failed to retract; re-run `gh cls retract %s` to finish them (repos already retracted are not touched again), and do not run assign until it succeeds, since assign would give students back the old repos", failed, name)
	}
	fmt.Fprintf(out, "The backups are %s*. Only staff can reach them, and they are not archived.\n", backupPrefix)
	if pushed > 0 {
		fmt.Fprintf(out, "%d backup(s) hold student pushes; see STUDENT PUSHES above.\n", pushed)
	}
	fmt.Fprintf(out, "Next: point assignments.%s.template at the right template, run `gh cls assign %s`, and tell students to clone their repo again.\n", name, name)
	return nil
}
