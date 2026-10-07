package roster

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	in := "identifier,username\nstudent-001,ada\nstudent-002,alan\n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", r.Len())
	}
	if u, ok := r.Lookup("student-001"); !ok || u != "ada" {
		t.Errorf("Lookup(student-001) = %q,%v want ada,true", u, ok)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Error("Lookup(missing) should report not found")
	}
	if got := r.IDs(); !reflect.DeepEqual(got, []string{"student-001", "student-002"}) {
		t.Errorf("IDs() = %v, want file order", got)
	}
}

func TestByUsername(t *testing.T) {
	in := "identifier,username\nstudent-001,Ada\nstudent-002,alan\n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	rev := r.ByUsername()
	// Lookups are case-insensitive, since GitHub logins are.
	if got := rev["ada"]; got != "student-001" {
		t.Errorf("ByUsername()[ada] = %q, want student-001", got)
	}
	if got := rev["alan"]; got != "student-002" {
		t.Errorf("ByUsername()[alan] = %q, want student-002", got)
	}
	if _, ok := rev["missing"]; ok {
		t.Error("ByUsername() should not contain an unknown username")
	}
}

func TestUsersByLowercase(t *testing.T) {
	in := "identifier,username\nstudent-001,Ada\nstudent-002,alan\n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	got := r.UsersByLowercase()
	want := map[string]string{"ada": "Ada", "alan": "alan"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UsersByLowercase() = %v, want %v", got, want)
	}
}

func TestParseColumnsCaseAndOrderInsensitive(t *testing.T) {
	in := "USERNAME, Identifier\nada, student-001\n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := r.Lookup("student-001"); !ok || u != "ada" {
		t.Errorf("got %q,%v want ada,true", u, ok)
	}
}

func TestParseStripsBOMAndWhitespace(t *testing.T) {
	in := "\ufeffidentifier,username\n  student-001 , ada \n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := r.Lookup("student-001"); !ok || u != "ada" {
		t.Errorf("got %q,%v want ada,true (BOM/whitespace not handled)", u, ok)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"missing username column":   "identifier\nstudent-001\n",
		"missing identifier column": "username\nada\n",
		"duplicate identifier":      "identifier,username\ns1,ada\ns1,alan\n",
		"duplicate username":        "identifier,username\ns1,ada\ns2,ada\n",
		"duplicate username case":   "identifier,username\ns1,ada\ns2,Ada\n",
		"empty username":            "identifier,username\ns1,\n",
		"empty identifier":          "identifier,username\n,ada\n",
		"header only":               "identifier,username\n",
		"empty input":               "",
	}
	for name, in := range cases {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestParseDuplicateUsernameError checks the rejection names the colliding
// username and the earlier line, and treats case-different logins as the same
// account (GitHub usernames are case-insensitive).
func TestParseDuplicateUsernameError(t *testing.T) {
	in := "identifier,username\ns1,Ada\ns2,ada\n"
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected a duplicate-username error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ada") || !strings.Contains(msg, "line 2") {
		t.Errorf("error should name the username and the first line, got: %v", err)
	}
}

// TestParseWithoutAccessColumn checks a roster that predates the access column
// reads every student as enrolled.
func TestParseWithoutAccessColumn(t *testing.T) {
	r, err := Parse(strings.NewReader("identifier,username\ns1,ada\ns2,alan\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range r.IDs() {
		if a := r.Access(id); a != AccessEnrolled {
			t.Errorf("Access(%s) = %q, want enrolled", id, a)
		}
	}
}

// TestParseAccessOnlyOnMarkedRows checks the access column can be added to the
// header and filled in only for the students who dropped, without a trailing
// comma on every other row. encoding/csv rejects that by default, so this
// guards the relaxation.
func TestParseAccessOnlyOnMarkedRows(t *testing.T) {
	in := "identifier,username,access\ns1,ada\ns2,alan,read\ns3,grace,NONE\ns4,katherine,own\ns5,margaret,\n"
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Access{"s1": AccessEnrolled, "s2": AccessRead, "s3": AccessNone, "s4": AccessOwn, "s5": AccessEnrolled}
	for id, w := range want {
		if a := r.Access(id); a != w {
			t.Errorf("Access(%s) = %q, want %q", id, a, w)
		}
	}
	if r.Len() != 5 {
		t.Errorf("Len() = %d, want 5", r.Len())
	}
}

// TestParseAccessColumnNotLast checks the column may sit anywhere in the header,
// though a row can then only omit it by leaving the position empty.
func TestParseAccessColumnNotLast(t *testing.T) {
	r, err := Parse(strings.NewReader("access,identifier,username\n,s1,ada\nown,s2,alan\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Access("s2"); a != AccessOwn {
		t.Errorf("Access(s2) = %q, want own", a)
	}
	if a := r.Access("s1"); a != AccessEnrolled {
		t.Errorf("Access(s1) = %q, want enrolled", a)
	}
}

func TestParseAccessErrors(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"unknown value":      {"identifier,username,access\ns1,ada,gone\n", "line 2"},
		"more fields":        {"identifier,username,access\ns1,ada,read,extra\n", "line 2"},
		"extra without col":  {"identifier,username\ns1,ada,read\n", "line 2"},
		"short row username": {"identifier,username,access\ns1,ada\ns2\n", "line 3"},
	}
	for name, c := range cases {
		_, err := Parse(strings.NewReader(c.in))
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error should name %s, got: %v", name, c.want, err)
		}
	}
}
