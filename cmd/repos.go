package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rixner/gh-cls/config"
	"github.com/rixner/gh-cls/gh"
	"github.com/rixner/gh-cls/unit"
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

// inOrgTemplates maps the lowercased name of every template repository the config
// places in the org to the assignment that names it. GitHub repository names are
// case-insensitive, so the lowercased name is the identity. A template in another
// org is in no assignment's namespace and is left out. Assignments are visited in
// order, so a template two of them share always reports the same one.
func inOrgTemplates(cfg *config.Config) map[string]string {
	names := make([]string, 0, len(cfg.Assignments))
	for name := range cfg.Assignments {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]string, len(names))
	for _, name := range names {
		ref := cfg.Assignments[name].Template
		if ref == "" {
			continue
		}
		owner, repo, err := splitRepo(qualifyTemplate(ref, cfg.Org))
		if err != nil {
			// A malformed reference names no repo in this org, so it can match
			// none; the run that tries to use it reports the reference itself.
			continue
		}
		if !strings.EqualFold(owner, cfg.Org) {
			continue
		}
		if _, seen := out[strings.ToLower(repo)]; !seen {
			out[strings.ToLower(repo)] = name
		}
	}
	return out
}

// filterAssignmentRepos drops the repos in a <name>-* listing that are not this
// assignment's student work. Two kinds leak in, and both are defense in depth.
//
// A repo matching a longer configured assignment's <other>-* prefix. Every
// command lists an assignment's repos by name+"-", but a shorter assignment name
// can be a dash-prefix of a longer one (e.g. "proj" and "proj-final");
// config.Validate rejects that combination for assignments known at config load
// time, but a repo left behind by an assignment since renamed or removed from the
// config would otherwise still leak into the shorter assignment's results.
//
// A template repository the config names. A template sits in the namespace by
// design (hw1-template under hw1), and callers also skip whatever GitHub flags as
// a template repository, which covers a template no assignment names. That flag
// is remote and mutable though: cleared in the web UI, the template starts
// looking like student work, and freeze would downgrade its collaborators while
// collect cloned it as a submission. Excluding the templates the config names
// keeps the exclusion from depending on remote state.
func filterAssignmentRepos(cfg *config.Config, name string, repos []gh.Repo) []gh.Repo {
	var longerPrefixes []string
	for other := range cfg.Assignments {
		if other != name && len(other) > len(name) {
			longerPrefixes = append(longerPrefixes, other+"-")
		}
	}
	templates := inOrgTemplates(cfg)
	if len(longerPrefixes) == 0 && len(templates) == 0 {
		return repos
	}
	filtered := make([]gh.Repo, 0, len(repos))
	for _, r := range repos {
		if _, configured := templates[strings.ToLower(r.Name)]; configured {
			continue
		}
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

// checkTemplateCollision rejects a unit whose repository name is a template
// repository configured anywhere in the course. A template lives in the same
// <name>-* namespace as the repos it seeds (hw1-template under hw1), so a key can
// complete its name exactly: a group named "template" under hw1 resolves to
// hw1-template. assign skips a repository that already exists and still grants
// its members access, so that group would be handed push on the starter code
// instead of getting a repository of its own.
//
// A template's name is arbitrary, so it need not sit in its own assignment's
// namespace: assignments.hw1.template may name "hw2-starter", which lands in
// hw2's. The scan therefore covers every assignment's template, not just this
// one's. Only an in-org template can collide, since units are always created in
// the configured org.
func checkTemplateCollision(cfg *config.Config, name string, typ config.AssignmentType, units []unit.Unit) error {
	byName := inOrgTemplates(cfg)

	noun := "student"
	if typ == config.TypeGroup {
		noun = "group"
	}
	for _, u := range units {
		repo := name + "-" + u.Key
		other, ok := byName[strings.ToLower(repo)]
		if !ok {
			continue
		}
		whose := fmt.Sprintf("assignment %q's template repository", other)
		if other == name {
			whose = "this assignment's own template repository"
		}
		fix := fmt.Sprintf("point assignments.%s.template at a repository not named %s-<key>", other, name)
		if typ == config.TypeGroup {
			fix = "rename the group in the groups file, or " + fix
		}
		return fmt.Errorf("assignment %q: %s %q maps to %s/%s, which is %s; assign would grant access to the template instead of creating a repository. To fix: %s", name, noun, u.Key, cfg.Org, repo, whose, fix)
	}
	return nil
}
