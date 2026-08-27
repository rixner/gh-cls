package gh

import (
	"context"
	"sync"
	"time"
)

// gate is the run-wide pause every request observes. GitHub's rate limits are
// per token, not per request, so a limit one request runs into is already in
// force for every other request the run is making: the sibling worker about to
// issue its own create will be refused too, and continuing to send while limited
// is what GitHub's own guidance warns can get an integration banned. Recording
// the limit here, rather than sleeping only inside the refused request's retry
// loop, is what makes the whole run back off together and then continue, instead
// of the other workers burning their attempts against a limit that is still on.
//
// The zero value is an open gate.
type gate struct {
	mu    sync.Mutex
	until time.Time
}

// block records that GitHub is refusing requests for at least d from now. An
// existing, later pause is kept: two workers refused at once must not let the
// second one's shorter estimate cut the first one's wait short.
func (g *gate) block(now time.Time, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if u := now.Add(d); u.After(g.until) {
		g.until = u
	}
}

// hold reports how long a request must wait before it may be issued, or a
// non-positive duration when the gate is open.
func (g *gate) hold(now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until.Sub(now)
}

// await blocks until the gate is open or ctx is done. It re-checks after each
// wait because another worker may have extended the pause in the meantime, and
// resuming early would just earn another rejection.
func (g *gate) await(ctx context.Context, wait func(context.Context, time.Duration) error, now func() time.Time) error {
	for {
		d := g.hold(now())
		if d <= 0 {
			return ctx.Err()
		}
		if err := wait(ctx, d); err != nil {
			return err
		}
	}
}
