package groups

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseFile reads and parses the groups YAML at path.
func ParseFile(path string) (*Groups, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening groups file %s: %w", path, err)
	}
	defer f.Close()
	g, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("groups file %s: %w", path, err)
	}
	return g, nil
}

// Parse reads a groups file: a mapping of group name to a list of student
// identifiers, in either flow ([a, b]) or block (- a) style. Each group must
// have at least one member, and identifiers must be unique within a group.
func Parse(in io.Reader) (*Groups, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(in).Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty groups file")
		}
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty groups file")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("groups file must be a mapping of group name to a list of identifiers")
	}

	g := &Groups{members: make(map[string][]string)}
	for i := 0; i+1 < len(root.Content); i += 2 {
		name := strings.TrimSpace(root.Content[i].Value)
		list := root.Content[i+1]
		if name == "" {
			return nil, fmt.Errorf("empty group name")
		}
		if _, dup := g.members[name]; dup {
			return nil, fmt.Errorf("duplicate group %q", name)
		}
		if list.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("group %q: value must be a list of identifiers", name)
		}

		ids := make([]string, 0, len(list.Content))
		seen := make(map[string]bool, len(list.Content))
		for _, item := range list.Content {
			id := strings.TrimSpace(item.Value)
			if id == "" {
				return nil, fmt.Errorf("group %q: empty identifier", name)
			}
			if seen[id] {
				return nil, fmt.Errorf("group %q: duplicate identifier %q", name, id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("group %q: has no members", name)
		}
		g.names = append(g.names, name)
		g.members[name] = ids
	}
	if len(g.names) == 0 {
		return nil, fmt.Errorf("groups file has no groups")
	}
	return g, nil
}
