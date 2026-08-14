package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Custom property value types and edit scopes, as GitHub names them.
const (
	PropertyTypeTrueFalse = "true_false"
	// PropertyEditableByOrg restricts setting a property's value to organization
	// actors. It deliberately excludes repository actors: a repository admin must
	// not be able to rewrite a value the tool relies on, and an outside
	// collaborator (how students are added) never can.
	PropertyEditableByOrg = "org_actors"
)

// PropertyDefinition is an organization-level custom property's schema: the
// declaration that must exist before any repository can carry a value for it.
type PropertyDefinition struct {
	PropertyName     string `json:"property_name"`
	ValueType        string `json:"value_type"`
	Description      string `json:"description"`
	ValuesEditableBy string `json:"values_editable_by"`
}

// propertyValue is a custom property's value. GitHub types it null | string |
// string[]: a multi_select property returns an array, and null means the
// repository has never been given a value. Only a string is kept; anything else
// decodes to the empty string. Any organization actor can define a multi_select
// property, so decoding value as a plain string would let one such property on
// one repository fail the whole org-wide listing every command reads. The tool
// never needs a multi_select value, it only has to survive its presence.
type propertyValue string

func (v *propertyValue) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		*v = ""
		return nil
	}
	*v = propertyValue(s)
	return nil
}

// repoPropertyValues is one repository's property values as the org-wide values
// listing returns them.
type repoPropertyValues struct {
	RepositoryName string `json:"repository_name"`
	Properties     []struct {
		PropertyName string        `json:"property_name"`
		Value        propertyValue `json:"value"`
	} `json:"properties"`
}

// GetPropertyDefinition fetches an organization custom property's schema,
// reporting existence via the bool so callers can branch without inspecting
// error strings.
func (c *restClient) GetPropertyDefinition(ctx context.Context, org, name string) (*PropertyDefinition, bool, error) {
	var def PropertyDefinition
	path := fmt.Sprintf("orgs/%s/properties/schema/%s", url.PathEscape(org), url.PathEscape(name))
	if _, err := c.do(ctx, "GET", path, nil, &def); err != nil {
		if notFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &def, true, nil
}

// SetPropertyDefinition creates or updates an organization custom property's
// schema. It is idempotent: re-declaring an identical property is a no-op on
// GitHub's side.
func (c *restClient) SetPropertyDefinition(ctx context.Context, org string, def PropertyDefinition) error {
	path := fmt.Sprintf("orgs/%s/properties/schema/%s", url.PathEscape(org), url.PathEscape(def.PropertyName))
	body := map[string]any{
		"value_type":         def.ValueType,
		"description":        def.Description,
		"values_editable_by": def.ValuesEditableBy,
	}
	_, err := c.do(ctx, "PUT", path, body, nil)
	return err
}

// ListRepoPropertyValues returns every repository in the org that carries custom
// property values, as repo name -> property name -> value. This is a single
// paginated org-wide listing rather than a call per repository, so reading a
// property across a whole assignment costs the same as reading it for one repo.
func (c *restClient) ListRepoPropertyValues(ctx context.Context, org string) (map[string]map[string]string, error) {
	rows, err := getPaged[repoPropertyValues](ctx, c, func(page int) string {
		return fmt.Sprintf("orgs/%s/properties/values?per_page=%d&page=%d", url.PathEscape(org), pageSize, page)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(rows))
	for _, r := range rows {
		vals := make(map[string]string, len(r.Properties))
		for _, p := range r.Properties {
			vals[p.PropertyName] = string(p.Value)
		}
		out[r.RepositoryName] = vals
	}
	return out, nil
}

// SetRepoPropertyValue sets one custom property on a repository. The update
// names only this property, so it cannot disturb any other value the repository
// carries: unlike topics, there is no read-modify-write and so no window in
// which a concurrent writer's change is lost.
func (c *restClient) SetRepoPropertyValue(ctx context.Context, org, repo, name, value string) error {
	path := fmt.Sprintf("repos/%s/%s/properties/values", url.PathEscape(org), url.PathEscape(repo))
	body := map[string]any{
		"properties": []map[string]any{{"property_name": name, "value": value}},
	}
	_, err := c.do(ctx, "PATCH", path, body, nil)
	return err
}

// GetRepoPropertyValues returns one repository's custom property values, for
// confirming a write took effect without re-listing the whole organization.
func (c *restClient) GetRepoPropertyValues(ctx context.Context, org, repo string) (map[string]string, error) {
	var props []struct {
		PropertyName string        `json:"property_name"`
		Value        propertyValue `json:"value"`
	}
	path := fmt.Sprintf("repos/%s/%s/properties/values", url.PathEscape(org), url.PathEscape(repo))
	if _, err := c.do(ctx, "GET", path, nil, &props); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(props))
	for _, p := range props {
		out[p.PropertyName] = string(p.Value)
	}
	return out, nil
}
