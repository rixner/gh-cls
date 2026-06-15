package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/rixner/gh-cls/unit"
	"github.com/spf13/cobra"
)

// auditClient is the narrow set of GitHub operations audit needs.
type auditClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error
	DeleteRepoInvitation(ctx context.Context, owner, repo string, id int64) error
}

// memberStatus is one expected student's actual access state on their repo.
type memberStatus int

const (
	statusOnRepo  memberStatus = iota // accepted: a direct collaborator with write access
	statusPending                     // invited, invitation not yet expired
	statusExpired                     // invited, but the invitation has expired
	statusMissing                     // repo exists but the student has no access or invitation
	statusNoRepo                      // the expected repo does not exist
)

func (s memberStatus) label() string {
	switch s {
	case statusOnRepo:
		return "on repo"
	case statusPending:
		return "invited (pending)"
	case statusExpired:
		return "invited (EXPIRED)"
	case statusMissing:
		return "MISSING"
	case statusNoRepo:
		return "NO REPO"
	}
	return "?"
}

// auditOpts carries the resolved flags and dependencies for `gh cls audit`.
type auditOpts struct {
	g         *globalOpts
	roster    string
	teams     string
	all       bool
	renew     bool
	dryRun    bool
	newClient func(context.Context) (auditClient, error)
}

func newAuditCmd(g *globalOpts) *cobra.Command {
	o := &auditOpts{
		g:         g,
		newClient: func(context.Context) (auditClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "audit <name>",
		Short: "Reconcile who should be on each assignment repo against who actually is",
		Long: `Compare the students who should have access to the <name>-* repos (resolved
from the roster, and the teams file for a group assignment) against the actual
state on GitHub, reporting each student as one of: on repo (accepted), invited
(pending), invited (EXPIRED), MISSING (the repo exists but they have neither
access nor an invitation), or NO REPO (the repo was never created). It also
flags any access that is present but not expected.

Students are added as outside collaborators, so a grant becomes an invitation
they must accept within seven days; --renew re-issues access for everyone whose
invitation expired or who is missing entirely (it never removes access).`,
		Example: `  gh cls audit hw1 --roster roster.csv
  gh cls audit project --roster roster.csv --teams teams.yml
  gh cls audit hw1 --roster roster.csv --renew`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.teams, "teams", "T", "", "path to the teams file (required for group, rejected for individual)")
	f.BoolVar(&o.all, "all", false, "list every student, including those already on their repo")
	f.BoolVar(&o.renew, "renew", false, "re-issue access for expired or missing students")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "with --renew, show what would change without doing it")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// repoAudit is the classification of one expected repo and its members.
type repoAudit struct {
	repo    string
	members []memberAudit
	extra   []extraAccess
	err     error
}

// memberAudit is one expected student's status on their repo.
type memberAudit struct {
	id     string // university identifier (roster), empty if not found
	login  string // GitHub username
	status memberStatus
	invID  int64 // the expired invitation's id, for --renew
}

// extraAccess is access present on a repo that the assignment did not expect.
type extraAccess struct {
	login string
	kind  string
}

func (o *auditOpts) run(ctx context.Context, out io.Writer, name string) error {
	org := o.g.org
	policy, err := o.g.cfg.Resolve(name, config.Overrides{})
	if err != nil {
		return err
	}

	switch policy.Type {
	case config.TypeGroup:
		if o.teams == "" {
			return fmt.Errorf("assignment %q is a group assignment: --teams is required", name)
		}
	case config.TypeIndividual:
		if o.teams != "" {
			return fmt.Errorf("assignment %q is an individual assignment: --teams is not allowed", name)
		}
	}

	r, err := roster.ParseFile(o.roster)
	if err != nil {
		return err
	}
	var tm *teams.Teams
	if policy.Type == config.TypeGroup {
		if tm, err = teams.ParseFile(o.teams); err != nil {
			return err
		}
	}
	units, report, err := unit.Resolve(policy.Type, r, tm)
	if err != nil {
		return err
	}
	for _, id := range report.UnassignedIDs {
		fmt.Fprintf(out, "warning: enrolled student %s is on no team\n", id)
	}
	byUser := r.ByUsername()

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// One listing tells us which expected repos exist, so a not-yet-created repo
	// is reported as NO REPO rather than surfacing as a 404 mid-audit.
	repos, err := client.ListOrgReposByPrefix(ctx, org, name+"-")
	if err != nil {
		return fmt.Errorf("listing %s-* repositories: %w", name, err)
	}
	exists := make(map[string]bool, len(repos))
	for _, rp := range repos {
		exists[rp.Name] = true
	}

	results := runConcurrent(ctx, o.g.concurrency, units, func(ctx context.Context, u unit.Unit) repoAudit {
		return o.auditUnit(ctx, client, org, name, u, byUser, exists)
	})

	if o.renew {
		return o.runRenew(ctx, out, client, org, name, results)
	}
	return reportAudit(out, org, name, o.all, results)
}

// auditUnit classifies one expected repo: each member's status, plus any access
// present that the assignment did not expect.
func (o *auditOpts) auditUnit(ctx context.Context, client auditClient, org, name string, u unit.Unit, byUser map[string]string, exists map[string]bool) repoAudit {
	repo := name + "-" + u.Key
	res := repoAudit{repo: repo}

	if !exists[repo] {
		for _, m := range u.Members {
			res.members = append(res.members, memberAudit{id: byUser[strings.ToLower(m)], login: m, status: statusNoRepo})
		}
		return res
	}

	collabs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("listing collaborators of %s: %w", repo, err)
		return res
	}
	invs, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("listing invitations of %s: %w", repo, err)
		return res
	}

	writeAccess := map[string]bool{}
	isAdmin := map[string]bool{}
	for _, c := range collabs {
		l := strings.ToLower(c.Login)
		if c.Permissions.Admin {
			isAdmin[l] = true
		}
		if c.Permissions.Admin || c.Permissions.Maintain || c.Permissions.Push {
			writeAccess[l] = true
		}
	}
	pending := map[string]bool{}
	expired := map[string]int64{}
	for _, inv := range invs {
		l := strings.ToLower(inv.Invitee.Login)
		if inv.Expired {
			expired[l] = inv.ID
		} else {
			pending[l] = true
		}
	}

	expected := make(map[string]bool, len(u.Members))
	for _, m := range u.Members {
		l := strings.ToLower(m)
		expected[l] = true
		ma := memberAudit{id: byUser[l], login: m}
		switch {
		case writeAccess[l]:
			ma.status = statusOnRepo
		case pending[l]:
			ma.status = statusPending
		case expired[l] != 0:
			ma.status = statusExpired
			ma.invID = expired[l]
		default:
			ma.status = statusMissing
		}
		res.members = append(res.members, ma)
	}

	// Belt-and-suspenders: access present that this assignment did not expect.
	// Admins (staff/instructor) are skipped; staff reach repos through their team,
	// not as direct collaborators, so they do not appear here at all.
	for _, c := range collabs {
		l := strings.ToLower(c.Login)
		if expected[l] || isAdmin[l] {
			continue
		}
		if c.Permissions.Pull || c.Permissions.Triage || c.Permissions.Push || c.Permissions.Maintain {
			res.extra = append(res.extra, extraAccess{login: c.Login, kind: "collaborator"})
		}
	}
	for _, inv := range invs {
		l := strings.ToLower(inv.Invitee.Login)
		if expected[l] {
			continue
		}
		kind := "invitation (pending)"
		if inv.Expired {
			kind = "invitation (expired)"
		}
		res.extra = append(res.extra, extraAccess{login: inv.Invitee.Login, kind: kind})
	}
	return res
}

