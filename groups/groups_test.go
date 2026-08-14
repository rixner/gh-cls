package groups

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFlowAndBlockEquivalent(t *testing.T) {
	flow := "group-alpha: [student-001, student-003]\ngroup-beta: [student-002]\n"
	block := "group-alpha:\n  - student-001\n  - student-003\ngroup-beta:\n  - student-002\n"

	for name, in := range map[string]string{"flow": flow, "block": block} {
		g, err := Parse(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := g.Names(); !reflect.DeepEqual(got, []string{"group-alpha", "group-beta"}) {
			t.Errorf("%s: Names() = %v, want file order", name, got)
		}
		if got := g.Members("group-alpha"); !reflect.DeepEqual(got, []string{"student-001", "student-003"}) {
			t.Errorf("%s: Members(group-alpha) = %v", name, got)
		}
		if g.Len() != 2 {
			t.Errorf("%s: Len() = %d, want 2", name, g.Len())
		}
	}
}

func TestMembersUnknownGroup(t *testing.T) {
	g, err := Parse(strings.NewReader("a: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Members("nope"); got != nil {
		t.Errorf("Members(nope) = %v, want nil", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"empty input":            "",
		"sequence at top level":  "- a\n- b\n",
		"group value not a list": "group-alpha: student-001\n",
		"empty member":           "group-alpha: [\"\"]\n",
		"duplicate id in group":  "group-alpha: [x, x]\n",
		"empty group list":       "group-alpha: []\n",
	}
	for name, in := range cases {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseRejectsUnsafeGroupNames(t *testing.T) {
	// A group name is half of its repository's name. GitHub rewrites a name it
	// cannot use ("Team Alpha" becomes "proj-Team-Alpha"), so assign waits for a
	// repository that never appears and the rewritten one is left behind with no
	// grants, invisible to freeze, feedback, and status.
	cases := map[string]string{
		"space":          "Team Alpha",
		"slash":          "team/alpha",
		"unicode letter": "équipe",
		"colon":          "team:alpha",
	}
	for what, name := range cases {
		in := "good-group: [student-001]\n" + name + ": [student-002]\n"
		_, err := Parse(strings.NewReader(in))
		if err == nil {
			t.Errorf("%s: group %q should be rejected", what, name)
			continue
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: the error should name the group and its line, got %v", what, err)
		}
	}

	// The characters GitHub keeps verbatim are accepted.
	if _, err := Parse(strings.NewReader("Team.Alpha_1-2: [student-001]\n")); err != nil {
		t.Errorf("a repo-name-safe group should be accepted, got %v", err)
	}
}
