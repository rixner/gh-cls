package cmd

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// paceClock is the time the pacer runs on: only sleeping advances it, and it is
// safe to read from the goroutines a concurrent collection would use.
type paceClock struct {
	mu    sync.Mutex
	t     time.Time
	start time.Time
}

func (c *paceClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *paceClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *paceClock) since() time.Duration { return c.now().Sub(c.start) }

// recordingGit records when each operation ran, on the clock the pacer runs on,
// and how many were in flight at once.
type recordingGit struct {
	gitRunner
	clock *paceClock

	mu       sync.Mutex
	at       []time.Duration // when each GitHub-facing call ran, from the start
	inFlight int
	most     int
	locals   int
	err      error
}

func (r *recordingGit) enter() {
	r.mu.Lock()
	r.at = append(r.at, r.clock.since())
	r.inFlight++
	if r.inFlight > r.most {
		r.most = r.inFlight
	}
	r.mu.Unlock()

	// A real clone takes time. Yielding here is what gives an unserialized caller
	// the chance to overlap, so the in-flight count can actually catch one.
	runtime.Gosched()

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

func (r *recordingGit) Clone(_ context.Context, _, _, _ string) error {
	r.enter()
	return r.err
}

func (r *recordingGit) Fetch(_ context.Context, _, _ string) (bool, error) {
	r.enter()
	return true, r.err
}

func (r *recordingGit) CloneExists(string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.locals++
	return true
}

// newPacedTest builds a paced runner on a clock that only sleeping advances.
func newPacedTest(spacing time.Duration) (*pacedGit, *recordingGit, *paceClock) {
	start := time.Unix(1_000_000, 0)
	clock := &paceClock{t: start, start: start}
	rec := &recordingGit{clock: clock}
	p := newPacedGit(rec, spacing)
	p.now = clock.now
	p.sleep = clock.sleep
	return p, rec, clock
}

func TestClonesAreSpacedAndSerial(t *testing.T) {
	// Driven from several goroutines, as a collection runs it: the spacing has to
	// hold across the whole run, not just for one caller, and no two clones may
	// overlap, since serial is the shape the three seconds was measured in.
	p, rec, clock := newPacedTest(3 * time.Second)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Clone(ctx, "org", "hw1-s01", "dir"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if rec.most != 1 {
		t.Errorf("%d clones ran at once, want them serialized", rec.most)
	}
	sort.Slice(rec.at, func(i, j int) bool { return rec.at[i] < rec.at[j] })
	for i, at := range rec.at {
		if want := time.Duration(i) * 3 * time.Second; at != want {
			t.Errorf("clone %d ran at %v, want %v", i+1, at, want)
		}
	}
	if got := clock.since(); got != 21*time.Second {
		t.Errorf("eight clones spanned %v, want 21s", got)
	}
}

func TestFetchesShareTheCloneRate(t *testing.T) {
	// Both reach GitHub, so alternating between them must not let the run go at
	// twice the rate either one is allowed.
	p, rec, _ := newPacedTest(3 * time.Second)
	ctx := context.Background()

	if err := p.Clone(ctx, "org", "hw1-s01", "dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Fetch(ctx, "dir", "main"); err != nil {
		t.Fatal(err)
	}
	if err := p.Clone(ctx, "org", "hw1-s02", "dir2"); err != nil {
		t.Fatal(err)
	}

	want := []time.Duration{0, 3 * time.Second, 6 * time.Second}
	for i, w := range want {
		if rec.at[i] != w {
			t.Errorf("operation %d ran at %v, want %v", i+1, rec.at[i], w)
		}
	}
}

func TestLocalGitOperationsAreNotPaced(t *testing.T) {
	// Reading a worktree does not touch GitHub. Pacing it would slow a collection
	// for nothing, and these run far more often than the clones do.
	p, rec, clock := newPacedTest(3 * time.Second)

	for i := 0; i < 100; i++ {
		p.CloneExists("dir")
	}
	if rec.locals != 100 {
		t.Errorf("%d local calls passed through, want 100", rec.locals)
	}
	if len(rec.at) != 0 {
		t.Errorf("local calls took a turn in the pacing: %v", rec.at)
	}
	if got := clock.since(); got != 0 {
		t.Errorf("local calls advanced the clock by %v, want none", got)
	}
}

func TestPacingStopsWaitingWhenTheRunIsCancelled(t *testing.T) {
	// The queue behind a cancelled run is as long as the class, and unwinding it
	// three seconds at a time would leave a Ctrl-C looking ignored.
	p, rec, clock := newPacedTest(3 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	if err := p.Clone(ctx, "org", "hw1-s01", "dir"); err != nil {
		t.Fatal(err)
	}
	cancel()
	for i := 0; i < 3; i++ {
		_ = p.Clone(ctx, "org", "hw1-s02", "dir2")
	}

	if got := clock.since(); got != 0 {
		t.Errorf("a cancelled run waited %v, want none", got)
	}
	if len(rec.at) != 4 {
		t.Errorf("%d operations ran, want the cancelled ones to fall straight through", len(rec.at))
	}
}

func TestPacingHoldsTheNextTurnEvenWhenAnOperationFails(t *testing.T) {
	// A failed clone still reached GitHub. Letting the next one go early because
	// this one errored would speed the run up exactly when it is going wrong.
	p, rec, _ := newPacedTest(3 * time.Second)
	rec.err = errors.New("clone failed")
	ctx := context.Background()

	if err := p.Clone(ctx, "org", "hw1-s01", "dir"); err == nil {
		t.Fatal("want the underlying error surfaced")
	}
	rec.err = nil
	if err := p.Clone(ctx, "org", "hw1-s02", "dir2"); err != nil {
		t.Fatal(err)
	}
	if rec.at[1] != 3*time.Second {
		t.Errorf("the clone after a failure ran at %v, want 3s", rec.at[1])
	}
}
