package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteSetup records the values setup owns in the config at path: the org
// (always) and, when non-empty, the staff team. It creates the file (and any
// parent directories) if it does not exist, and returns the previous org value
// (empty if the key was absent) so the caller can announce the change.
//
// The file is edited through a yaml.Node tree rather than a marshal of the
// Config struct, so existing comments, key order, and any keys this tool does
// not model are preserved.
func WriteSetup(path, org, staffTeam string) (previousOrg string, err error) {
	var doc yaml.Node
	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return "", fmt.Errorf("parsing config %s: %w", path, err)
		}
	case !os.IsNotExist(readErr):
		return "", fmt.Errorf("reading config %s: %w", path, readErr)
	}

	// org is pinned to the top of the file; staff_team follows it. The staff team
	// is recorded only when known, so later commands (assign) inherit it from
	// config and need not be told it again.
	previousOrg = setScalar(&doc, "org", org, true)
	if staffTeam != "" {
		setScalar(&doc, "staff_team", staffTeam, false)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("encoding config %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("creating config directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fmt.Errorf("writing config %s: %w", path, err)
	}
	return previousOrg, nil
}

// setScalar sets key to value in a parsed document, returning the prior value
// (empty if the key was absent). It handles an empty document (builds a fresh
// mapping) and an existing key (replaces its value in place, preserving
// surrounding comments). A missing key is prepended (to pin it to the top of the
// file) when prepend is true, otherwise appended.
func setScalar(doc *yaml.Node, key, value string, prepend bool) (previous string) {
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		mapping = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content[0] = mapping
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			val := mapping.Content[i+1]
			previous = val.Value
			val.Tag = "!!str"
			val.Value = value
			return previous
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if prepend {
		mapping.Content = append([]*yaml.Node{k, v}, mapping.Content...)
	} else {
		mapping.Content = append(mapping.Content, k, v)
	}
	return ""
}