// reportAudit prints the reconciliation. By default it lists only students who
// are not already on their repo (with --all it lists everyone), always shows
// unexpected access, and summarizes. It returns an error only when a repo could
// not be audited, not when it found expired or missing students.
func reportAudit(out io.Writer, org, name string, showAll bool, results []repoAudit) error {
	counts := map[memberStatus]int{}
	students, repos, failed := 0, 0, 0

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(out, "Audit of %s-* in %s\n\n", name, org)
	fmt.Fprintln(tw, "  REPO\tUNIVERSITY ID\tGITHUB\tSTATUS")
	shown := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		repos++
		for _, m := range r.members {
			students++
			counts[m.status]++
			if m.status == statusOnRepo && !showAll {
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.repo, dash(m.id), m.login, m.status.label())
			shown++
		}
	}
	if shown > 0 {
		tw.Flush()
	}
	if shown == 0 {
		fmt.Fprintf(out, "All %d student(s) are on their repos.\n", students)
	}

	fmt.Fprintf(out, "\nSummary: %d on repo, %d pending, %d expired, %d missing, %d without a repo (across %d repo(s), %d student(s)).\n",
		counts[statusOnRepo], counts[statusPending], counts[statusExpired], counts[statusMissing], counts[statusNoRepo], repos, students)

	if action := counts[statusExpired] + counts[statusMissing]; action > 0 {
		fmt.Fprintf(out, "Action needed: %d expired + %d missing — re-issue with `gh cls audit %s --roster <file> --renew`.\n",
			counts[statusExpired], counts[statusMissing], name)
	}
	if counts[statusNoRepo] > 0 {
		fmt.Fprintf(out, "Note: %d student(s) have no repo yet — run `gh cls assign %s` to create them.\n", counts[statusNoRepo], name)
	}

	reportExtras(out, results)

	if failed > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s: %v\n", r.repo, r.err)
			}
		}
		return fmt.Errorf("%d repo(s) could not be audited", failed)
	}
	return nil
}

