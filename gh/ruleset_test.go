package gh

import (
	"context"
	"strings"
	"testing"
)

func TestApplyRulesetSkipsWhenPresent(t *testing.T) {
	// The managed ruleset already exists: only the GET happens, no POST.
	f := &fakeRequester{steps: []step{{resp: okResp(`[{"name":"gh-cls-protect"}]`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.ApplyRuleset(context.Background(), "org", "hw1-ada", 42); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("an existing ruleset should not be recreated, got %d requests", f.calls)
	}
	if f.methods[0] != "GET" {
		t.Errorf("first request should be the existence GET, got %s", f.methods[0])
	}
}

func TestApplyRulesetCreatesWithStaffBypass(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[]`)}, // no existing rulesets
		{resp: okResp(`{}`)}, // create
	}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.ApplyRuleset(context.Background(), "org", "hw1-ada", 42); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || f.methods[1] != "POST" {
		t.Fatalf("expected a GET then POST, got %v", f.methods)
	}
	if f.paths[1] != "repos/org/hw1-ada/rulesets" {
		t.Errorf("create path = %q", f.paths[1])
	}
	body := f.bodies[1]
	for _, want := range []string{
		`"name":"gh-cls-protect"`,
		`"non_fast_forward"`,
		`"deletion"`,
		`"OrganizationAdmin"`,
		`"actor_id":42`,
		`"actor_type":"Team"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("create body missing %s:\n%s", want, body)
		}
	}
}

func TestApplyRulesetWithoutStaffTeamOmitsTeamActor(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[]`)},
		{resp: okResp(`{}`)},
	}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.ApplyRuleset(context.Background(), "org", "hw1-ada", 0); err != nil {
		t.Fatal(err)
	}
	body := f.bodies[1]
	if !strings.Contains(body, `"OrganizationAdmin"`) {
		t.Errorf("org admin bypass should always be present:\n%s", body)
	}
	if strings.Contains(body, `"actor_type":"Team"`) {
		t.Errorf("no team actor expected when staffTeamID is zero:\n%s", body)
	}
}
