package gh

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// jsonRepos builds a JSON array of repos with the given names.
func jsonRepos(names ...string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf(`{"name":%q}`, n)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestListOrgReposByPrefix(t *testing.T) {
	// Page 1 is a full 100 entries (so paging continues), alternating a matching
	// "hw1-" name with a non-matching one; page 2 is short and holds one match.
	page1 := make([]string, 100)
	want := 0
	for i := range page1 {
		if i%2 == 0 {
			page1[i] = fmt.Sprintf("hw1-%d", i)
			want++
		} else {
			page1[i] = fmt.Sprintf("other-%d", i)
		}
	}
	want++ // the page-2 match below

	f := &fakeRequester{steps: []step{
		{resp: okResp(jsonRepos(page1...))},
		{resp: okResp(jsonRepos("hw1-last"))},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListOrgReposByPrefix(context.Background(), "org", "hw1-")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != want {
		t.Errorf("got %d matching repos, want %d", len(out), want)
	}
	for _, r := range out {
		if !strings.HasPrefix(r.Name, "hw1-") {
			t.Errorf("non-matching repo %q leaked through the filter", r.Name)
		}
	}
	if f.calls != 2 {
		t.Errorf("expected two pages fetched, got %d requests", f.calls)
	}
	if !strings.Contains(f.paths[0], "page=1") || !strings.Contains(f.paths[1], "page=2") {
		t.Errorf("pages not requested in order: %v", f.paths)
	}
	if !strings.Contains(f.paths[0], "per_page=100") {
		t.Errorf("expected per_page=100, got %q", f.paths[0])
	}
}

func TestListOrgReposByPrefixStopsOnShortFirstPage(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(jsonRepos("hw1-a", "zzz"))}}}
	var waits int
	c := newTestClient(f, &waits)
	out, err := c.ListOrgReposByPrefix(context.Background(), "org", "hw1-")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "hw1-a" {
		t.Errorf("got %v, want only hw1-a", out)
	}
	if f.calls != 1 {
		t.Errorf("a short first page must not trigger a second request, got %d", f.calls)
	}
}

