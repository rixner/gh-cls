package ghtest

import (
	"context"
	"sync"
	"testing"
)

func TestFakePanicsWhenFuncUnset(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic when OrgRoleFunc is unset")
		}
	}()
	(&Fake{}).OrgRole(context.Background(), "org")
}

func TestFakeRecordsCallsConcurrently(t *testing.T) {
	fk := &Fake{OrgRoleFunc: func(context.Context, string) (string, error) { return "admin", nil }}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := fk.OrgRole(context.Background(), "org"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if len(fk.Calls) != 50 {
		t.Errorf("got %d recorded calls, want 50", len(fk.Calls))
	}
	for _, c := range fk.Calls {
		if c != "OrgRole" {
			t.Errorf("unexpected call record %q", c)
		}
	}
}
