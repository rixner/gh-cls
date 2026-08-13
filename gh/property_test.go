package gh

import (
	"context"
	"strings"
	"testing"
)

func TestGetPropertyDefinition(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`{"property_name":"gh-cls-frozen","value_type":"true_false","values_editable_by":"org_actors"}`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	def, ok, err := c.GetPropertyDefinition(context.Background(), "org", "gh-cls-frozen")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the property should be reported as existing")
	}
	if def.ValueType != PropertyTypeTrueFalse || def.ValuesEditableBy != PropertyEditableByOrg {
		t.Errorf("decoded = %+v", def)
	}
	if f.paths[0] != "orgs/org/properties/schema/gh-cls-frozen" {
		t.Errorf("path = %q", f.paths[0])
	}
}

func TestGetPropertyDefinitionMissing(t *testing.T) {
	// An org that has never declared the property must be reported as absent, not
	// as an error: setup branches on this to decide whether to create it.
	f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
	var waits int
	c := newTestClient(f, &waits)

	def, ok, err := c.GetPropertyDefinition(context.Background(), "org", "gh-cls-frozen")
	if err != nil {
		t.Fatalf("a missing property is not an error: %v", err)
	}
	if ok || def != nil {
		t.Errorf("got %+v, %v; want absent", def, ok)
	}
}

func TestSetPropertyDefinition(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	err := c.SetPropertyDefinition(context.Background(), "org", PropertyDefinition{
		PropertyName:     "gh-cls-frozen",
		ValueType:        PropertyTypeTrueFalse,
		Description:      "d",
		ValuesEditableBy: PropertyEditableByOrg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PUT" || f.paths[0] != "orgs/org/properties/schema/gh-cls-frozen" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	// The edit scope must be sent explicitly rather than relying on the API
	// default, since it is what stops a repository admin rewriting a record.
	if !strings.Contains(f.bodies[0], `"values_editable_by":"org_actors"`) {
		t.Errorf("body = %q, want the restricted edit scope", f.bodies[0])
	}
}

func TestListRepoPropertyValues(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[{"repository_name":"hw1-ada","properties":[{"property_name":"gh-cls-frozen","value":"true"}]},
		               {"repository_name":"hw1-bob","properties":[{"property_name":"gh-cls-frozen","value":"false"}]}]`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.ListRepoPropertyValues(context.Background(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if out["hw1-ada"]["gh-cls-frozen"] != "true" || out["hw1-bob"]["gh-cls-frozen"] != "false" {
		t.Errorf("decoded = %v", out)
	}
	// One org-wide listing, not a call per repository: this is what keeps reading
	// the freeze state off the per-repo cost curve.
	if !strings.HasPrefix(f.paths[0], "orgs/org/properties/values") {
		t.Errorf("path = %q", f.paths[0])
	}
}

func TestSetRepoPropertyValue(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	if err := c.SetRepoPropertyValue(context.Background(), "org", "hw1-ada", "gh-cls-frozen", "true"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PATCH" || f.paths[0] != "repos/org/hw1-ada/properties/values" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	// The body names only this property. That is the whole reason this mechanism
	// was chosen over repository topics, whose API replaces the entire set and so
	// needs a read-modify-write that can lose a concurrent writer's change.
	if !strings.Contains(f.bodies[0], `"property_name":"gh-cls-frozen"`) || !strings.Contains(f.bodies[0], `"value":"true"`) {
		t.Errorf("body = %q", f.bodies[0])
	}
}

func TestGetRepoPropertyValues(t *testing.T) {
	f := &fakeRequester{steps: []step{
		{resp: okResp(`[{"property_name":"gh-cls-frozen","value":"true"}]`)},
	}}
	var waits int
	c := newTestClient(f, &waits)

	out, err := c.GetRepoPropertyValues(context.Background(), "org", "hw1-ada")
	if err != nil {
		t.Fatal(err)
	}
	if out["gh-cls-frozen"] != "true" {
		t.Errorf("decoded = %v", out)
	}
}