func TestListDirectCollaborators(t *testing.T) {
	// One full page then a short page, to exercise the pagination loop.
	page1 := make([]string, 100)
	for i := range page1 {
		page1[i] = fmt.Sprintf(`{"login":"u%d"}`, i)
	}
	f := &fakeRequester{steps: []step{
		{resp: okResp("[" + strings.Join(page1, ",") + "]")},
		{resp: okResp(`[{"login":"last","permissions":{"push":true}}]`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListDirectCollaborators(context.Background(), "org", "hw1-ada")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 101 {
		t.Errorf("got %d collaborators, want 101", len(out))
	}
	if last := out[100]; last.Login != "last" || !last.Permissions.Push {
		t.Errorf("decoded last collaborator wrong: %+v", last)
	}
	if !strings.Contains(f.paths[0], "affiliation=direct") {
		t.Errorf("expected affiliation=direct filter, got %q", f.paths[0])
	}
	if f.calls != 2 {
		t.Errorf("expected two pages, got %d requests", f.calls)
	}
}

func TestListRepoInvitations(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[{"id":1,"invitee":{"login":"ada"},"expired":false},{"id":2,"invitee":{"login":"alan"},"expired":true}]`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoInvitations(context.Background(), "org", "hw1-ada")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d invitations, want 2", len(out))
	}
	if out[0].ID != 1 || out[0].Invitee.Login != "ada" || out[0].Expired {
		t.Errorf("decoded[0] = %+v", out[0])
	}
	if out[1].ID != 2 || !out[1].Expired {
		t.Errorf("decoded[1] should be the expired invitation: %+v", out[1])
	}
	if !strings.Contains(f.paths[0], "invitations") {
		t.Errorf("path = %q", f.paths[0])
	}
}

func TestDeleteRepoInvitation(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	if err := c.DeleteRepoInvitation(context.Background(), "org", "hw1-ada", 555); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "DELETE" || f.paths[0] != "repos/org/hw1-ada/invitations/555" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
}

func TestUpdateRepoInvitation(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	stillPending, err := c.UpdateRepoInvitation(context.Background(), "org", "hw1-ada", 555, InvitationRead)
	if err != nil {
		t.Fatal(err)
	}
	if !stillPending {
		t.Error("a successful update means the invitation was still pending")
	}
	if f.methods[0] != "PATCH" || f.paths[0] != "repos/org/hw1-ada/invitations/555" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	// The API's key is "permissions" (plural); "permission" -- the collaborator
	// API's spelling -- is silently ignored, which would leave the invitation at
	// write while the call still returns 200.
	if !strings.Contains(f.bodies[0], `"permissions":"read"`) {
		t.Errorf("body = %q, want the permissions field set to read", f.bodies[0])
	}
}

func TestUpdateRepoInvitationAlreadyAccepted(t *testing.T) {
	// Accepting consumes the invitation, so a PATCH racing an acceptance 404s.
	// That is not a failure: the invitee is a collaborator now, and freeze's
	// collaborator pass governs them. Reporting it as an error would abort a
	// freeze over a normal interleaving.
	f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
	var waits int
	c := newTestClient(f, &waits)

	stillPending, err := c.UpdateRepoInvitation(context.Background(), "org", "hw1-ada", 555, InvitationRead)
	if err != nil {
		t.Fatalf("a consumed invitation is not an error: %v", err)
	}
	if stillPending {
		t.Error("a 404 means the invitation is no longer pending")
	}
}

func TestInvitationPermissionPredicates(t *testing.T) {
	// The invitation vocabulary must classify the same way the collaborator one
	// does, or freeze would treat the two paths to a repo inconsistently.
	tests := []struct {
		permissions   string
		wantConfers   bool
		wantAboveRead bool
	}{
		{InvitationRead, false, false},
		{InvitationTriage, false, true},
		{InvitationWrite, true, true},
		{InvitationMaintain, true, true},
		{InvitationAdmin, true, false}, // staff keep access through a freeze
		{"", false, false},             // absent field must not read as write
	}
	for _, tc := range tests {
		t.Run(tc.permissions, func(t *testing.T) {
			i := Invitation{Permissions: tc.permissions}
			if got := i.ConfersPush(); got != tc.wantConfers {
				t.Errorf("ConfersPush() = %v, want %v", got, tc.wantConfers)
			}
			if got := i.AboveRead(); got != tc.wantAboveRead {
				t.Errorf("AboveRead() = %v, want %v", got, tc.wantAboveRead)
			}
		})
	}
}

func TestCollaboratorPermissionPredicates(t *testing.T) {
	tests := []struct {
		name                                string
		admin, maintain, push, triage, pull bool
		wantCanPush                         bool
		wantAboveRead                       bool
	}{
		{name: "admin only", admin: true, wantCanPush: true, wantAboveRead: false},
		{name: "maintain", maintain: true, wantCanPush: true, wantAboveRead: true},
		{name: "push", push: true, wantCanPush: true, wantAboveRead: true},
		{name: "triage", triage: true, wantCanPush: false, wantAboveRead: true},
		{name: "pull", pull: true, wantCanPush: false, wantAboveRead: false},
		{name: "none", wantCanPush: false, wantAboveRead: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Collaborator
			c.Permissions.Admin = tt.admin
			c.Permissions.Maintain = tt.maintain
			c.Permissions.Push = tt.push
			c.Permissions.Triage = tt.triage
			c.Permissions.Pull = tt.pull
			if got := c.CanPush(); got != tt.wantCanPush {
				t.Errorf("CanPush() = %v, want %v", got, tt.wantCanPush)
			}
			if got := c.AboveRead(); got != tt.wantAboveRead {
				t.Errorf("AboveRead() = %v, want %v", got, tt.wantAboveRead)
			}
		})
	}
}
