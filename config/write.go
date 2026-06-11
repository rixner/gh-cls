package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteOrg sets the org key in the config at path, creating the file (and any
// parent directories) if it does not exist. It returns the previous org value
// (empty if the key was absent) so the caller can announce the change.
//
// The file is edited through a yaml.Node tree rather than a marshal of the
// Config struct, so existing comments, key order, and any keys this tool does
// not model are preserved.
func WriteOrg(path, org string) (previous string, err error) {
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

	previous = setOrg(&doc, org)

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
	return previous, nil
}

// setOrg sets the org scalar in a parsed document, returning the prior value.
// It handles an empty document (builds a fresh mapping), an existing org key
// (replaces its value in place, preserving surrounding comments), and a missing
// org key (prepends it so it stays at the top of the file).
func setOrg(doc *yaml.Node, org string) (previous string) {
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
		if mapping.Content[i].Value == "org" {
			val := mapping.Content[i+1]
			previous = val.Value
			val.Tag = "!!str"
			val.Value = org
			return previous
		}
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "org"}
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: org}
	mapping.Content = append([]*yaml.Node{key, val}, mapping.Content...)
	return ""
}
