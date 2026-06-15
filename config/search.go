package config

import (
	"os"
	"path/filepath"
)

// envVar names the environment variable that, when set, points directly at the
// config file and takes precedence over every other location.
const envVar = "GH_CLS_CONFIG"

// workingDirFile is the per-course working-directory config name. It is a
// visible (non-dotted) file on purpose: the config is hand-edited course
// structure, not a hidden machine setting, so it should be easy to see and open.
const workingDirFile = "gh-cls.yml"

// Search returns the config path to use, following the documented order:
//
//  1. $GH_CLS_CONFIG, if set (returned even if the file is missing, so an
//     explicit pointer surfaces a read error rather than being silently ignored).
//  2. ./gh-cls.yml, if it exists.
//  3. $XDG_CONFIG_HOME/gh-cls/config.yml (or ~/.config/gh-cls/config.yml), if it
//     exists.
//
// It returns "" when none apply; a missing config is not an error.
func Search() string {
	if p := os.Getenv(envVar); p != "" {
		return p
	}
	if fileExists(workingDirFile) {
		return workingDirFile
	}
	if p := xdgPath(); fileExists(p) {
		return p
	}
	return ""
}

// DefaultPath is where setup writes the org: the located config if one exists,
// otherwise a fresh working-directory file.
func DefaultPath() string {
	if p := Search(); p != "" {
		return p
	}
	return workingDirFile
}

// xdgPath is the XDG config location for the file.
func xdgPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gh-cls", "config.yml")
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
