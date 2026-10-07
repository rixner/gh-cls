package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/unit"
)

// revokeResult records the outcome of taking excess access from one dropped
// student on one repo.
type revokeResult struct {
	repo   string
	login  string
	retain unit.Retain
	err    error
}

// runRevoke takes away the access each dropped student holds beyond what the
// roster allows them. Like --renew it refuses to act on a partial picture, and it
// refuses outright if any of them holds admin, which this tool never changes.
func (o *auditOpts) runRevoke(ctx context.Context, out io.Writer, client auditClient, org string, results []repoAudit) error {
	for _, r := range results {
		if r.err != nil {
			return fmt.Errorf("aborting --revoke: %s could not be audited, so the set to revoke is incomplete: %w", r.repo, r.err)
		}
	}

	type job struct {
		repo string
		m    memberAudit
	}
	var jobs []job
	var admins []string
	settled := 0
	for _, r := range results {
		for _, m := range r.members {
			switch m.status {
			case statusExcess:
				jobs = append(jobs, job{r.repo, m})
				if m.held == "admin" {
					admins = append(admins, m.login+" on "+r.repo)
				}
			case statusDropped:
				settled++
			}
		}
	}
	// Pre-condition: admin is staff access, and gh cls never grants or removes it,
	// so a dropped student holding it is a mistake for a person to look at. Checked
	// before the first change, so the run does not revoke some and stop partway.
	if len(admins) > 0 {
		return fmt.Errorf("aborting --revoke: %s hold(s) admin, which gh cls never changes; remove it in the repository's settings (Collaborators and teams) and re-run", strings.Join(admins, ", "))
	}

	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%sRevoking access for %d dropped student(s) in %s\n", prefix, len(jobs), org)
	if settled > 0 {
		fmt.Fprintf(out, "note: %d dropped student(s) already hold no more than the roster allows\n", settled)
	}
	if len(jobs) == 0 {
		fmt.Fprintln(out, "nothing to revoke")
		return nil
	}

	// The plan names who and where before anything changes, so a wrong row in the
	// roster shows up as the wrong person, and -n shows it without acting.
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  REPO\tUNIVERSITY ID\tGITHUB\tHOLDS\tKEEPS")
	for _, j := range jobs {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", j.repo, dash(j.m.id), j.m.login, j.m.held, retainLabel(j.m.retain))
	}
	tw.Flush()

	res := runConcurrent(ctx, o.g.concurrency, jobs, func(ctx context.Context, j job) revokeResult {
		r := revokeResult{repo: j.repo, login: j.m.login, retain: j.m.retain}
		if o.dryRun {
			return r
		}
		r.err = revokeOne(ctx, client, org, j.repo, j.m.login, j.m.retain)
		return r
	})
	return reportRevoke(out, o.dryRun, res)
}

// revokeOne reduces one dropped student's access on one repo to what they may
// keep, then confirms it took. It works from a fresh read rather than the
// audit's, since the student may have accepted an invitation in between.
//
// Invitations go first and the collaborator second, the order freeze uses: a
// student who accepts mid-run moves from the invitation list to the collaborator
// list, so doing collaborators first would leave a window in which they appear in
// neither and keep the access.
func revokeOne(ctx context.Context, client auditClient, org, repo, login string, retain unit.Retain) error {
	invs, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("listing invitations of %s: %w", repo, err)
	}
	for _, inv := range invs {
		if !strings.EqualFold(inv.Invitee.Login, login) || !invitationExceeds(inv, retain) {
			continue
		}
		if retain == unit.RetainNothing {
			// A 404 means the invitation was accepted since the listing; the
			// collaborator pass below removes them instead.
			if err := client.DeleteRepoInvitation(ctx, org, repo, inv.ID); err != nil && !gh.IsNotFound(err) {
				return fmt.Errorf("cancelling %s's invitation to %s: %w", login, repo, err)
			}
			continue
		}
		// A false result likewise means it was accepted, and is handled below.
		if _, err := client.UpdateRepoInvitation(ctx, org, repo, inv.ID, gh.InvitationRead); err != nil {
			return fmt.Errorf("downgrading %s's invitation to %s to read: %w", login, repo, err)
		}
	}

	collabs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("listing collaborators of %s (any invitation was already handled; re-run --revoke): %w", repo, err)
	}
	for _, c := range collabs {
		if !strings.EqualFold(c.Login, login) || !collabExceeds(c, retain) {
			continue
		}
		if c.Permissions.Admin {
			return fmt.Errorf("%s became an admin on %s during the run; gh cls never changes admin, so remove it in the repository's settings and re-run", login, repo)
		}
		if retain == unit.RetainNothing {
			if err := client.RemoveCollaborator(ctx, org, repo, c.Login); err != nil {
				return fmt.Errorf("removing %s from %s (any invitation was already cancelled; re-run --revoke): %w", login, repo, err)
			}
		} else if err := client.AddCollaborator(ctx, org, repo, c.Login, "pull"); err != nil {
			return fmt.Errorf("downgrading %s to read on %s (any invitation was already downgraded; re-run --revoke): %w", login, repo, err)
		}
	}
	return verifyRevoked(ctx, client, org, repo, login, retain)
}

// verifyRevoked is revokeOne's post-condition: a success response is not proof,
// so re-read the repo and confirm the student holds no more than they may keep.
func verifyRevoked(ctx context.Context, client auditClient, org, repo, login string, retain unit.Retain) error {
	collabs, err := client.ListDirectCollaborators(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s on %s after revoking: %w", login, repo, err)
	}
	invs, err := client.ListRepoInvitations(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("verifying %s on %s after revoking: %w", login, repo, err)
	}
	if exceeds, held := droppedHeld(login, retain, collabs, invs); exceeds {
		return fmt.Errorf("revoking %s on %s did not take: afterward they still hold %s; re-run `gh cls audit --revoke`", login, repo, held)
	}
	return nil
}

// reportRevoke summarizes a revoke run and returns an error if any student failed.
func reportRevoke(out io.Writer, dryRun bool, results []revokeResult) error {
	done, failed := 0, 0
	verb := "revoked"
	if dryRun {
		verb = "would revoke"
	}
	for _, r := range results {
		if r.err != nil {
			failed++
			continue
		}
		done++
		now := "removed"
		if r.retain == unit.RetainRead {
			now = "read"
		}
		fmt.Fprintf(out, "  %s %s %s (now %s)\n", verb, r.repo, r.login, now)
	}
	fmt.Fprintf(out, "%s access for %d dropped student(s), %d failed\n", verb, done, failed)
	if failed > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(out, "  FAILED %s %s: %v\n", r.repo, r.login, r.err)
			}
		}
		return fmt.Errorf("%d dropped student(s) failed to revoke", failed)
	}
	return nil
}
