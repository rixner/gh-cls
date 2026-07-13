package gh

import (
	"context"
	"testing"
)

func TestUserExists(t *testing.T) {
	t.Run("existing user returns true", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{resp: okResp(`{"login":"ada"}`)}}}
		var waits int
		c := newTestClient(f, &waits)

		exists, err := c.UserExists(context.Background(), "ada")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Error("a 200 should report the user exists")
		}
		if f.paths[0] != "users/ada" {
			t.Errorf("wrong path %q", f.paths[0])
		}
	})

	t.Run("missing user returns false, no error", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(404, nil)}}}
		var waits int
		c := newTestClient(f, &waits)

		exists, err := c.UserExists(context.Background(), "nope")
		if err != nil {
			t.Fatalf("a 404 is a clean non-existence, not an error: %v", err)
		}
		if exists {
			t.Error("a 404 should report the user does not exist")
		}
	})

	t.Run("other errors propagate", func(t *testing.T) {
		f := &fakeRequester{steps: []step{{err: httpErr(500, nil)}}}
		var waits int
		c := newTestClient(f, &waits)

		if _, err := c.UserExists(context.Background(), "ada"); err == nil {
			t.Fatal("a 5xx must not be mistaken for a non-existent user")
		}
	})
}

func TestUserExistsEscapesLogin(t *testing.T) {
	f := &fakeRequester{steps: []step{{resp: okResp(`{}`)}}}
	var waits int
	c := newTestClient(f, &waits)

	if _, err := c.UserExists(context.Background(), "a b"); err != nil {
		t.Fatal(err)
	}
	if f.paths[0] != "users/a%20b" {
		t.Errorf("login should be path-escaped, got %q", f.paths[0])
	}
}
