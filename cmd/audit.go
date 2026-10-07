package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
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
	RemoveCollaborator(ctx context.Context, owner, repo, username string) error
	DeleteRepoInvitation(ctx context.Context, owner, repo string, id int64) error
	UpdateRepoInvitation(ctx context.Context, owner, repo string, id int64, permission string) (bool, error)
	GetPropertyDefinition(ctx context.Context, org, name string) (*gh.PropertyDefinition, bool, error)
	ListRepoPropertyValues(ctx context.Context, org string) (map[string]map[string]string, error)
}

// memberStatus is one expected student's actual access state on their repo.
type memberStatus int

const (
	statusOnRepo  memberStatus = iota // accepted: a direct collaborator with write access
	statusFrozen                      // accepted: a direct collaborator holding read, as a freeze leaves them
	statusPending                     // invited, invitation not yet expired
	statusExpired                     // invited, but the invitation has expired
	statusMissing                     // repo exists but the student has no access or invitation
	statusNoRepo                      // the expected repo does not exist
	statusDropped                     // marked dropped in the roster, holding no more than the roster allows
	statusExcess                      // marked dropped in the roster, but holding more than it allows
)

// settled reports whether the student needs no action: they are on their repo,
// either writable or frozen, or they dropped and hold no more than the roster
// allows. These are correct end states, so none is listed by default nor picked
// up by --renew or --revoke.
func (s memberStatus) settled() bool {
	return s == statusOnRepo || s == statusFrozen || s == statusDropped
}

func (s memberStatus) label() string {
	switch s {
	case statusOnRepo:
		return "on repo"
	case statusFrozen:
		return "on repo (frozen)"
	case statusPending:
		return "invited (pending)"
	case statusExpired:
		return "invited (EXPIRED)"
	case statusMissing:
		return "MISSING"
	case statusNoRepo:
		return "NO REPO"
	case statusDropped:
		return "dropped"
	case statusExcess:
		return "DROPPED"
	}
	return "?"
}

// auditOpts carries the resolved flags and dependencies for `gh cls audit`.
type auditOpts struct {
	g         *globalOpts
	roster    string
	groups    string
	all       bool
	renew     bool
	revoke    bool
	dryRun    bool
	newClient func(context.Context) (auditClient, error)
}

func newAuditCmd(g *globalOpts) *cobra.Command {
	o := &auditOpts{
		g:         g,
		newClient: func(context.Context) (auditClient, error) { return g.client() },
	}
	cmd := &cobra.Command{
		Use:   "audit <name>",
		Short: "Reconcile who should be on each assignment repo against who actually is",
		Long: `Compare the students who should have access to the <name>-* repos (resolved
from the roster, and the groups file for a group assignment) against the actual
state on GitHub, reporting each student as one of: on repo (accepted), invited
(pending), invited (EXPIRED), MISSING (the repo exists but they have neither
access nor an invitation), or NO REPO (the repo was never created). It also
flags any access that is present but not expected.

For a group assignment it also warns (but never aborts) when the groups file
leaves an enrolled student in no group or puts one in more than one group -- the
same inconsistencies assign refuses to create repos for without --force.

Students are added as outside collaborators, so a grant becomes an invitation
they must accept within seven days; --renew re-issues access for everyone whose
invitation expired or who is missing entirely (it never removes access).

A student who drops is marked in the roster's optional access column: read
keeps read access everywhere, none removes them everywhere, and own keeps read
on individual assignments and removes them from group ones. Audit reports a
dropped student holding more than that as DROPPED on any repo they hold access
to, whether or not the groups file still puts them there, and --renew never
grants them anything. --revoke takes the excess away (it never grants access):
it reads each repo of the assignment once for all the dropped students
together, downgrades or removes their access and pending invitations, and
re-reads each repo it changed to confirm.`,
		Example: `  gh cls audit hw1 --roster roster.csv
  gh cls audit project --roster roster.csv --groups groups.yml
  gh cls audit hw1 --roster roster.csv --renew
  gh cls audit hw1 --roster roster.csv --revoke -n`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.roster, "roster", "r", "", "path to the roster CSV (required)")
	f.StringVarP(&o.groups, "groups", "g", "", "path to the groups file (required for group, rejected for individual)")
	f.BoolVar(&o.all, "all", false, "list every student, including those already on their repo")
	f.BoolVar(&o.renew, "renew", false, "re-issue access for expired or missing students")
	f.BoolVar(&o.revoke, "revoke", false, "take away access dropped students hold beyond the roster's access column")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "with --renew or --revoke, show what would change without doing it")
	_ = cmd.MarkFlagRequired("roster")
	cmd.MarkFlagsMutuallyExclusive("renew", "revoke")
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
	// retain and held describe a dropped student: the most access the roster
	// allows them on this repo, and what they actually hold there.
	retain unit.Retain
	held   string
}

