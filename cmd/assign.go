package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/roster"
	"github.com/rixner/gh-cls/teams"
	"github.com/rixner/gh-cls/unit"
	"github.com/spf13/cobra"
)

// feedback modes accepted by -f.
const (
	feedbackPR    = "pr"
	feedbackIssue = "issue"
)

// How long to wait for an asynchronously-generated repo to become ready.
const (
	readyAttempts = 10
	readyDelay    = 2 * time.Second
)

// Feedback artifact constants.
const (
	feedbackBranch    = "feedback"
	feedbackTitle     = "Feedback"
	feedbackPRBody    = "This pull request is where the course staff leaves feedback on your work. Please do not close it. As you push commits to the default branch, your changes against the starter code appear in the diff here."
	feedbackIssueBody = "This issue is where the course staff leaves feedback on your work. Please do not close it."
)

// assignClient is the narrow set of GitHub operations assign needs.
type assignClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	SetRepoTemplate(ctx context.Context, owner, name string) error
	ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]gh.BranchCount, error)
	GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	DeleteRepo(ctx context.Context, org, name string) error
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error
	AddTeamRepo(ctx context.Context, org, teamSlug, owner, repo, permission string) error
	ApplyRuleset(ctx context.Context, org, repo string) error
	GetRef(ctx context.Context, owner, repo, ref string) (string, error)
	CreateRef(ctx context.Context, owner, repo, ref, sha string) error
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error
	PRExists(ctx context.Context, owner, repo, base string) (bool, error)
	EnableIssues(ctx context.Context, owner, repo string) error
	CreateIssue(ctx context.Context, owner, repo, title, body string) error
	IssueExists(ctx context.Context, owner, repo, title string) (bool, error)
	ListDirectCollaborators(ctx context.Context, owner, repo string) ([]gh.Collaborator, error)
	ListRepoInvitations(ctx context.Context, owner, repo string) ([]gh.Invitation, error)
}

// assignOpts carries the resolved flags and dependencies for `gh cls assign`.
type assignOpts struct {
	g                *globalOpts
	roster           string
	teams            string
	public           bool
	branchProtection bool
	allBranches      bool
	feedback         string
	allowUnsquashed  bool
	markTemplate     bool
	dryRun           bool
	newClient        func(context.Context) (assignClient, error)
	sleep            func(time.Duration)
}

func newAssignCmd(g *globalOpts) *cobra.Command {
	o := &assignOpts{
		g:         g,
		newClient: func(context.Context) (assignClient, error) { return gh.New() },
		sleep:     time.Sleep,
	}
	cmd := &cobra.Command{
		Use:   "assign <name>",
		Short: "Bulk-create assignment repositories from the assignment's template",
		Long: `Create one repository per unit (each student for an individual assignment,
each team for a group assignment) from the template repository the assignment
names, granting push to the unit's members and to the staff team. Idempotent:
existing repos are skipped for generation but their access grants are re-asserted.`,
		Example: `  gh cls assign hw1 --roster roster.csv
  gh cls assign project --roster roster.csv --teams teams.yml --branch-protection`,
		Args:    cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error { return o.validate() },
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0], o.overrides(cmd))
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.teams, "teams", "T", "", "path to the teams file (required for group, rejected for individual)")
	f.BoolVarP(&o.public, "public", "p", false, "create public repos (default private)")
	f.BoolVarP(&o.branchProtection, "branch-protection", "b", false, "apply an all-branches protection ruleset")
	f.BoolVarP(&o.allBranches, "all-branches", "a", false, "include all template branches (default: default branch only)")
	f.StringVarP(&o.feedback, "feedback", "f", "", "create a feedback artifact: pr or issue")
	f.BoolVarP(&o.allowUnsquashed, "allow-unsquashed", "U", false, "proceed even if a template branch has more than one commit")
	f.BoolVar(&o.markTemplate, "mark-template", false, "mark the assignment's template a template repository if it is not already")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "list what would be created without doing it")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// validate checks flag values that don't depend on config or the filesystem.
func (o *assignOpts) validate() error {
	switch o.feedback {
	case "", feedbackPR, feedbackIssue:
	default:
		return fmt.Errorf("invalid --feedback %q: must be %q or %q", o.feedback, feedbackPR, feedbackIssue)
	}
	return nil
}

// overrides captures which policy flags the user set explicitly, so config
// values stand for the rest.
func (o *assignOpts) overrides(cmd *cobra.Command) config.Overrides {
	ov := config.Overrides{}
	if cmd.Flags().Changed("public") {
		ov.Public = &o.public
	}
	if cmd.Flags().Changed("branch-protection") {
		ov.BranchProtection = &o.branchProtection
	}
	if cmd.Flags().Changed("feedback") {
		ov.Feedback = &o.feedback
	}
	return ov
}

