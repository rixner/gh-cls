package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/unit"
	"github.com/spf13/cobra"
)

// assignClient is the narrow set of GitHub operations assign needs.
type assignClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	UserExists(ctx context.Context, username string) (bool, error)
	GetTeam(ctx context.Context, org, slug string) (*gh.Team, bool, error)
	GetRepo(ctx context.Context, owner, name string) (*gh.Repo, bool, error)
	ListOrgReposByPrefix(ctx context.Context, org, prefix string) ([]gh.Repo, error)
	SetRepoTemplate(ctx context.Context, owner, name string) error
	ListBranchesWithCommitCount(ctx context.Context, owner, repo string) ([]gh.BranchCount, error)
	GenerateFromTemplate(ctx context.Context, tmplOwner, tmplRepo, owner, name string, private, includeAllBranches bool) error
	DeleteRepo(ctx context.Context, org, name string) error
	AddCollaborator(ctx context.Context, owner, repo, username, permission string) error
	AddTeamRepo(ctx context.Context, org, teamSlug, owner, repo, permission string) error
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	ListRepoPropertyValues(ctx context.Context, org string) (map[string]map[string]string, error)
	ApplyRuleset(ctx context.Context, org, repo string) error
	CreateRef(ctx context.Context, owner, repo, ref, sha string) error
	RebaseOntoEmptyRoot(ctx context.Context, owner, repo, branch string) (string, error)
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	CreatePR(ctx context.Context, owner, repo, title, head, base, body string) error
	PRExists(ctx context.Context, owner, repo, base string) (bool, error)
	// The two Find lookups let assign ask an existing repo which feedback
	// artifact it actually carries, rather than assuming the configured one.
	FindPRByBase(ctx context.Context, owner, repo, base string) (int, string, bool, error)
	FindIssueByTitle(ctx context.Context, owner, repo, title string) (int, string, bool, error)
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
	groups           string
	public           bool
	branchProtection bool
	allBranches      bool
	feedback         string
	allowUnsquashed  bool
	force            bool
	markTemplate     bool
	dryRun           bool
	newClient        func(context.Context) (assignClient, error)
	sleep            func(time.Duration)
}

func newAssignCmd(g *globalOpts) *cobra.Command {
	o := &assignOpts{
		g:         g,
		newClient: func(context.Context) (assignClient, error) { return g.client() },
		sleep:     time.Sleep,
	}
	cmd := &cobra.Command{
		Use:   "assign <name>",
		Short: "Bulk-create assignment repositories from the assignment's template",
		Long: `Create one repository per unit (each student for an individual assignment,
each group for a group assignment) from the template repository the assignment
names, granting push to the unit's members and to the staff team. Idempotent:
existing repos are skipped for generation but their access grants are re-asserted.

For a group assignment, assign aborts before creating anything if the roster and
groups file are inconsistent -- an enrolled student in no group, or a student in
more than one group -- so a mistake is fixed before repos exist. Pass --force to
downgrade those to warnings and proceed anyway (e.g. a student intentionally
excused from the group work).`,
		Example: `  gh cls assign hw1 --roster roster.csv
  gh cls assign project --roster roster.csv --groups groups.yml --branch-protection`,
		Args:    cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error { return o.validate() },
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0], o.overrides(cmd))
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.groups, "groups", "g", "", "path to the groups file (required for group, rejected for individual)")
	f.BoolVarP(&o.public, "public", "p", false, "create public repos (default private)")
	f.BoolVarP(&o.branchProtection, "branch-protection", "b", false, "apply an all-branches protection ruleset")
	f.BoolVarP(&o.allBranches, "all-branches", "a", false, "include all template branches (default: default branch only)")
	f.StringVar(&o.feedback, "feedback", "", "override the assignment's feedback artifact for this run: pr or issue")
	f.BoolVarP(&o.allowUnsquashed, "allow-unsquashed", "U", false, "proceed even if a template branch has more than one commit")
	f.BoolVarP(&o.force, "force", "F", false, "proceed even if the roster/groups are inconsistent (a student in no group, or in more than one)")
	f.BoolVar(&o.markTemplate, "mark-template", false, "mark the assignment's template a template repository if it is not already")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "run the preflight checks and report what would be created, changing nothing")
	_ = cmd.MarkFlagRequired("roster")
	return cmd
}

