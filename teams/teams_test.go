package teams

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFlowAndBlockEquivalent(t *testing.T) {
	flow := "team-alpha: [student-001, student-003]\nteam-beta: [student-002]\n"
	block := "team-alpha:\n  - student-001\n  - student-003\nteam-beta:\n  - student-002\n"

	for name, in := range map[string]string{"flow": flow, "block": block} {
		tm, err := Parse(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := tm.Names(); !reflect.DeepEqual(got, []string{"team-alpha", "team-beta"}) {
			t.Errorf("%s: Names() = %v, want file order", name, got)
		}
		if got := tm.Members("team-alpha"); !reflect.DeepEqual(got, []string{"student-001", "student-003"}) {
			t.Errorf("%s: Members(team-alpha) = %v", name, got)
		}
		if tm.Len() != 2 {
			t.Errorf("%s: Len() = %d, want 2", name, tm.Len())
		}
	}
}

func TestMembersUnknownTeam(t *testing.T) {
	tm, err := Parse(strings.NewReader("a: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tm.Members("nope"); got != nil {
		t.Errorf("Members(nope) = %v, want nil", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"empty input":           "",
		"sequence at top level": "- a\n- b\n",
		"team value not a list": "team-alpha: student-001\n",
		"empty member":          "team-alpha: [\"\"]\n",
		"duplicate id in team":  "team-alpha: [x, x]\n",
		"empty team list":       "team-alpha: []\n",
	}
	for name, in := range cases {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
