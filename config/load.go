package config

import (
	"fmt"
	"os"

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
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}
