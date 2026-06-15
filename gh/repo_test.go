package gh

import (
	"context"
	"strings"
	"testing"
)

func TestGetRepo(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"name":"hw1-ada","default_branch":"main","has_issues":true}`)}}}
		var waits int
		c := newTestClient(f, &waits)
		repo, exists, err := c.GetRepo(context.Background(), "org", "hw1-ada")
		if err != nil || !exists {
			t.Fatalf("want exists, got exists=%v err=%v", exists, err)
		}
		if repo.Name != "hw1-ada" || repo.DefaultBranch != "main" || !repo.HasIssues {
			t.Errorf("decoded %+v", repo)
		}
		if f.methods[0] != "GET" || f.paths[0] != "repos/org/hw1-ada" {
			t.Errorf("request = %s %s", f.methods[0], f.paths[0])
		}
	})
	t.Run("absent on 404", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		repo, exists, err := c.GetRepo(context.Background(), "org", "missing")
		if err != nil || exists || repo != nil {
			t.Fatalf("404 should be absent without error, got repo=%v exists=%v err=%v", repo, exists, err)
		}
	})
	t.Run("propagates other errors", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
		var waits int
		c := newTestClient(f, &waits)
		if _, _, err := c.GetRepo(context.Background(), "org", "x"); err == nil {
			t.Fatal("server error should propagate, not read as absent")
		}
	})
}

func TestSetRepoTemplate(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.SetRepoTemplate(context.Background(), "org", "hw1-template"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PATCH" || f.paths[0] != "repos/org/hw1-template" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"is_template":true`) {
		t.Errorf("body %s missing is_template", f.bodies[0])
	}
}

func TestDeleteRepo(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.DeleteRepo(context.Background(), "org", "hw1-old"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "DELETE" || f.paths[0] != "repos/org/hw1-old" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
}

func TestGenerateFromTemplate(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	err := c.GenerateFromTemplate(context.Background(), "org", "hw1-template", "org", "hw1-ada", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "POST" || f.paths[0] != "repos/org/hw1-template/generate" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	for _, want := range []string{`"include_all_branches":false`, `"name":"hw1-ada"`, `"owner":"org"`, `"private":true`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("body %s missing %s", f.bodies[0], want)
		}
	}
}

func TestAddCollaborator(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)
	if err := c.AddCollaborator(context.Background(), "org", "hw1-ada", "ada", "push"); err != nil {
		t.Fatal(err)
	}
	if f.methods[0] != "PUT" || f.paths[0] != "repos/org/hw1-ada/collaborators/ada" {
		t.Errorf("request = %s %s", f.methods[0], f.paths[0])
	}
	if !strings.Contains(f.bodies[0], `"permission":"push"`) {
		t.Errorf("body %s missing permission", f.bodies[0])
	}
}
