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
