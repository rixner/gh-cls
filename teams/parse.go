package teams

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseFile reads and parses the teams YAML at path.
func ParseFile(path string) (*Teams, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening teams file %s: %w", path, err)
	}
	defer f.Close()
	t, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("teams file %s: %w", path, err)
	}
	return t, nil
}

// Parse reads a teams file: a mapping of team name to a list of student
// identifiers, in either flow ([a, b]) or block (- a) style. Each team must
// have at least one member, and identifiers must be unique within a team.
func Parse(in io.Reader) (*Teams, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(in).Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty teams file")
		}
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty teams file")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("teams file must be a mapping of team name to a list of identifiers")
	}

	t := &Teams{members: make(map[string][]string)}
	for i := 0; i+1 < len(root.Content); i += 2 {
		name := strings.TrimSpace(root.Content[i].Value)
		list := root.Content[i+1]
		if name == "" {
			return nil, fmt.Errorf("empty team name")
		}
		if _, dup := t.members[name]; dup {
			return nil, fmt.Errorf("duplicate team %q", name)
		}
		if list.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("team %q: value must be a list of identifiers", name)
		}

		ids := make([]string, 0, len(list.Content))
		seen := make(map[string]bool, len(list.Content))
		for _, item := range list.Content {
			id := strings.TrimSpace(item.Value)
			if id == "" {
				return nil, fmt.Errorf("team %q: empty identifier", name)
			}
			if seen[id] {
				return nil, fmt.Errorf("team %q: duplicate identifier %q", name, id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("team %q: has no members", name)
		}
		t.names = append(t.names, name)
		t.members[name] = ids
	}
	if len(t.names) == 0 {
		return nil, fmt.Errorf("teams file has no teams")
	}
	return t, nil
}
