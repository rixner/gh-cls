package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads the config from the first location in the search order. A missing
// config is not an error: it returns an empty Config and an empty path so
// commands can still run (setup writes one; others report a missing org).
func Load() (*Config, string, error) {
	path := Search()
	if path == "" {
		return &Config{}, "", nil
	}
	return LoadFile(path)
}

// LoadFile parses the config at an explicit path. It returns the path alongside
// the parsed config so callers can report where settings came from.
func LoadFile(path string) (*Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("reading config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, path, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, path, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, path, nil
}
