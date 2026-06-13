package gh

import (
	"context"
	"strings"
	"testing"
)

func TestGetTeam(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"id":42,"slug":"staff","name":"Staff"}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		team, exists, err := c.GetTeam(context.Background(), "org", "staff")
		if err != nil || !exists {
			t.Fatalf("want exists, got exists=%v err=%v", exists, err)
		}
		if team.ID != 42 || team.Slug != "staff" {
			t.Errorf("decoded %+v", team)
		}
		if f.paths[0] != "orgs/org/teams/staff" {
			t.Errorf("path = %q", f.paths[0])
		}
	})
	t.Run("absent on 404", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		team, exists, err := c.GetTeam(context.Background(), "org", "ghost")
		if err != nil || exists || team != nil {
			t.Fatalf("404 should be absent without error, got team=%v exists=%v err=%v", team, exists, err)
		}
	})
}

func TestCreateTeam(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{"id":7,"slug":"staff"}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	team, err := c.CreateTeam(context.Background(), "org", "staff")
	if err != nil {
		t.Fatal(err)
	}
	if team.ID != 7 {
		t.Errorf("decoded %+v", team)
	}
	if f.methods[0] != "POST" || f.paths[0] != "orgs/org/teams" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	for _, want := range []string{`"name":"staff"`, `"privacy":"closed"`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("body %s missing %s", f.bodies[0], want)
		}
	}
}

func TestAddTeamRepo(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.AddTeamRepo(context.Background(), "org", "staff", "org", "hw1-ada", "push"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PUT" || f.paths[0] != "orgs/org/teams/staff/repos/org/hw1-ada" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"permission":"push"`) {
		t.Errorf("body %s missing permission", f.bodies[0])
	}
}