// label renders the status, spelling out a dropped student's access, since
// "dropped" alone does not say whether they were left read or removed.
func (m memberAudit) label() string {
	switch m.status {
	case statusDropped:
		return fmt.Sprintf("dropped (%s)", m.held)
	case statusExcess:
		return fmt.Sprintf("DROPPED (holds %s, allowed %s)", m.held, retainLabel(m.retain))
	}
	return m.status.label()
}

// retainLabel renders what a dropped student may keep.
func retainLabel(r unit.Retain) string {
	if r == unit.RetainRead {
		return "read"
	}
	return "nothing"
}

// collabExceeds reports whether a collaborator holds more than a dropped student
// may keep. Admin counts: a student should never hold it, and the excess is
// reported even though --revoke refuses to change admin access.
func collabExceeds(c gh.Collaborator, r unit.Retain) bool {
	if r == unit.RetainRead {
		return c.Permissions.Admin || c.AboveRead()
	}
	return c.Permissions.Admin || c.AboveRead() || c.Permissions.Pull
}

// invitationExceeds reports whether accepting an invitation would give a dropped
// student more than they may keep. An expired invitation can no longer be
// accepted, so it never exceeds.
func invitationExceeds(inv gh.Invitation, r unit.Retain) bool {
	if inv.Expired {
		return false
	}
	return r == unit.RetainNothing || inv.Permissions != gh.InvitationRead
}

// droppedHeld classifies a dropped student's access on a repo from its
// collaborators and invitations: whether it exceeds what they may keep, and a
// description of what they hold.
func droppedHeld(login string, r unit.Retain, collabs []gh.Collaborator, invs []gh.Invitation) (exceeds bool, held string) {
	held = "no access"
	for _, inv := range invs {
		if !strings.EqualFold(inv.Invitee.Login, login) || inv.Expired {
			continue
		}
		held = "an invitation (" + inv.Permissions + ")"
		if invitationExceeds(inv, r) {
			exceeds = true
		}
	}
	for _, c := range collabs {
		if !strings.EqualFold(c.Login, login) {
			continue
		}
		held = collabPermission(c)
		if collabExceeds(c, r) {
			exceeds = true
		}
	}
	return exceeds, held
}

// collabPermission names a collaborator's highest permission.
func collabPermission(c gh.Collaborator) string {
	switch {
	case c.Permissions.Admin:
		return "admin"
	case c.Permissions.Maintain:
		return "maintain"
	case c.Permissions.Push:
		return "write"
	case c.Permissions.Triage:
		return "triage"
	case c.Permissions.Pull:
		return "read"
	}
	return "no access"
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

	units, report, r, err := loadUnits(name, policy.Type, o.roster, o.groups)
	if err != nil {
		return err
	}
	printUnitWarnings(out, report)
	byUser := r.ByUsername()

	// Every dropped student, from the roster alone. A unit lists only the dropped
	// students its groups file still puts on it, and removing a dropped student
	// from their group is the natural thing to do, so audit recognizes them on any
	// repo they turn up on rather than only where the groups file says.
	dropped := make(map[string]unit.DroppedMember)
	for _, d := range unit.DroppedStudents(policy.Type, r) {
		dropped[strings.ToLower(d.Login)] = d
	}
	if o.revoke && len(dropped) == 0 {
		return fmt.Errorf("--revoke: no student in %s is marked as dropped; set their access column to read, none or own first", o.roster)
	}

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
	repos = filterAssignmentRepos(o.g.cfg, name, repos)
	exists := make(map[string]bool, len(repos))
	for _, rp := range repos {
		exists[rp.Name] = true
	}

	// --revoke reads every repo of the assignment once and checks it for all the
	// dropped students together: two requests per repo however many dropped, and
	// independent of what the groups file still says. A template is never a
	// student's repo, so it is not read.
	if o.revoke {
		units = nil
		for _, rp := range repos {
			if !rp.IsTemplate {
				units = append(units, unit.Unit{Key: strings.TrimPrefix(rp.Name, name+"-")})
			}
		}
	}

	results := runConcurrent(ctx, o.g.concurrency, units, func(ctx context.Context, u unit.Unit) repoAudit {
		return o.auditUnit(ctx, client, org, name, u, byUser, dropped, exists)
	})

	if o.revoke {
		return o.runRevoke(ctx, out, client, org, results)
	}
	if o.renew {
		return o.runRenew(ctx, out, client, org, name, results)
	}
	return reportAudit(out, org, name, o.all, results)
}

