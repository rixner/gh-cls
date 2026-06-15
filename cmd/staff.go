package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/roster"
	"github.com/spf13/cobra"
)

// staffClient is the narrow set of GitHub operations staff needs.
type staffClient interface {
	OrgRole(ctx context.Context, org string) (string, error)
	GetTeam(ctx context.Context, org, slug string) (*gh.Team, bool, error)
	ListTeamMembers(ctx context.Context, org, slug string) ([]string, error)
	AddTeamMembership(ctx context.Context, org, slug, username string) (string, error)
	RemoveTeamMembership(ctx context.Context, org, slug, username string) error
}

// staffOpts carries the resolved flags and dependencies for `gh cls staff`.
type staffOpts struct {
	g         *globalOpts
	tas       string
	prune     bool
	dryRun    bool
	newClient func(context.Context) (staffClient, error)
}

func newStaffCmd(g *globalOpts) *cobra.Command {
	o := &staffOpts{
		g:         g,
		newClient: func(context.Context) (staffClient, error) { return gh.New() },
	}
	cmd := &cobra.Command{
		Use:   "staff",
		Short: "Add the staff team's members from a TA list",
		Long: `Add the GitHub usernames in a TA file (an identifier,username CSV — the same
format as the roster) to the staff team. By default it only adds: members not in
the file are left alone and reported, so an incomplete file can never silently
remove a TA. Pass --prune to also remove members not in the file; the removals are
named so a mistake is easy to undo.

The staff team is the config's staff_team and must already exist (run setup). A
TA who is not yet an organization member is invited and joins once they accept.`,
		Example: `  gh cls staff --tas tas.csv
  gh cls staff --tas tas.csv --prune`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.tas, "tas", "t", "", "path to the TA CSV (identifier,username; required)")
	f.BoolVar(&o.prune, "prune", false, "also remove team members not listed in the TA file")
	f.BoolVarP(&o.dryRun, "dry-run", "n", false, "show what would change without doing it")
	_ = cmd.MarkFlagRequired("tas")
	return cmd
}

func (o *staffOpts) run(ctx context.Context, out io.Writer) error {
	org := o.g.org
	staffTeam := o.g.staffTeam
	if staffTeam == "" {
		return fmt.Errorf("no staff_team in the config; set it before syncing staff")
	}

	// Parse the TA list before any API call, so a malformed file aborts before
	// the team is touched. A TA file is the same CSV format as the roster, so the
	// roster parser is reused (case- and order-insensitive, BOM- and dup-checked).
	r, err := roster.ParseFile(o.tas)
	if err != nil {
		return err
	}
	// desired maps a lower-cased login to the original spelling: GitHub logins are
	// case-insensitive, so comparison is lower-cased, but the original is kept for
	// display and the add call.
	desired := make(map[string]string, r.Len())
	for _, id := range r.IDs() {
		u, _ := r.Lookup(id)
		desired[strings.ToLower(u)] = u
	}

	client, err := o.newClient(ctx)
	if err != nil {
		return err
	}
	if err := requireOwner(ctx, client, org); err != nil {
		return err
	}
	if _, exists, err := client.GetTeam(ctx, org, staffTeam); err != nil {
		return fmt.Errorf("checking staff team %q: %w", staffTeam, err)
	} else if !exists {
		return fmt.Errorf("staff team %q not found in %s; run `gh cls setup` to create it", staffTeam, org)
	}

	members, err := client.ListTeamMembers(ctx, org, staffTeam)
	if err != nil {
		return fmt.Errorf("listing %q members: %w", staffTeam, err)
	}
	current := make(map[string]string, len(members))
	for _, m := range members {
		current[strings.ToLower(m)] = m
	}

	// toAdd: listed but not yet members. extra: members not listed — removed only
	// with --prune, otherwise just reported.
	var toAdd, extra []string
	for low, login := range desired {
		if _, ok := current[low]; !ok {
			toAdd = append(toAdd, login)
		}
	}
	for low, login := range current {
		if _, ok := desired[low]; !ok {
			extra = append(extra, login)
		}
	}
	sort.Strings(toAdd)
	sort.Strings(extra)

	if o.dryRun {
		fmt.Fprintf(out, "DRY RUN — no changes will be made\n\n")
	}
	fmt.Fprintf(out, "Staff team %q in %s ← %s (%d listed):\n", staffTeam, org, o.tas, len(desired))
	if len(toAdd) == 0 && len(extra) == 0 {
		fmt.Fprintln(out, "  already in sync — nothing to change")
		return nil
	}

	for _, u := range toAdd {
		if o.dryRun {
			fmt.Fprintf(out, "  + %s  (would add)\n", u)
			continue
		}
		state, err := client.AddTeamMembership(ctx, org, staffTeam, u)
		if err != nil {
			return fmt.Errorf("adding %s to %q: %w", u, staffTeam, err)
		}
		if state == "pending" {
			fmt.Fprintf(out, "  + %s  (invited — joins once they accept org membership)\n", u)
		} else {
			fmt.Fprintf(out, "  + %s\n", u)
		}
	}

	if o.prune {
		for _, u := range extra {
			if o.dryRun {
				fmt.Fprintf(out, "  - %s  (would remove)\n", u)
				continue
			}
			if err := client.RemoveTeamMembership(ctx, org, staffTeam, u); err != nil {
				return fmt.Errorf("removing %s from %q: %w", u, staffTeam, err)
			}
			fmt.Fprintf(out, "  - %s  (removed)\n", u)
		}
	}

	verb := "added"
	if o.dryRun {
		verb = "to add"
	}
	if o.prune {
		removed := "removed"
		if o.dryRun {
			removed = "to remove"
		}
		fmt.Fprintf(out, "\n%d %s, %d %s\n", len(toAdd), verb, len(extra), removed)
	} else {
		fmt.Fprintf(out, "\n%d %s\n", len(toAdd), verb)
		if len(extra) > 0 {
			// An incomplete file must never silently delete a TA, so unlisted
			// members are warned about, not removed, with the names so the user can
			// decide whether to prune.
			fmt.Fprintf(out, "\nwarning: %d member(s) of %q are not in %s — re-run with --prune to remove them:\n  %s\n",
				len(extra), staffTeam, o.tas, strings.Join(extra, ", "))
		}
	}
	return nil
}