// reportExtras lists access present on a repo that the assignment did not expect.
func reportExtras(out io.Writer, results []repoAudit) {
	var any bool
	for _, r := range results {
		if len(r.extra) > 0 {
			any = true
			break
		}
	}
	if !any {
		return
	}
	fmt.Fprintln(out, "\nUnexpected access (not in the roster/teams for this assignment):")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, r := range results {
		for _, e := range r.extra {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.repo, e.login, e.kind)
		}
	}
	tw.Flush()
}

// renewResult records the outcome of re-issuing access to one student.
type renewResult struct {
	repo  string
	login string
	err   error
}

// runRenew re-issues access for every expired or missing student. It refuses to
// act on a partial picture: if any repo failed to audit, it aborts so a flaky
// listing cannot be mistaken for "nothing to renew there".
func (o *auditOpts) runRenew(ctx context.Context, out io.Writer, client auditClient, org, name string, results []repoAudit) error {
	for _, r := range results {
		if r.err != nil {
			return fmt.Errorf("aborting --renew: %s could not be audited, so the set to renew is incomplete: %w", r.repo, r.err)
		}
	}

	type job struct {
		repo, login string
		invID       int64 // non-zero => cancel this expired invitation first
	}
	var jobs []job
	noRepo := 0
	for _, r := range results {
		for _, m := range r.members {
			switch m.status {
			case statusExpired:
				jobs = append(jobs, job{r.repo, m.login, m.invID})
			case statusMissing:
				jobs = append(jobs, job{r.repo, m.login, 0})
			case statusNoRepo:
				noRepo++
			}
		}
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sRe-issuing access for %d student(s) in %s\n", prefix, len(jobs), org)
	if noRepo > 0 {
		fmt.Fprintf(out, "note: %d student(s) have no repo to renew on — run `gh cls assign %s` first\n", noRepo, name)
	}
	if len(jobs) == 0 {
		fmt.Fprintln(out, "nothing to re-issue")
		return nil
	}

	res := runConcurrent(ctx, o.g.concurrency, jobs, func(ctx context.Context, j job) renewResult {
		r := renewResult{repo: j.repo, login: j.login}
		if o.dryRun {
			return r
		}
		// Cancel an expired invitation before re-adding, so a genuinely fresh
		// invitation is issued rather than leaving the stale one in place.
		if j.invID != 0 {
			if err := client.DeleteRepoInvitation(ctx, org, j.repo, j.invID); err != nil {
				r.err = fmt.Errorf("cancelling expired invitation for %s on %s: %w", j.login, j.repo, err)
				return r
			}
		}
		if err := client.AddCollaborator(ctx, org, j.repo, j.login, "push"); err != nil {
			r.err = fmt.Errorf("re-inviting %s on %s (an expired invitation, if any, was already cancelled; re-run `gh cls assign %s` if access is now absent): %w", j.login, j.repo, name, err)
			return r
		}
		if err := o.verifyRenewed(ctx, client, org, j.repo, j.login); err != nil {
			r.err = err
			return r
		}
		return r
	})
	return reportRenew(out, o.dryRun, res)
}

// verifyRenewed confirms a re-issued student now holds access or has a fresh
// (non-expired) invitation. A 200 on the add is not proof; this is the
// post-condition that the access was actually restored.
func (o *auditOpts) verifyRenewed(ctx context.Context, client auditClient, org, repo, login string) error {
	collabs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s on %s after re-inviting: %w", login, repo, err)
	}
	for _, c := range collabs {
		if strings.EqualFold(c.Login, login) && (c.Permissions.Push || c.Permissions.Admin || c.Permissions.Maintain) {
			return nil
		}
	}
	invs, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s on %s after re-inviting: %w", login, repo, err)
	}
	for _, inv := range invs {
		if strings.EqualFold(inv.Invitee.Login, login) && !inv.Expired {
			return nil
		}
	}
	return fmt.Errorf("re-invite of %s on %s did not take: afterward they have neither access nor a fresh invitation; re-run `gh cls assign`", login, repo)
}

// reportRenew summarizes a renew run and returns an error if any student failed.
func reportRenew(out io.Writer, dryRun bool, results []renewResult) error {
	done, failed := 0, 0
	verb := "re-issued"
	if dryRun {
		verb = "would re-issue"
	}
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		done++
		fmt.Fprintf(out, "  %s %s\n", r.repo, r.login)
	}
	fmt.Fprintf(out, "%s access for %d student(s), %d failed\n", verb, done, failed)
	if failed > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s %s: %v\n", r.repo, r.login, r.err)
			}
		}
		return fmt.Errorf("%d student(s) failed to renew", failed)
	}
	return nil
}

// dash renders an empty university id as a placeholder in the table.
func dash(s string) string {
	if s == "" {
		return "(not in roster)"
	}
	return s
}