// auditUnit classifies one expected repo: each member's status, plus any access
// present that the assignment did not expect.
func (o *auditOpts) auditUnit(ctx context.Context, client auditClient, org, name string, u unit.Unit, byUser map[string]string, dropped map[string]unit.DroppedMember, exists map[string]bool) repoAudit {
	repo := name + "-" + u.Key
	res := repoAudit{repo: repo}

	if !exists[repo] {
		for _, m := range u.Members {
			res.members = append(res.members, memberAudit{id: byUser[strings.ToLower(m)], login: m, status: statusNoRepo})
		}
		// A dropped student with no repo holds nothing on it, which is within any
		// access the roster allows, and assign will not create one for them.
		for _, d := range u.Dropped {
			res.members = append(res.members, memberAudit{id: byUser[strings.ToLower(d.Login)], login: d.Login, status: statusDropped, retain: d.Retain, held: "no repo"})
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
	readAccess := map[string]bool{}
	isAdmin := map[string]bool{}
	for _, c := range collabs {
		l := strings.ToLower(c.Login)
		if c.Permissions.Admin {
			isAdmin[l] = true
		}
		// Read is tracked separately from write because it is where a freeze leaves
		// a student. Folding it in with "no access" would report a correctly frozen
		// class as MISSING and hand every one of them to --renew.
		if c.CanPush() {
			writeAccess[l] = true
		} else if c.Permissions.Pull {
			readAccess[l] = true
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
		case readAccess[l]:
			ma.status = statusFrozen
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
	// A dropped student is expected too, so their access is judged against what
	// the roster allows them rather than listed again as unexpected.
	for _, d := range u.Dropped {
		l := strings.ToLower(d.Login)
		expected[l] = true
		exceeds, held := droppedHeld(d.Login, d.Retain, collabs, invs)
		ma := memberAudit{id: byUser[l], login: d.Login, status: statusDropped, retain: d.Retain, held: held}
		if exceeds {
			ma.status = statusExcess
		}
		res.members = append(res.members, ma)
	}
	// A dropped student the unit does not list, typically one taken out of their
	// group, is still judged against what the roster allows them wherever they
	// hold access, rather than reported as a stranger.
	other := func(login string) {
		l := strings.ToLower(login)
		d, ok := dropped[l]
		if !ok || expected[l] {
			return
		}
		expected[l] = true
		exceeds, held := droppedHeld(d.Login, d.Retain, collabs, invs)
		ma := memberAudit{id: byUser[l], login: d.Login, status: statusDropped, retain: d.Retain, held: held}
		if exceeds {
			ma.status = statusExcess
		}
		res.members = append(res.members, ma)
	}
	for _, c := range collabs {
		other(c.Login)
	}
	for _, inv := range invs {
		other(inv.Invitee.Login)
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
			if m.status.settled() && !showAll {
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.repo, dash(m.id), m.login, m.label())
			shown++
		}
	}
	if shown > 0 {
		tw.Flush()
	}
	dropped := counts[statusDropped] + counts[statusExcess]
	if shown == 0 {
		note := ""
		if counts[statusFrozen] > 0 {
			note = fmt.Sprintf(" (%d frozen)", counts[statusFrozen])
		}
		fmt.Fprintf(out, "All %d student(s) are on their repos%s.\n", students-dropped, note)
		if dropped > 0 {
			fmt.Fprintf(out, "%d dropped student(s) hold no more access than the roster allows.\n", dropped)
		}
	}

	// Frozen and dropped appear only when there is one, so an assignment that has
	// never been frozen and has lost no one (the common case) reads exactly as
	// before rather than carrying a permanent "0 frozen".
	frozen := ""
	if counts[statusFrozen] > 0 {
		frozen = fmt.Sprintf("%d frozen, ", counts[statusFrozen])
	}
	droppedNote := ""
	if dropped > 0 {
		droppedNote = fmt.Sprintf(", %d dropped", dropped)
	}
	fmt.Fprintf(out, "\nSummary: %d on repo, %s%d pending, %d expired, %d missing, %d without a repo%s (across %d repo(s), %d student(s)).\n",
		counts[statusOnRepo], frozen, counts[statusPending], counts[statusExpired], counts[statusMissing], counts[statusNoRepo], droppedNote, repos, students)

	if action := counts[statusExpired] + counts[statusMissing]; action > 0 {
		fmt.Fprintf(out, "Action needed: %d expired + %d missing; re-issue with `gh cls audit %s --roster <file> --renew`.\n",
			counts[statusExpired], counts[statusMissing], name)
	}
	if counts[statusExcess] > 0 {
		fmt.Fprintf(out, "Action needed: %d dropped student(s) hold more access than the roster allows; remove it with `gh cls audit %s --roster <file> --revoke`.\n",
			counts[statusExcess], name)
	}
	if counts[statusNoRepo] > 0 {
		fmt.Fprintf(out, "Note: %d student(s) have no repo yet; run `gh cls assign %s` to create them.\n", counts[statusNoRepo], name)
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
	fmt.Fprintln(out, "\nUnexpected access (not in the roster/groups for this assignment):")
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
	// granted records that the collaborator add landed. Like assign's
	// grantedWrite it is independent of err: a grant whose verification then
	// failed still handed out the access.
	granted bool
	err     error
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

	// A student being renewed has no access on their repo, which is why they are
	// here, so their own repo carries no permission to read the deadline state
	// from. The recorded freeze state is what says whether to restore write or
	// read; without it a renew after a deadline would hand push back.
	frozen, err := readFrozenStates(ctx, client, org)
	if err != nil {
		return fmt.Errorf("aborting --renew: %w", err)
	}

	type job struct {
		repo, login string
		invID       int64 // non-zero => cancel this expired invitation first
		permission  string
	}
	var jobs []job
	noRepo := 0
	restoringRead := 0
	for _, r := range results {
		perm := frozen[r.repo].grantPermission()
		for _, m := range r.members {
			switch m.status {
			case statusExpired:
				jobs = append(jobs, job{r.repo, m.login, m.invID, perm})
			case statusMissing:
				jobs = append(jobs, job{r.repo, m.login, 0, perm})
			case statusNoRepo:
				noRepo++
				continue
			default:
				continue
			}
			if perm == "pull" {
				restoringRead++
			}
		}
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sRe-issuing access for %d student(s) in %s\n", prefix, len(jobs), org)
	if restoringRead > 0 {
		fmt.Fprintf(out, "note: %d of them are on frozen repos and are being restored to read, not write\n", restoringRead)
	}
	if noRepo > 0 {
		fmt.Fprintf(out, "note: %d student(s) have no repo to renew on; run `gh cls assign %s` first\n", noRepo, name)
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
		if err := client.AddCollaborator(ctx, org, j.repo, j.login, j.permission); err != nil {
			r.err = fmt.Errorf("re-inviting %s on %s with %s (an expired invitation, if any, was already cancelled; re-run `gh cls assign %s` if access is now absent): %w", j.login, j.repo, j.permission, name, err)
			return r
		}
		r.granted = true
		if err := o.verifyRenewed(ctx, client, org, j.repo, j.login, j.permission); err != nil {
			r.err = err
			return r
		}
		return r
	})

	// Post-condition, matching assign: a freeze that landed while these grants were
	// in flight would leave repos writable past their deadline, and the record read
	// above could not have seen it. What counts is the grant having landed, not the
	// job having finished cleanly, since a write handed out before a later step
	// failed is still a write.
	var grantedWrite []string
	if !o.dryRun {
		for i, j := range jobs {
			if j.permission == "push" && res[i].granted {
				grantedWrite = append(grantedWrite, j.repo)
			}
		}
	}
	reopened, raceErr := checkGrantRace(ctx, client, org, grantedWrite)

	// Report both and return both: a failed renewal must not swallow the news that
	// other repos are writable past their deadline.
	errs := []error{reportRenew(out, o.dryRun, res)}
	switch {
	case raceErr != nil:
		errs = append(errs, raceErr)
	case len(reopened) > 0:
		errs = append(errs, grantRaceError(name, reopened))
	}
	return errors.Join(errs...)
}

// verifyRenewed confirms a re-issued student now holds the access they were
// granted, or has a fresh (non-expired) invitation. A 200 on the add is not
// proof; this is the post-condition that the access was actually restored. On a
// frozen repo the grant is read, so requiring push here would fail every renew
// after a deadline.
func (o *auditOpts) verifyRenewed(ctx context.Context, client auditClient, org, repo, login, permission string) error {
	collabs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s on %s after re-inviting: %w", login, repo, err)
	}
	for _, c := range collabs {
		if !strings.EqualFold(c.Login, login) {
			continue
		}
		if permission == "pull" && c.Permissions.Pull {
			return nil
		}
		if permission != "pull" && c.CanPush() {
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
