package cmd

import (
	"fmt"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
)

// qualifyTemplate gives a bare template name (no owner) the configured org, so
// "hw1-template" means "<org>/hw1-template", the common in-org case. A reference
// that already names an owner ("owner/name") is returned unchanged, so a template
// may live in another org.
func qualifyTemplate(ref, org string) string {
	if strings.Contains(ref, "/") {
		return ref
	}
	return org + "/" + ref
}

// splitRepo parses an "owner/name" reference.
func splitRepo(ref string) (owner, name string, err error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repository %q: want owner/name", ref)
	}
	return parts[0], parts[1], nil
}

// filterAssignmentRepos drops any repo whose name also matches a longer
// configured assignment's <other>-* prefix. Every command lists an
// assignment's repos by name+"-", but a shorter assignment name can be a
// dash-prefix of a longer one (e.g. "proj" and "proj-final"); config.Validate
// rejects that combination for assignments known at config load time, but a
// repo left behind by an assignment since renamed or removed from the config
// would otherwise still leak into the shorter assignment's results. This is
// defense in depth for that case.
func filterAssignmentRepos(cfg *config.Config, name string, repos []gh.Repo) []gh.Repo {
	var longerPrefixes []string
	for other := range cfg.Assignments {
		if other != name && len(other) > len(name) {
			longerPrefixes = append(longerPrefixes, other+"-")
		}
	}
	if len(longerPrefixes) == 0 {
		return repos
	}
	filtered := make([]gh.Repo, 0, len(repos))
	for _, r := range repos {
		leaked := false
		for _, p := range longerPrefixes {
			if strings.HasPrefix(r.Name, p) {
				leaked = true
				break
			}
		}
		if !leaked {
			filtered = append(filtered, r)
		}
	}
	return filtered
}
