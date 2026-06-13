package gh

import (
	"context"
	"strings"
	"testing"
)

func TestOrgRole(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{"role":"admin"}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	role, err := c.OrgRole(context.Background(), "cs101-spring26")
	if err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Errorf("role = %q", role)
	}
	if f.methods[0] != "GET" || f.paths[0] != "user/memberships/orgs/cs101-spring26" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
}

func TestGetOrg(t *testing.T) {
	t.Run("present fields decode as pointers", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(
			`{"default_repository_permission":"write","members_can_create_repositories":true}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		s, err := c.GetOrg(context.Background(), "org")
		if err != nil {
			t.Fatal(err)
		}
		if s.DefaultRepositoryPermission != "write" {
			t.Errorf("permission = %q", s.DefaultRepositoryPermission)
		}
		if s.MembersCanCreateRepositories == nil || !*s.MembersCanCreateRepositories {
			t.Error("members_can_create_repositories should decode to a non-nil true")
		}
		if f.paths[0] != "orgs/org" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("absent toggle stays nil", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"default_repository_permission":"none"}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		s, err := c.GetOrg(context.Background(), "org")
		if err != nil {
			t.Fatal(err)
		}
		if s.MembersCanCreateRepositories != nil {
			t.Error("an omitted toggle must stay nil, distinguishing it from false")
		}
	})
}

func TestPatchOrg(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.PatchOrg(context.Background(), "org", map[string]any{"default_repository_permission": "none"})
	if err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PATCH" || f.paths[0] != "orgs/org" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"default_repository_permission":"none"`) {
		t.Errorf("body %s should pass the fields through", f.bodies[0])
	}
}

func TestGetActionsPermissions(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{"enabled_repositories":"all"}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	p, err := c.GetActionsPermissions(context.Background(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if p.EnabledRepositories != "all" {
		t.Errorf("enabled_repositories = %q", p.EnabledRepositories)
	}
	if f.paths[0] != "orgs/org/actions/permissions" {
		t.Errorf("path = %q", f.paths[0])
	}
}

func TestSetActionsEnabledRepositories(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.SetActionsEnabledRepositories(context.Background(), "org", "none"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PUT" || f.paths[0] != "orgs/org/actions/permissions" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"enabled_repositories":"none"`) {
		t.Errorf("body %s missing enabled_repositories", f.bodies[0])
	}
}

func TestCopilotSeatCount(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"seat_breakdown":{"total":5}}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		count, present, err := c.CopilotSeatCount(context.Background(), "org")
		if err != nil || !present || count != 5 {
			t.Fatalf("got count=%d present=%v err=%v", count, present, err)
		}
		if f.paths[0] != "orgs/org/copilot/billing" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("absent on 404", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		count, present, err := c.CopilotSeatCount(context.Background(), "org")
		if err != nil || present || count != 0 {
			t.Fatalf("404 should mean no subscription, got count=%d present=%v err=%v", count, present, err)
		}
	})
}
