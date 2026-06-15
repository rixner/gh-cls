package gh

import (
	"context"
	"strings"
	"testing"
)

func TestApplyRulesetSkipsWhenActivePresent(t *testing.T) {
	// The managed ruleset already exists and is active: only the GET happens.
	f := &fakeRequester{steps: []step{{resp: okResp(`[{"name":"gh-cls-protect","enforcement":"active"}]`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.ApplyRuleset(context.Background(), "org", "hw1-ada"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("an existing active ruleset should not be recreated, got %d requests", f.calls)
	}
	if f.methods[0] != "GET" {
		t.Errorf("first request should be the existence GET, got %s", f.methods[0])
	}
}

func TestApplyRulesetRejectsExistingInactive(t *testing.T) {
	// A ruleset that exists but is disabled leaves student work unprotected: it
	// must be a loud failure, not a silent idempotent skip.
	f := &fakeRequester{steps: []step{{resp: okResp(`[{"name":"gh-cls-protect","enforcement":"disabled"}]`)}}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.ApplyRuleset(context.Background(), "org", "hw1-ada")
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("an inactive existing ruleset should error, got %v", err)
	}
}

func TestApplyRulesetVerifiesAfterCreate(t *testing.T) {
	// Create returns success but the re-read shows the ruleset is still absent:
	// the protection did not take, so the call must fail loudly.
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[]`)}, // none existing
		{resp: okResp(`{}`)}, // create
		{resp: okResp(`[]`)}, // verify: still absent
	}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.ApplyRuleset(context.Background(), "org", "hw1-ada")
	if err == nil || !strings.Contains(err.Error(), "did not take effect") {
		t.Fatalf("a ruleset absent after creation should fail, got %v", err)
	}
}

func TestApplyRulesetExcludesStaffFromBypass(t *testing.T) {
	// Divergence guard: the bypass list grants org admins only. Staff push to
	// student repos but must never be able to force-push or delete protected
	// branches, so no Team actor may appear in the ruleset.
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[]`)}, // no existing rulesets
		{resp: okResp(`{}`)}, // create
		{resp: okResp(`[{"name":"gh-cls-protect","enforcement":"active"}]`)}, // verify
	}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.ApplyRuleset(context.Background(), "org", "hw1-ada"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 || f.methods[1] != "POST" {
		t.Fatalf("expected a GET, POST, then verifying GET, got %v", f.methods)
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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("create body missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"actor_type":"Team"`) {
		t.Errorf("staff team must not be granted a bypass:\n%s", body)
	}
}
