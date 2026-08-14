package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// envVar names the environment variable that points at the config file when the
// -c/--config flag is not given.
const envVar = "GH_CLS_CONFIG"

// ResolvePath returns the config file path from an explicit value (the -c/--config
// flag) or, failing that, $GH_CLS_CONFIG. The config is user-authored and the
// tool never guesses its location, so it is an error for neither to be set.
func ResolvePath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if p := os.Getenv(envVar); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no config file: pass -c <file> or set %s to your course config", envVar)
}

// Load reads, parses, and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var c Config
	// KnownFields rejects a key the schema does not define. Without it a
	// misspelling (branch_protecton, feedbck) parses cleanly and is dropped, so
	// assign silently creates a whole class of repositories without the protection
	// or the feedback artifact the config asked for. An empty file decodes as EOF
	// and is left to Validate, whose message says which keys are required.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config %s: %w", path, unknownKeyHint(err))
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// unknownKeyHint restates the decoder's unknown-key errors, which name Go types,
// in the config file's own vocabulary and points at where the keys are
// documented. Any other error is returned unchanged.
func unknownKeyHint(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, m := range te.Errors {
		m = strings.Replace(m, "not found in type config.Config", "is not a course setting", 1)
		m = strings.Replace(m, "not found in type config.Assignment", "is not an assignment setting", 1)
		msgs = append(msgs, strings.Replace(m, "field ", "key ", 1))
	}
	return fmt.Errorf("%s; check the spelling against the config keys in README.md", strings.Join(msgs, "; "))
}
