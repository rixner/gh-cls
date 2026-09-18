package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// historyMode is how much history collect keeps in the clones under one output
// directory: the collected commit alone, or the repository's whole history and
// every branch.
type historyMode string

const (
	historySnapshot historyMode = "snapshot"
	historyFull     historyMode = "full"
)

// historyFileName is where a directory's setting is recorded, inside collect's
// own directory.
const historyFileName = "collect.yml"

// directorySettings is what that file holds.
type directorySettings struct {
	History historyMode `yaml:"history"`
}

// parseHistoryMode validates a --history value.
func parseHistoryMode(s string) (historyMode, error) {
	switch historyMode(s) {
	case historySnapshot:
		return historySnapshot, nil
	case historyFull:
		return historyFull, nil
	default:
		return "", fmt.Errorf("--history %q is not a setting: use %q or %q", s, historySnapshot, historyFull)
	}
}

// loadHistoryMode reads the setting recorded for an output directory. found is
// false when the directory has none, which is every directory an older version
// made.
func loadHistoryMode(out string) (historyMode, bool, error) {
	path := filepath.Join(out, gitCLSDir, historyFileName)
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the history setting from %s: %w", path, err)
	}
	var s directorySettings
	if err := yaml.Unmarshal(body, &s); err != nil {
		return "", false, fmt.Errorf("parsing %s: %w; fix or remove it, then re-run", path, err)
	}
	mode, err := parseHistoryMode(string(s.History))
	if err != nil {
		return "", false, fmt.Errorf("%s records an unknown history setting %q; fix or remove it, then re-run",
			path, s.History)
	}
	return mode, true, nil
}

// saveHistoryMode records the setting for an output directory, so a later run
// that forgets the flag, or a colleague's run that never had it, still matches
// the clones already there.
func saveHistoryMode(out string, m historyMode) error {
	dir := filepath.Join(out, gitCLSDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, historyFileName)
	body, err := yaml.Marshal(directorySettings{History: m})
	if err != nil {
		return fmt.Errorf("encoding the history setting: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("writing the history setting to %s: %w", path, err)
	}
	return nil
}

// resolveHistory decides the setting for this run and says where it came from,
// which the run header prints. A recorded setting wins over the default, and the
// flag wins over both.
//
// The setting belongs to the directory rather than the run because a later run
// can forget the flag, and a colleague running the default into your directory
// would otherwise give some students snapshot clones in a full-history
// collection with nothing saying so.
func resolveHistory(out, flag string) (mode historyMode, source string, changedFrom historyMode, err error) {
	recorded, found, err := loadHistoryMode(out)
	if err != nil {
		return "", "", "", err
	}
	if flag == "" {
		if found {
			return recorded, "recorded for this directory", "", nil
		}
		return historySnapshot, "the default", "", nil
	}
	m, err := parseHistoryMode(flag)
	if err != nil {
		return "", "", "", err
	}
	switch {
	case !found:
		return m, "set on this directory by this run", "", nil
	case m == recorded:
		return m, "recorded for this directory", "", nil
	default:
		return m, fmt.Sprintf("changed from %s by this run", recorded), recorded, nil
	}
}