// unitResult records the outcome of provisioning one repository.
type unitResult struct {
	repo    string
	status  string   // "created" or "skipped"
	pending []string // members whose grant is a not-yet-accepted invitation
	err     error
}

func (o *assignOpts) run(ctx context.Context, out io.Writer, name string, ov config.Overrides) error {
	org := o.g.org
	policy, err := o.g.cfg.Resolve(name, ov)
	if err != nil {
		return err
	}

	// Preflight 1: type/inputs consistency (not overridable).
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

	// Preflight 4: unit resolution and roster/teams consistency.
	units, report, err := unit.Resolve(policy.Type, r, tm)
	if err != nil {
		return err
	}
	for _, id := range report.UnassignedIDs {
		fmt.Fprintf(out, "warning: enrolled student %s is on no team\n", id)
	}

	// The template repo to clone is named by the assignment; a bare name lives in
	// the configured org, an owner/name may live in another org.
	if policy.Template == "" {
		return fmt.Errorf("assignment %q has no template: set assignments.%s.template to the template repository assign should clone", name, name)
	}
	tmplOwner, tmplName, err := splitRepo(qualifyTemplate(policy.Template, org))
	if err != nil {
		return fmt.Errorf("assignment %q template: %w", name, err)
	}
	tmpl := tmplOwner + "/" + tmplName
	staffTeam := o.g.staffTeam

	if o.dryRun {
		visibility := "private"
		if policy.Public {
			visibility = "public"
		}
		fmt.Fprintf(out, "DRY RUN — no changes will be made\n\n")
		fmt.Fprintf(out, "Would create %d %s repo(s) in %s from %s:\n", len(units), visibility, org, tmpl)
		if extras := planExtras(policy); extras != "" {
			fmt.Fprintf(out, "  with %s\n", extras)
		}
		for _, u := range units {
			fmt.Fprintf(out, "  %s-%s  ->  push: %s\n", name, u.Key, strings.Join(u.Members, ", "))
		}
		return nil
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// Preflight 2: the template repo exists and is actually a template repository
	// (required to generate from it). We never silently flip it: --mark-template
	// opts into marking a repo that is not yet a template.
	tmplRepo, exists, err := client.GetRepo(ctx, tmplOwner, tmplName)
	if err != nil {
		return fmt.Errorf("checking template %s: %w", tmpl, err)
	}
	if !exists {
		return fmt.Errorf("template %s not found; create it with `gh cls template %s -s <source>` or fix assignments.%s.template", tmpl, tmplName, name)
	}
	if !tmplRepo.IsTemplate {
		if !o.markTemplate {
			return fmt.Errorf("template %s is not a template repository; mark it in the GitHub UI, or re-run with --mark-template to set it", tmpl)
		}
		if err := client.SetRepoTemplate(ctx, tmplOwner, tmplName); err != nil {
			return fmt.Errorf("marking template %s a template repository: %w", tmpl, err)
		}
	}

	// Preflight 3: template fully squashed (all branches), overridable with -U.
	if err := o.checkSquashed(ctx, client, tmplOwner, tmplName); err != nil {
		return err
	}

	results := runConcurrent(ctx, o.g.concurrency, units, func(ctx context.Context, u unit.Unit) unitResult {
		return o.provision(ctx, client, org, name, tmplOwner, tmplName, staffTeam, policy, u)
	})
	return reportResults(out, results)
}

// planExtras describes the per-repo protection/feedback a run would add.
func planExtras(policy config.Policy) string {
	var parts []string
	if policy.BranchProtection {
		parts = append(parts, "an all-branches protection ruleset")
	}
	switch policy.Feedback {
	case feedbackPR:
		parts = append(parts, "a feedback pull request")
	case feedbackIssue:
		parts = append(parts, "a feedback issue")
	}
	return strings.Join(parts, " and ")
}

// checkSquashed verifies every branch of the template has exactly one commit,
// aborting with a per-branch breakdown unless --allow-unsquashed is set, so a
// template carrying development history never leaks it into student repos.
func (o *assignOpts) checkSquashed(ctx context.Context, client assignClient, tmplOwner, tmplName string) error {
	branches, err := client.ListBranchesWithCommitCount(ctx, tmplOwner, tmplName)
	if err != nil {
		return fmt.Errorf("inspecting template branches: %w", err)
	}
	unsquashed := false
	for _, b := range branches {
		if b.Commits > 1 {
			unsquashed = true
		}
	}
	if !unsquashed || o.allowUnsquashed {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "template %s/%s is not fully squashed:\n", tmplOwner, tmplName)
	for _, b := range branches {
		state := "ok"
		if b.Commits > 1 {
			state = "NOT squashed"
		}
		fmt.Fprintf(&sb, "  %-20s %d commit(s)  %s\n", b.Name, b.Commits, state)
	}
	fmt.Fprintf(&sb, "Aborting. Rebuild it squashed with `gh cls template`, or pass --allow-unsquashed (-U) to proceed anyway.")
	return errors.New(sb.String())
}

// provision creates (or reuses) one repository and asserts its access grants.
// Branch protection is applied once, when the repo is first created; the
// feedback artifact is reconciled on every run so a partial failure is repaired
// on re-run without reopening a closed PR or issue.
func (o *assignOpts) provision(ctx context.Context, client assignClient, org, name, tmplOwner, tmplName, staffTeam string, policy config.Policy, u unit.Unit) unitResult {
	repo := name + "-" + u.Key
	res := unitResult{repo: repo}

	info, exists, err := client.GetRepo(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("checking %s: %w", repo, err)
		return res
	}
	if exists {
		res.status = "skipped"
	} else {
		if err := client.GenerateFromTemplate(ctx, tmplOwner, tmplName, org, repo, !policy.Public, o.allBranches); err != nil {
			res.err = fmt.Errorf("generating %s: %w", repo, err)
			return res
		}
		if info, err = waitRepoReady(ctx, client, o.sleep, org, repo); err != nil {
			res.err = err
			return res
		}
		res.status = "created"
	}

	// Confirm the repo's visibility matches the policy before granting anyone
	// access — on a freshly generated repo and on a reused one alike. A private
	// assignment that came out (or has since drifted) public would expose student
	// work, so abort this repo rather than (re-)assert access on a leaky one.
	if err := checkVisibility(repo, info, policy.Public); err != nil {
		// A repo we just generated with the wrong visibility is our own leaky
		// artifact: no access has been granted yet, so roll it back rather than
		// leave a wrongly-public repo behind. A reused repo is never deleted — it
		// may already hold student work — so it is only reported.
		if res.status == "created" {
			if delErr := client.DeleteRepo(ctx, org, repo); delErr != nil {
				res.err = fmt.Errorf("%w; additionally, rolling back the leaked repo failed — delete %s/%s manually: %v", err, org, repo, delErr)
				return res
			}
			res.err = fmt.Errorf("%w (rolled back the just-created repo)", err)
			return res
		}
		res.err = err
		return res
	}

	// Re-assert grants so re-running is safe and access is correct.
	for _, member := range u.Members {
		if err := client.AddCollaborator(ctx, org, repo, member, "push"); err != nil {
			res.err = fmt.Errorf("granting push to %s on %s: %w", member, repo, err)
			return res
		}
	}
	if staffTeam != "" {
		if err := client.AddTeamRepo(ctx, org, staffTeam, org, repo, "push"); err != nil {
			res.err = fmt.Errorf("granting staff team on %s: %w", repo, err)
			return res
		}
	}

	// Branch protection is reconciled on every run, not only when the repo is
	// first created: ApplyRuleset is idempotent (it no-ops when the ruleset is
	// already present), so a transient failure on a prior run is repaired here
	// instead of leaving an existing repo permanently unprotected.
	if policy.BranchProtection {
		if err := client.ApplyRuleset(ctx, org, repo); err != nil {
			res.err = fmt.Errorf("applying branch protection to %s: %w", repo, err)
			return res
		}
	}
	// Feedback is reconciled on every run: each piece is created only if missing,
	// so a partial failure on a prior run is repaired here.
	if err := o.addFeedback(ctx, client, org, repo, info, policy.Feedback); err != nil {
		res.err = err
		return res
	}

	// Post-condition: confirm every member actually holds the access we granted.
	// A grant to a non-member becomes a GitHub invitation that conveys no access
	// until accepted, so the only honest end state is "has write, or has a pending
	// invitation"; a member who is neither means the grant silently did not land.
	pending, err := o.verifyAccess(ctx, client, org, repo, u.Members)
	if err != nil {
		res.err = err
		return res
	}
	res.pending = pending
	return res
}

// checkVisibility fails if a repo's visibility does not match what the policy
// requested. It gates access: a private assignment that is (or has drifted)
// public would expose student work, so this is checked before any grant.
func checkVisibility(repo string, info *gh.Repo, wantPublic bool) error {
	if info.Private != wantPublic {
		return nil
	}
	actual, want := "private", "private"
	if !info.Private {
		actual = "public"
	}
	if wantPublic {
		want = "public"
	}
	return fmt.Errorf("repository %s is %s but %s was requested; aborting before asserting access", repo, actual, want)
}

// verifyAccess re-reads a repo's collaborators and pending invitations and
// confirms every granted member is reflected as one or the other. A member who
// is neither means the grant silently failed, which is a loud error. The pending
// invitees are returned so the run can report that they must still accept.
func (o *assignOpts) verifyAccess(ctx context.Context, client assignClient, org, repo string, members []string) ([]string, error) {
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("verifying access on %s: %w", repo, err)
	}
	hasWrite := make(map[string]bool, len(collaborators))
	for _, c := range collaborators {
		if c.Permissions.Admin || c.Permissions.Maintain || c.Permissions.Push {
			hasWrite[strings.ToLower(c.Login)] = true
		}
	}
	invitations, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("verifying invitations on %s: %w", repo, err)
	}
	invited := make(map[string]bool, len(invitations))
	for _, inv := range invitations {
		invited[strings.ToLower(inv.Invitee.Login)] = true
	}

	var pending []string
	for _, m := range members {
		key := strings.ToLower(m)
		switch {
		case hasWrite[key]:
			// access is live
		case invited[key]:
			pending = append(pending, m)
		default:
			return nil, fmt.Errorf("push grant to %s on %s did not take effect: they are neither a collaborator nor have a pending invitation; re-run assign to repair it", m, repo)
		}
	}
	return pending, nil
}

