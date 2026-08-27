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

func TestPacerSpacesWritesInOrderOfArrival(t *testing.T) {
	// Three workers reaching the pacer at the same instant must be spread out,
	// each behind the last, rather than all waiting the same interval and then
	// writing together, which is the burst the spacing exists to prevent.
	now := time.Unix(1_000_000, 0)
	p := pacer{spacing: 750 * time.Millisecond}

	want := []time.Duration{0, 750 * time.Millisecond, 1500 * time.Millisecond}
	for i, w := range want {
		if got := p.reserve(now); got != w {
			t.Errorf("reservation %d = %v, want %v", i+1, got, w)
		}
	}
}

func TestPacerDoesNotDelayAnIdleRun(t *testing.T) {
	// A run whose writes are already further apart than the spacing pays nothing:
	// the pacer is a ceiling on the rate, not a delay on every write.
	now := time.Unix(1_000_000, 0)
	p := pacer{spacing: 750 * time.Millisecond}

	if d := p.reserve(now); d != 0 {
		t.Errorf("first write waited %v, want none", d)
	}
	if d := p.reserve(now.Add(2 * time.Second)); d != 0 {
		t.Errorf("a write two seconds later waited %v, want none", d)
	}
}

func TestPacerZeroValuePacesNothing(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var p pacer
	for i := 0; i < 3; i++ {
		if d := p.reserve(now); d != 0 {
			t.Errorf("reservation %d = %v, want no pacing from the zero value", i+1, d)
		}
	}
}

func TestMutatingClassifiesMethods(t *testing.T) {
	for _, m := range []string{"GET", "HEAD"} {
		if mutating(m) {
			t.Errorf("%s is a read and must not be paced", m)
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if !mutating(m) {
			t.Errorf("%s is a write and must be paced", m)
		}
	}
}

func TestGateReportsAFreshPauseOnce(t *testing.T) {
	// Several workers refused at once are one pause, not several: the run says it
	// is waiting a single time, then says so again only if it is refused after
	// resuming.
	now := time.Unix(1_000_000, 0)
	var reported []time.Duration
	g := gate{notify: func(d time.Duration) { reported = append(reported, d) }}

	g.block(now, time.Minute)
	g.block(now.Add(time.Second), time.Minute)
	g.block(now.Add(2*time.Second), time.Minute)
	if len(reported) != 1 || reported[0] != time.Minute {
		t.Errorf("reported %v, want one 1m pause", reported)
	}

	g.block(now.Add(5*time.Minute), 2*time.Minute)
	if len(reported) != 2 || reported[1] != 2*time.Minute {
		t.Errorf("reported %v, want a second pause once the first had ended", reported)
	}
}

func TestCostTakesTheSlowestBudget(t *testing.T) {
	// The three rates drain in parallel, so a run is as long as its slowest
	// budget, not the sum of them. Reporting the sum would tell an instructor a
	// twenty-minute run takes half an hour.
	content := Cost{Content: 400, Access: 100, Reads: 100}
	if got, want := content.Duration(), 300*time.Second; got != want {
		t.Errorf("content-bound run = %v, want %v", got, want)
	}
	access := Cost{Content: 10, Access: 900, Reads: 100}
	if got, want := access.Duration(), 299700*time.Millisecond; got != want {
		t.Errorf("access-bound run = %v, want %v", got, want)
	}
	if got := (Cost{}).Duration(); got != 0 {
		t.Errorf("an empty run = %v, want 0", got)
	}
}
