package gh

import (
	"context"
	"testing"
	"time"
)

// testClock is the injected time a policy runs on: waiting advances it exactly
// as sleeping advances real time, so a gate that re-checks the clock after each
// wait behaves in tests the way it does in a run.
type testClock struct {
	t     time.Time
	waits []time.Duration
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_000_000, 0)} }

func (c *testClock) now() time.Time { return c.t }

func (c *testClock) wait(_ context.Context, d time.Duration) error {
	c.waits = append(c.waits, d)
	c.t = c.t.Add(d)
	return nil
}

func TestGateHoldsUntilThePauseElapses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var g gate

	if d := g.hold(now); d > 0 {
		t.Fatalf("the zero value must be an open gate, got a %v hold", d)
	}
	g.block(now, 30*time.Second)
	if d := g.hold(now); d != 30*time.Second {
		t.Errorf("hold at the moment of the limit = %v, want 30s", d)
	}
	if d := g.hold(now.Add(29 * time.Second)); d != time.Second {
		t.Errorf("hold one second before the pause ends = %v, want 1s", d)
	}
	if d := g.hold(now.Add(30 * time.Second)); d > 0 {
		t.Errorf("the gate must be open once the pause elapses, got a %v hold", d)
	}
}

func TestGateKeepsTheLongerPause(t *testing.T) {
	// Two workers refused at once report their own estimates. The second, shorter
	// one must not cut the first one's wait short and send the run back into the
	// limit it is already waiting out.
	now := time.Unix(1_000_000, 0)
	var g gate

	g.block(now, time.Minute)
	g.block(now, 10*time.Second)
	if d := g.hold(now); d != time.Minute {
		t.Errorf("hold = %v, want the longer 1m pause to stand", d)
	}

	g.block(now, 2*time.Minute)
	if d := g.hold(now); d != 2*time.Minute {
		t.Errorf("hold = %v, want a longer pause to extend the gate", d)
	}
}

func TestGateAwaitWaitsOutThePause(t *testing.T) {
	c := newTestClock()
	var g gate
	start := c.now()
	g.block(start, 45*time.Second)

	if err := g.await(context.Background(), c.wait, c.now); err != nil {
		t.Fatal(err)
	}
	if len(c.waits) != 1 || c.waits[0] != 45*time.Second {
		t.Errorf("waits = %v, want one 45s wait", c.waits)
	}
	if got := c.now().Sub(start); got != 45*time.Second {
		t.Errorf("await returned %v after the limit, want 45s", got)
	}
}

func TestGateAwaitReChecksAnExtendedPause(t *testing.T) {
	// A worker already waiting must not resume the moment its own pause ends if
	// another worker has since been refused and extended the gate: resuming into
	// a limit that is still on just earns another rejection.
	c := newTestClock()
	var g gate
	start := c.now()
	g.block(start, 10*time.Second)

	extended := false
	wait := func(ctx context.Context, d time.Duration) error {
		if err := c.wait(ctx, d); err != nil {
			return err
		}
		if !extended {
			extended = true
			g.block(c.now(), 20*time.Second)
		}
		return nil
	}

	if err := g.await(context.Background(), wait, c.now); err != nil {
		t.Fatal(err)
	}
	if len(c.waits) != 2 {
		t.Fatalf("waits = %v, want the extended pause to be waited out too", c.waits)
	}
	if got := c.now().Sub(start); got != 30*time.Second {
		t.Errorf("await returned %v after the limit, want 30s (10s then the 20s extension)", got)
	}
}

func TestGateAwaitOpenGateDoesNotWait(t *testing.T) {
	c := newTestClock()
	var g gate

	if err := g.await(context.Background(), c.wait, c.now); err != nil {
		t.Fatal(err)
	}
	if len(c.waits) != 0 {
		t.Errorf("an open gate must not wait, got %v", c.waits)
	}
}

func TestGateAwaitStopsOnContextCancel(t *testing.T) {
	c := newTestClock()
	var g gate
	g.block(c.now(), time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := g.await(ctx, func(ctx context.Context, _ time.Duration) error { return ctx.Err() }, c.now)
	if err != context.Canceled {
		t.Errorf("await = %v, want context.Canceled", err)
	}
}