// addFeedback ensures the chosen feedback artifact exists, creating only the
// pieces that are missing. This makes it safe to call on every run: a partial
// failure is repaired, while an existing (even closed) PR or issue is left be.
func (o *assignOpts) addFeedback(ctx context.Context, client assignClient, org, repo string, info *gh.Repo, mode string) error {
	switch mode {
	case feedbackPR:
		// A PR needs two distinct refs sharing history; pin a feedback branch at
		// the starter commit so the student's later pushes diff against it.
		branchExists, err := client.BranchExists(ctx, org, repo, feedbackBranch)
		if err != nil {
			return fmt.Errorf("checking feedback branch on %s: %w", repo, err)
		}
		if !branchExists {
			sha, err := client.GetRef(ctx, org, repo, "heads/"+info.DefaultBranch)
			if err != nil {
				return fmt.Errorf("reading starter commit of %s: %w", repo, err)
			}
			if err := client.CreateRef(ctx, org, repo, "refs/heads/"+feedbackBranch, sha); err != nil {
				return fmt.Errorf("creating feedback branch on %s: %w", repo, err)
			}
		}
		prExists, err := client.PRExists(ctx, org, repo, feedbackBranch)
		if err != nil {
			return fmt.Errorf("checking feedback PR on %s: %w", repo, err)
		}
		if !prExists {
			if err := client.CreatePR(ctx, org, repo, feedbackTitle, info.DefaultBranch, feedbackBranch, feedbackPRBody); err != nil {
				return fmt.Errorf("opening feedback PR on %s: %w", repo, err)
			}
		}
	case feedbackIssue:
		if !info.HasIssues {
			if err := client.EnableIssues(ctx, org, repo); err != nil {
				return fmt.Errorf("enabling issues on %s: %w", repo, err)
			}
		}
		issueExists, err := client.IssueExists(ctx, org, repo, feedbackTitle)
		if err != nil {
			return fmt.Errorf("checking feedback issue on %s: %w", repo, err)
		}
		if !issueExists {
			if err := client.CreateIssue(ctx, org, repo, feedbackTitle, feedbackIssueBody); err != nil {
				return fmt.Errorf("opening feedback issue on %s: %w", repo, err)
			}
		}
	}
	return nil
}

// reportResults summarizes the run and returns an error if any unit failed.
func reportResults(out io.Writer, results []unitResult) error {
	var created, skipped, failed, pending int
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
		case r.status == "skipped":
			skipped++
		default:
			created++
		}
		pending += len(r.pending)
	}
	fmt.Fprintf(out, "\n%d created, %d skipped, %d failed\n", created, skipped, failed)
	if pending > 0 {
		// Outside collaborators must accept the invitation before they have access;
		// until then the repo is provisioned but the student cannot push.
		fmt.Fprintf(out, "note: %d student invitation(s) are still pending — those students must accept the GitHub invitation before they can push\n", pending)
	}
	if skipped > 0 {
		// Re-asserting push on existing repos un-does a prior freeze on them.
		fmt.Fprintf(out, "note: re-asserted push on %d existing repo(s); if these were frozen, they are now writable again\n", skipped)
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
