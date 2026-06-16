package roster

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// Recognized column headers (matched case-insensitively, in any order).
const (
	colIdentifier = "identifier"
	colUsername   = "username"
)

// ParseFile reads and parses the roster CSV at path.
func ParseFile(path string) (*Roster, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening roster %s: %w", path, err)
	}
	defer f.Close()
	r, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("roster %s: %w", path, err)
	}
	return r, nil
}

// Parse reads a roster CSV: a header row naming (in any order, any case) the
// required identifier and username columns, followed by one row per student.
// Values are trimmed; a duplicate identifier or an empty field is an error.
func Parse(in io.Reader) (*Roster, error) {
	cr := csv.NewReader(in)
	cr.TrimLeadingSpace = true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reading CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty roster (no header row)")
	}

	idCol, userCol := -1, -1
	for i, h := range rows[0] {
		switch strings.ToLower(strings.TrimSpace(stripBOM(h))) {
		case colIdentifier:
			idCol = i
		case colUsername:
			userCol = i
		}
	}
	if idCol == -1 || userCol == -1 {
		return nil, fmt.Errorf("roster must have %q and %q columns", colIdentifier, colUsername)
	}

	r := &Roster{byID: make(map[string]string)}
	// Track the first line each username appeared on (lower-cased, since GitHub
	// logins are case-insensitive) so a second occurrence is rejected rather than
	// silently collapsing two students onto one repo or audit identity.
	userLine := make(map[string]int)
	for n, row := range rows[1:] {
		line := n + 2 // 1-based, counting the header
		if idCol >= len(row) || userCol >= len(row) {
			return nil, fmt.Errorf("line %d: too few columns", line)
		}
		id := strings.TrimSpace(row[idCol])
		user := strings.TrimSpace(row[userCol])
		if id == "" || user == "" {
			return nil, fmt.Errorf("line %d: identifier and username must both be non-empty", line)
		}
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("line %d: duplicate identifier %q", line, id)
		}
		key := strings.ToLower(user)
		if first, dup := userLine[key]; dup {
			return nil, fmt.Errorf("line %d: username %q already used on line %d (GitHub usernames are case-insensitive); remove the duplicate", line, user, first)
		}
		userLine[key] = line
		r.byID[id] = user
		r.ids = append(r.ids, id)
	}
	if len(r.ids) == 0 {
		return nil, fmt.Errorf("roster has a header but no student rows")
	}
	return r, nil
}

// stripBOM removes a leading UTF-8 byte-order mark, which spreadsheet exports
// often prepend to the first header cell.
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
