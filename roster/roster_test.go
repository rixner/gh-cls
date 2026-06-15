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