// validate checks flag values that don't depend on config or the filesystem.
func (o *assignOpts) validate() error {
	switch o.feedback {
	case config.FeedbackNone, config.FeedbackPR, config.FeedbackIssue:
	default:
		return fmt.Errorf("invalid --feedback %q: must be %q or %q", o.feedback, config.FeedbackPR, config.FeedbackIssue)
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
	frozen  bool     // the repo is recorded frozen, so members were granted read
	// grantedWrite records that at least one member was actually granted push,
	// which is what makes the repo worth re-checking against a concurrent freeze.
	// It is deliberately independent of err: a grant that landed before a later
	// step failed still handed out write.
	grantedWrite bool
	err          error
}

func (o *assignOpts) run(ctx context.Context, out io.Writer, name string, ov config.Overrides) error {
	org := o.g.org
	policy, err := o.g.cfg.Resolve(name, ov)
	if err != nil {
		return err
	}

	// Preflight 1 & 4: type/inputs consistency and unit resolution. A student on
	// no group or in more than one group aborts before anything is created, so the
	// mistake is fixed before repos exist; --force downgrades it to a warning.
	units, report, _, err := loadUnits(name, policy.Type, o.roster, o.groups)
	if err != nil {
		return err
	}
	if err := checkGroupConsistency(out, report, o.force); err != nil {
		return err
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

	// Preflight 1b: no unit's repository name is a configured template repository.
	// Purely local, so it aborts before the first remote call, let alone the first
	// mutation.
	if err := checkTemplateCollision(o.g.cfg, name, policy.Type, units); err != nil {
		return err
	}
	staffTeam := o.g.staffTeam

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN: no changes will be made\n\n")
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}

	// Preflight: the staff team must already exist. assign grants it push on every
	// repo, so a missing team would only surface after repos are created; check it
	// up front, before the first mutation. setup is what creates it.
	if _, exists, err := client.GetTeam(ctx, org, staffTeam); err != nil {
		return fmt.Errorf("checking staff team %q: %w", staffTeam, err)
	} else if !exists {
		return fmt.Errorf("staff team %q not found in %s; run `gh cls setup` to create it", staffTeam, org)
	}

	// Preflight 2: the template repo exists and is actually a template repository
	// (required to generate from it). We never silently flip it: --mark-template
	// opts into marking a repo that is not yet a template.
	tmplRepo, exists, err := client.GetRepo(ctx, tmplOwner, tmplName)
	if err != nil {
		return fmt.Errorf("checking template %s: %w", tmpl, err)
	}
	if !exists {
		return fmt.Errorf("template %s not found; create it with `gh cls template %s -S <source>` or fix assignments.%s.template", tmpl, tmplName, name)
	}
	markTemplate := false
	if !tmplRepo.IsTemplate {
		if !o.markTemplate {
			return fmt.Errorf("template %s is not a template repository; mark it in the GitHub UI, or re-run with --mark-template to set it", tmpl)
		}
		markTemplate = true
		if !o.dryRun {
			if err := client.SetRepoTemplate(ctx, tmplOwner, tmplName); err != nil {
				return fmt.Errorf("marking template %s a template repository: %w", tmpl, err)
			}
		}
	}

	// Preflight 3: template fully squashed (all branches), overridable with -U.
	if err := o.checkSquashed(ctx, client, tmplOwner, tmplName); err != nil {
		return err
	}

	// Preflight 5: every roster username is a real GitHub account. A bogus handle
	// otherwise fails only at the invite step, after its repo has already been
	// generated, leaving a stray repo behind. Validate up front, before the first
	// mutation, so the whole roster can be fixed in one pass.
	if err := checkRosterUsers(ctx, client, o.g.concurrency, units); err != nil {
		return err
	}

	// Preflight 6: each repo's recorded freeze state, so re-asserting grants on an
	// existing repo restores the access that repo is supposed to have rather than
	// unconditionally push. assign is idempotent by design and re-run freely, so
	// without this, adding one late student after a deadline re-opens the whole
	// assignment. One org-wide call covers every repo.
	frozen, err := readFrozenStates(ctx, client, org)
	if err != nil {
		return err
	}

	// Preflight 7: what this run is about to cost. One listing tells us which
	// repos already exist, which is the difference between a run that creates a
	// class and one that only re-asserts its grants, and those differ by an order
	// of magnitude in time. Stating it before the first mutation is what keeps a
	// long run from being a surprise partway through a class.
	existing, err := existingRepos(ctx, client, org, name)
	if err != nil {
		return err
	}
	cost := runCost(units, existing, name, policy)

	printPlan(out, units, existing, name, org, tmpl, policy, markTemplate, cost)

	if o.dryRun {
		// Every preflight above has run against the real org, so what remains is the
		// per-unit outcome: whether each repo already exists, and what access its
		// recorded freeze state gives its members. Both are reads.
		plans := runConcurrent(ctx, o.g.concurrency, units, func(ctx context.Context, u unit.Unit) planResult {
			return planUnit(ctx, client, org, name, policy, frozen, u)
		})
		return reportPlan(out, plans)
	}

	// A full class takes minutes at the rate the writes are paced, and each repo
	// is several round trips, so report every one as it lands. Silence for that long is indistinguishable
	// from a hung run, and the instructor cannot tell how far a run got if they
	// have to interrupt it.
	prog := newProgress(out, len(units), 7) // "created", "skipped", "FAILED"
	results := runConcurrentProgress(ctx, o.g.concurrency, units, func(ctx context.Context, u unit.Unit) unitResult {
		return o.provision(ctx, client, org, name, tmplOwner, tmplName, staffTeam, policy, frozen, u)
	}, func(r unitResult) { prog.item(failedOr(r.err, r.status), r.repo) })

	// Post-condition: a freeze that started while this run was granting would have
	// been invisible to the record read above, leaving repos writable past their
	// deadline. Re-read and fail loudly if so, rather than reporting a clean run.
	// What matters is whether write was actually handed out, not whether the repo
	// finished cleanly: a repo whose grant landed and whose next step then failed
	// is writable just the same, and is exactly the one worth re-checking.
	var grantedWrite []string
	for _, r := range results {
		if r.grantedWrite {
			grantedWrite = append(grantedWrite, r.repo)
		}
	}
	reopened, raceErr := checkGrantRace(ctx, client, org, grantedWrite)

	// Both outcomes are reported and both errors returned. Returning early on a
	// failed repo used to discard the race result entirely, so one unrelated
	// failure hid "other repos are writable past their deadline" -- the silent
	// outcome this check exists to prevent, lost precisely in the messy run where
	// it is most likely.
	errs := []error{reportResults(out, results)}
	switch {
	case raceErr != nil:
		errs = append(errs, raceErr)
	case len(reopened) > 0:
		errs = append(errs, grantRaceError(name, reopened))
	}
	return errors.Join(errs...)
}

// checkGroupConsistency enforces the roster/groups consistency findings: an
// enrolled student in no group, or a student in more than one group. Both are
// almost always a groups-file mistake, so by default this aborts before any repo
// is created, listing every problem so the file can be fixed in one pass. --force
// downgrades them to warnings and proceeds, for the rare intentional case (a
// student excused from the group work).
func checkGroupConsistency(out io.Writer, report unit.Report, force bool) error {
	if len(report.UnassignedIDs) == 0 && len(report.MultiGroup) == 0 {
		return nil
	}

	var problems []string
	if len(report.UnassignedIDs) > 0 {
		problems = append(problems, "enrolled students in no group:\n  "+strings.Join(report.UnassignedIDs, "\n  "))
	}
	if len(report.MultiGroup) > 0 {
		lines := make([]string, len(report.MultiGroup))
		for i, m := range report.MultiGroup {
			lines[i] = fmt.Sprintf("%s: %s", m.ID, strings.Join(m.Groups, ", "))
		}
		problems = append(problems, "students in more than one group:\n  "+strings.Join(lines, "\n  "))
	}
	joined := strings.Join(problems, "\n")

	if !force {
		return fmt.Errorf("roster and groups file are inconsistent (fix it, or pass --force to proceed anyway; no repositories were created):\n%s", joined)
	}
	fmt.Fprintf(out, "warning: proceeding with --force despite roster/groups inconsistencies:\n%s\n", joined)
	return nil
}

// printPlan states what the run is about to provision, before the first
// mutation. A real run otherwise said nothing until its results summary, so the
// org and template it targeted never appeared in the output: with a config per
// semester and $GH_CLS_CONFIG able to point at either, this line is the last
// chance to notice the wrong one.
func printPlan(out io.Writer, units []unit.Unit, existing map[string]bool, name, org, tmpl string, policy config.Policy, markTemplate bool, cost gh.Cost) {
	visibility := "private"
	if policy.Public {
		visibility = "public"
	}
	fmt.Fprintf(out, "Provisioning %d %s repo(s) in %s from %s\n", len(units), visibility, org, tmpl)
	if extras := planExtras(policy); extras != "" {
		fmt.Fprintf(out, "  with %s\n", extras)
	}
	if markTemplate {
		fmt.Fprintf(out, "  marking %s a template repository first (--mark-template)\n", tmpl)
	}

	have := countExisting(units, existing, name)
	switch create := len(units) - have; {
	case create == 0:
		fmt.Fprintf(out, "  nothing to create; %s already there, so this re-asserts their access\n", plural(have, "repo"))
	case have == 0:
		fmt.Fprintf(out, "  %s to create\n", plural(create, "repo"))
	default:
		fmt.Fprintf(out, "  %s to create, %d already there\n", plural(create, "repo"), have)
	}
	fmt.Fprintf(out, "  at least %s at the rates GitHub's limits allow\n", roundDuration(cost.Duration()))
	if total := cost.Reads + cost.Content + cost.Access; total > gh.RequestsPerHour {
		// The primary limit counts every request, reads included, and a run's
		// reads outnumber its writes several times over. This is the ceiling a
		// large class meets first: the run pauses until the hour resets and then
		// finishes, but that is an hour the instructor should plan for rather
		// than discover.
		fmt.Fprintf(out, "  note: about %d requests, and GitHub allows %d an hour, so this run will pause partway until the hour resets\n", total, gh.RequestsPerHour)
	}
	if cost.Content > gh.ContentPerHour {
		// Whether this ceiling governs repository generation is unproven, and
		// nothing paces to it: a run that met it would pause and carry on rather
		// than fail. An instructor watching a class provision should know it may.
		fmt.Fprintf(out, "  note: about %d content-creating requests, and GitHub allows %d an hour, so this run may pause\n", cost.Content, gh.ContentPerHour)
	}
}

// existingRepos reports which of the assignment's repositories are already in
// the org, by name. One listing covers the whole assignment.
func existingRepos(ctx context.Context, client assignClient, org, name string) (map[string]bool, error) {
	repos, err := client.ListOrgReposByPrefix(ctx, org, name+"-")
	if err != nil {
		return nil, fmt.Errorf("listing existing %s-* repositories in %s: %w", name, org, err)
	}
	out := make(map[string]bool, len(repos))
	for _, r := range repos {
		out[strings.ToLower(r.Name)] = true
	}
	return out, nil
}

func countExisting(units []unit.Unit, existing map[string]bool, name string) int {
	n := 0
	for _, u := range units {
		if existing[strings.ToLower(name+"-"+u.Key)] {
			n++
		}
	}
	return n
}

// Per-unit request counts, taken from real runs rather than read off the code.
// The two disagreed by a factor of two on reads, because half of a run's reads
// are polls waiting for GitHub to catch up with its own writes, and counting
// call sites misses every repeat.
const (
	unitReads  = 3 // the repo check, then the two reads that verify the grants
	unitAccess = 1 // the staff team grant; every member adds another
	// A repo being created is polled until its default branch lands. The poll
	// waits before looking, so that is one round.
	newRepoReads   = 2
	newRepoContent = 1
	// The feedback artifact. The issue is one create plus the single check that
	// confirms it is listable, the guard before it being provably unnecessary on
	// a repo this run generated. A template with issues disabled costs one more
	// content request to enable them.
	issueFeedbackContent = 1
	issueFeedbackReads   = 1
	// The pull request has no run behind it: these are its calls on the same path
	// (the base rebuild, its ref check, the branch, the PR), with the
	// ref-consistency poll left at the depth the issue path showed, since that
	// poll is the one still unmeasured.
	prFeedbackContent = 5
	prFeedbackReads   = 6
	// An existing repo is asked which artifact it already carries instead.
	existingFeedbackReads = 2
	rulesetContent        = 1
)

// runCost estimates the requests provisioning these units will make, so the plan
// can state how long the run takes before it changes anything. It is an estimate
// by construction (see the counts above); it exists to tell an instructor whether
// this is a five-minute or a two-hour operation.
func runCost(units []unit.Unit, existing map[string]bool, name string, policy config.Policy) gh.Cost {
	var c gh.Cost
	for _, u := range units {
		c.Reads += unitReads
		c.Access += unitAccess + len(u.Members)

		if existing[strings.ToLower(name+"-"+u.Key)] {
			if policy.Feedback != config.FeedbackNone {
				c.Reads += existingFeedbackReads
			}
			continue
		}
		c.Reads += newRepoReads
		c.Content += newRepoContent
		switch policy.Feedback {
		case config.FeedbackPR:
			c.Content += prFeedbackContent
			c.Reads += prFeedbackReads
		case config.FeedbackIssue:
			c.Content += issueFeedbackContent
			c.Reads += issueFeedbackReads
		}
		if policy.BranchProtection {
			c.Content += rulesetContent
		}
	}
	return c
}

// roundDuration renders an estimate at the precision it deserves: whole seconds
// under a minute, whole minutes above, since the counts behind it are a floor
// and a figure like "4m37s" would claim an accuracy it does not have.
func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// planResult is one unit's dry-run outcome: what a real run would do to its
// repository, read from the same state that run would act on.
type planResult struct {
	repo    string
	members []string
	create  bool
	grant   string   // "push", or "read" on a repo recorded frozen
	notes   []string // why it reads the way it does
	abort   string   // non-empty: a real run would refuse this repo, and why
	err     error    // this unit's plan could not be read at all
}

// planUnit reports what provisioning one unit would do. It reads the same state
// provision acts on: whether the repository already exists (create or skip), its
// visibility against the policy (which decides whether the repo is acted on at
// all), and its recorded freeze state (push or read). Reporting "would create"
// for an existing repo, push for a frozen one, or a routine skip for a repo the
// run would refuse to touch is exactly the kind of wrong answer a dry run is run
// to avoid.
func planUnit(ctx context.Context, client assignClient, org, name string, policy config.Policy, frozen map[string]freezeState, u unit.Unit) planResult {
	repo := name + "-" + u.Key
	res := planResult{repo: repo, members: u.Members, grant: "push"}

	info, exists, err := client.GetRepo(ctx, org, repo)
	if err != nil {
		res.err = fmt.Errorf("checking %s: %w", repo, err)
		return res
	}
	res.create = !exists
	if exists {
		res.notes = append(res.notes, "exists")
		// A repo whose visibility disagrees with the policy is aborted by a real
		// run before any grant, since a private assignment that is public exposes
		// student work. A freshly generated repo is created at the right
		// visibility, so only an existing one can be in this state.
		res.abort = visibilityMismatch(info, policy.Public)
	}
	if state := frozen[repo]; state.grantPermission() == "pull" {
		res.grant = "read"
		res.notes = append(res.notes, "recorded "+state.describe())
	}
	return res
}

// reportPlan prints the per-unit plan as an aligned table and returns an error
// if any unit could not be read: a plan missing rows is not a plan.
func reportPlan(out io.Writer, plans []planResult) error {
	var create, skip, abort, failed int
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, p := range plans {
		if p.err != nil {
			failed++
			fmt.Fprintf(tw, "  FAILED\t%s\t%v\n", p.repo, p.err)
			continue
		}
		if p.abort != "" {
			abort++
			fmt.Fprintf(tw, "  ABORT\t%s\t%s, so a run would refuse it before granting access\n", p.repo, p.abort)
			continue
		}
		action := "skip"
		if p.create {
			action = "create"
			create++
		} else {
			skip++
		}
		detail := fmt.Sprintf("%s: %s", p.grant, strings.Join(p.members, ", "))
		if len(p.notes) > 0 {
			detail += " (" + strings.Join(p.notes, ", ") + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", action, p.repo, detail)
	}
	tw.Flush()

	fmt.Fprintf(out, "\n%d would be created, %d already exist", create, skip)
	if abort > 0 {
		fmt.Fprintf(out, ", %d would be refused", abort)
	}
	fmt.Fprintln(out)

	switch {
	case failed > 0:
		return fmt.Errorf("%d repo(s) could not be checked, so this plan is incomplete", failed)
	case abort > 0:
		return fmt.Errorf("%d repo(s) would be refused as they stand; fix their visibility before running assign", abort)
	}
	return nil
}

// planExtras describes the per-repo protection/feedback a run would add.
func planExtras(policy config.Policy) string {
	var parts []string
	if policy.BranchProtection {
		parts = append(parts, "an all-branches protection ruleset")
	}
	switch policy.Feedback {
	case config.FeedbackPR:
		parts = append(parts, "a feedback pull request")
	case config.FeedbackIssue:
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

// checkRosterUsers verifies every unit member is a real GitHub account before any
// repository is created. Members are deduped case-insensitively (a username may
// appear on several units) and checked concurrently. Every non-existent handle is
// collected and reported together so the whole roster can be fixed in one pass,
// and a lookup error (not a plain "not found") aborts rather than being mistaken
// for a bad username.
func checkRosterUsers(ctx context.Context, client assignClient, concurrency int, units []unit.Unit) error {
	seen := make(map[string]string)
	for _, u := range units {
		for _, m := range u.Members {
			key := strings.ToLower(m)
			if _, ok := seen[key]; !ok {
				seen[key] = m
			}
		}
	}
	usernames := make([]string, 0, len(seen))
	for _, m := range seen {
		usernames = append(usernames, m)
	}
	sort.Strings(usernames)

	type check struct {
		user    string
		missing bool
		err     error
	}
	results := runConcurrent(ctx, concurrency, usernames, func(ctx context.Context, user string) check {
		exists, err := client.UserExists(ctx, user)
		return check{user: user, missing: !exists, err: err}
	})

	var missing []string
	for _, r := range results {
		if r.err != nil {
			return fmt.Errorf("validating roster username %q: %w", r.user, r.err)
		}
		if r.missing {
			missing = append(missing, r.user)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("roster has %d GitHub username(s) that do not exist: %s; fix the roster before assigning (no repositories were created)",
			len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// provision creates (or reuses) one repository and asserts its access grants.
// Branch protection is applied once, when the repo is first created; the
// feedback artifact is reconciled on every run so a partial failure is repaired
// on re-run without reopening a closed PR or issue.
func (o *assignOpts) provision(ctx context.Context, client assignClient, org, name, tmplOwner, tmplName, staffTeam string, policy config.Policy, frozen map[string]freezeState, u unit.Unit) unitResult {
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
	// access, on a freshly generated repo and on a reused one alike. A private
	// assignment that came out (or has since drifted) public would expose student
	// work, so abort this repo rather than (re-)assert access on a leaky one.
	if err := checkVisibility(repo, info, policy.Public); err != nil {
		// A repo we just generated with the wrong visibility is our own leaky
		// artifact: no access has been granted yet, so roll it back rather than
		// leave a wrongly-public repo behind. A reused repo is never deleted (it
		// may already hold student work), so it is only reported.
		if res.status == "created" {
			if delErr := client.DeleteRepo(ctx, org, repo); delErr != nil {
				res.err = fmt.Errorf("%w; additionally, rolling back the leaked repo failed; delete %s/%s manually: %v", err, org, repo, delErr)
				return res
			}
			res.err = fmt.Errorf("%w (rolled back the just-created repo)", err)
			return res
		}
		res.err = err
		return res
	}

	// Feedback is set up before grants and branch protection: on a freshly created
	// pr repo it reshapes the default branch (a force update), which ApplyRuleset
	// would block and which must happen before anyone can push. Each piece is
	// created only if missing, so a partial failure on a prior run is repaired.
	if err := o.addFeedback(ctx, client, org, repo, info, policy.Feedback, res.status == "created"); err != nil {
		res.err = err
		return res
	}

	// Re-assert grants so re-running is safe and access is correct. "Correct" is
	// the repo's recorded freeze state, not always push: a repo frozen at its
	// deadline must stay read-only through a later assign, or re-running this to
	// add one late student would hand write back to the whole assignment. A repo
	// just created has no record and so gets push, which is right.
	grant := frozen[repo].grantPermission()
	if grant != "push" {
		res.frozen = true
	}
	for _, member := range u.Members {
		if err := client.AddCollaborator(ctx, org, repo, member, grant); err != nil {
			res.err = fmt.Errorf("granting %s to %s on %s: %w", grant, member, repo, err)
			return res
		}
		if grant == "push" {
			res.grantedWrite = true
		}
	}
	if err := client.AddTeamRepo(ctx, org, staffTeam, org, repo, "push"); err != nil {
		res.err = fmt.Errorf("granting staff team on %s: %w", repo, err)
		return res
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

	// Post-condition: confirm every member actually holds the access we granted.
	// A grant to a non-member becomes a GitHub invitation that conveys no access
	// until accepted, so the only honest end state is "holds the granted
	// permission, or has a pending invitation"; a member who is neither means the
	// grant silently did not land.
	pending, err := o.verifyAccess(ctx, client, org, repo, grant, u.Members)
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
	mismatch := visibilityMismatch(info, wantPublic)
	if mismatch == "" {
		return nil
	}
	return fmt.Errorf("repository %s is %s; aborting before asserting access", repo, mismatch)
}

// visibilityMismatch describes how a repo's visibility disagrees with the
// policy ("public but private was requested"), or "" when they agree. The
// phrase is shared by the error that aborts a repo and by the dry run's report
// of the same repo, so a preview cannot describe the state differently from the
// run it is previewing.
func visibilityMismatch(info *gh.Repo, wantPublic bool) string {
	// Compare like polarities (Private vs Private) rather than info.Private
	// against wantPublic, whose opposite meanings make the check easy to misread.
	wantPrivate := !wantPublic
	if info.Private == wantPrivate {
		return ""
	}
	actual, want := "private", "private"
	if !info.Private {
		actual = "public"
	}
	if wantPublic {
		want = "public"
	}
	return fmt.Sprintf("%s but %s was requested", actual, want)
}

// verifyAccess re-reads a repo's collaborators and pending invitations and
// confirms every granted member holds the access the grant conferred, or has a
// pending invitation. A "push" grant requires the collaborator to CanPush; a
// "pull" grant (a frozen repo) requires only that they hold pull, since every
// direct collaborator holds at least that. A member who is neither means the
// grant silently failed, which is a loud error. The pending invitees are
// returned so the run can report that they must still accept.
func (o *assignOpts) verifyAccess(ctx context.Context, client assignClient, org, repo, grant string, members []string) ([]string, error) {
	collaborators, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("verifying access on %s: %w", repo, err)
	}
	hasAccess := make(map[string]bool, len(collaborators))
	for _, c := range collaborators {
		live := c.CanPush()
		if grant == "pull" {
			live = c.Permissions.Pull
		}
		if live {
			hasAccess[strings.ToLower(c.Login)] = true
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
		case hasAccess[key]:
			// access is live
		case invited[key]:
			pending = append(pending, m)
		default:
			return nil, fmt.Errorf("%s grant to %s on %s did not take effect: they are neither a collaborator nor have a pending invitation; re-run assign to repair it", grant, m, repo)
		}
	}
	return pending, nil
}

// addFeedback ensures the chosen feedback artifact exists, creating only the
// pieces that are missing. This makes it safe to call on every run: a partial
// failure is repaired, while an existing (even closed) PR or issue is left be.
func (o *assignOpts) addFeedback(ctx context.Context, client assignClient, org, repo string, info *gh.Repo, mode string, created bool) error {
	// Find out what an existing repo already carries before creating anything.
	// The mode says what this run intends, not what is there, and the two diverge
	// the moment an assignment's feedback setting is changed after its repos were
	// made. Creating on top of the other kind would leave that repo with two
	// artifacts, the class split between them, and no way to tell which one a
	// student read. A repo generated by this run carries neither, so it is not
	// worth the lookups.
	if !created && mode != config.FeedbackNone {
		existing, found, err := findExisting(ctx, client, org, repo)
		if err != nil {
			return err
		}
		if found {
			if existing.mode != mode {
				return fmt.Errorf("%s already has a feedback %s (#%d), but this assignment is now configured for a feedback %s; assign will not add a second one. Set the assignment's feedback back to %q, or close the existing %s on the repos that should change and re-run",
					repo, artifactNoun(existing.mode), existing.number, artifactNoun(mode), existing.mode, artifactNoun(existing.mode))
			}
			return nil // the repo already has the artifact this run would create
		}
	}

	switch mode {
	case config.FeedbackPR:
		// The feedback PR shows the whole project as additions so staff can comment
		// on any line, including unchanged starter code. That needs the PR's base to
		// share history with the default branch (GitHub refuses a PR between
		// branches with no common ancestor) yet have an empty merge base with it.
		// Neither a branch pinned at the starter commit (identical to the default
		// branch -> "No commits between") nor a detached orphan (no shared history
		// -> "no history in common") gives both. So, mirroring GitHub Classroom,
		// rebuild the freshly generated repo's initial commit on top of an empty
		// root and point the feedback branch at that root: the default branch now
		// descends from it (the PR opens) and their merge base is empty.
		// A repository this run generated carries no feedback branch, unless
		// --all-branches copied one from the template, so on the default path the
		// answer is known before asking and the request is pure waste. On a
		// class-sized run these guards are hundreds of requests.
		branchExists := false
		if !created || o.allBranches {
			var err error
			if branchExists, err = client.BranchExists(ctx, org, repo, feedbackBranch); err != nil {
				return fmt.Errorf("checking feedback branch on %s: %w", repo, err)
			}
		}
		if !branchExists {
			if !created {
				// Building the base force-updates the default branch, a history
				// rewrite only safe on a repo with no student work. Never do it to an
				// existing repo.
				//
				// Which existing repo this is decides the way out, and the tool cannot
				// tell the two apart: a repo left by an assign run that died in the
				// window between generating it and creating this branch provably holds
				// no student work, since grants come after this step, while a repo made
				// by an earlier run without feedback may hold a term of it. So the
				// message states the test rather than picking for the instructor. It is
				// also why this is not auto-repaired on a repo with no collaborators:
				// acting on that inference destroys the work if it is ever wrong.
				return fmt.Errorf("feedback branch missing on existing repo %s/%s, and building it force-updates the default branch, so it is refused on a repo that already exists. "+
					"If this repo is left over from an interrupted assign run it holds no student work, since the branch is built before anyone is granted access: confirm no student is a collaborator on it, then `gh repo delete %s/%s` and re-run assign. "+
					"If it does hold student work, do not delete it: restore the feedback branch from its pull request if the repo once had one, or, if it never had one, give this repo an issue instead by re-running with --feedback issue and a roster naming only its student.",
					org, repo, org, repo)
			}
			root, err := client.RebaseOntoEmptyRoot(ctx, org, repo, info.DefaultBranch)
			if err != nil {
				return fmt.Errorf("preparing feedback base on %s: %w", repo, err)
			}
			if err := client.CreateRef(ctx, org, repo, "refs/heads/"+feedbackBranch, root); err != nil {
				return fmt.Errorf("creating feedback branch on %s: %w", repo, err)
			}
		}
		// Generating from a template copies no pull requests, so a repository this
		// run created provably has none.
		prExists := false
		if !created {
			var err error
			if prExists, err = client.PRExists(ctx, org, repo, feedbackBranch); err != nil {
				return fmt.Errorf("checking feedback PR on %s: %w", repo, err)
			}
		}
		if !prExists {
			if err := client.CreatePR(ctx, org, repo, feedbackTitle, info.DefaultBranch, feedbackBranch, feedbackPRBody); err != nil {
				return fmt.Errorf("opening feedback PR on %s: %w", repo, err)
			}
		}
	case config.FeedbackIssue:
		if !info.HasIssues {
			if err := client.EnableIssues(ctx, org, repo); err != nil {
				return fmt.Errorf("enabling issues on %s: %w", repo, err)
			}
		}
		// Generating from a template copies no issues either.
		issueExists := false
		if !created {
			var err error
			if issueExists, err = client.IssueExists(ctx, org, repo, feedbackTitle); err != nil {
				return fmt.Errorf("checking feedback issue on %s: %w", repo, err)
			}
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
	var created, skipped, failed, pending, frozen int
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
		case r.status == "skipped":
			skipped++
		default:
			created++
		}
		if r.frozen {
			frozen++
		}
		pending += len(r.pending)
	}
	fmt.Fprintf(out, "\n%d created, %d skipped, %d failed\n", created, skipped, failed)
	if pending > 0 {
		// Outside collaborators must accept the invitation before they have access;
		// until then the repo is provisioned but the student cannot push.
		fmt.Fprintf(out, "note: %d student invitation(s) are still pending; those students must accept the GitHub invitation before they can push\n", pending)
	}
	if frozen > 0 {
		// The opposite of the old hazard: these repos keep their deadline lock
		// through the re-assert, so say so rather than leaving the instructor to
		// wonder why those students cannot push.
		fmt.Fprintf(out, "note: %d existing repo(s) are recorded frozen, so their members were granted read, not write\n", frozen)
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
