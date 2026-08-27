package gh

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// contentSpacing is the minimum interval between requests that create content:
// a repository, a commit, a ref, a pull request, an issue. This is the tight
// budget, and the one that refused a class-sized assign run. GitHub allows 80
// content-creating requests a minute, and provisioning one repository costs
// about six of them, so three every two seconds holds a run near 50 a minute:
// under the documented ceiling with room to spare, and still roughly nine
// repositories a minute.
//
// Pacing the operation, rather than bounding how many repositories are worked on
// at once, is what makes the guarantee hold: the rate is enforced where the
// request is made, so no amount of parallelism above it can raise it.
const contentSpacing = 750 * time.Millisecond

// accessSpacing is the minimum interval between requests that only change who
// can reach something: a collaborator grant, an invitation, a team's repositories
// or members. These create no content, so the only limit they run into is the
// 900 points a minute an endpoint allows, and a write costs five of those: 180 a
// minute, or one every 333ms.
//
// They get their own rate because freeze is made entirely of them and is a
// deadline. Pacing a freeze as though it were creating content would leave a
// class of 183 writable for four minutes past the moment it was supposed to
// close, which is a correctness cost, not a speed one.
const accessSpacing = 333 * time.Millisecond

// readSpacing is the minimum interval between reads, run-wide. Reads are limited
// far more loosely than writes (5,000 an hour, and one point each against the
// 900 per minute an endpoint allows, so fifteen a second), and a course-sized
// org has few enough repositories that no command comes near the per-minute
// ceiling on its own. Ten a second keeps a margin under it whatever the run is
// doing, and costs a hundred-repository audit about twenty seconds.
const readSpacing = 100 * time.Millisecond

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
	mu     sync.Mutex
	until  time.Time
	notify func(time.Duration)
}

// block records that GitHub is refusing requests for at least d from now. An
// existing, later pause is kept: two workers refused at once must not let the
// second one's shorter estimate cut the first one's wait short.
//
// Only a pause that starts one reports itself, so a run that is refused several
// times over while already waiting says so once rather than once per refused
// request. The report happens under the lock, which keeps concurrent pauses from
// interleaving mid-line with each other.
func (g *gate) block(now time.Time, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fresh := !g.until.After(now)
	if u := now.Add(d); u.After(g.until) {
		g.until = u
	}
	if fresh && g.notify != nil {
		g.notify(g.until.Sub(now))
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

// pacer spaces mutating requests across the whole run. Its zero value paces
// nothing, which is what a client built without a spacing wants.
type pacer struct {
	mu      sync.Mutex
	spacing time.Duration
	next    time.Time
}

// reserve claims this request's turn and reports how long the caller must wait
// to take it. Turns are handed out in order of arrival, so concurrent workers
// queue behind one another instead of all waiting the same interval and then
// firing together.
func (p *pacer) reserve(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spacing <= 0 {
		return 0
	}
	if p.next.After(now) {
		d := p.next.Sub(now)
		p.next = p.next.Add(p.spacing)
		return d
	}
	p.next = now.Add(p.spacing)
	return 0
}

// mutating reports whether a method changes state. Reads are governed by limits
// far looser than the ones writes run into (5,000 requests an hour, one point
// each), so pacing them would slow the read-heavy commands for nothing.
func mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
}
