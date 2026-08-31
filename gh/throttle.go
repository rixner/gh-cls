package gh

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// contentSpacing is the minimum interval between requests that create content:
// a repository, a commit, a ref, a pull request, an issue. This is the tight
// budget, and the one that refused a class-sized assign run.
//
// GitHub allows 80 content-creating requests a minute, and 800ms holds a run to
// 75. The margin is deliberate: a rate set to the ceiling exactly crosses it on
// measurement noise alone, which real runs at 750ms were observed doing.
//
// Where above 75 it stops being safe is unknown. Real runs have only ever
// confirmed the rates below it.
//
// Pacing the operation, rather than bounding how many repositories are worked on
// at once, is what makes the guarantee hold: the rate is enforced where the
// request is made, so no amount of parallelism above it can raise it.
const contentSpacing = 800 * time.Millisecond

// accessSpacing is the minimum interval between requests that only change who
// can reach something: a collaborator grant, an invitation, a team's repositories
// or members. These create no content, so the only limit they run into is the
// 900 points a minute an endpoint allows, and a write costs five of those: 180 a
// minute, or one every 333ms.
//
// They get their own rate because freeze is made entirely of them and is a
// deadline. Pacing a freeze as though it were creating content would leave a
// whole class writable for minutes past the moment it was supposed to close,
// which is a correctness cost, not a speed one.
const accessSpacing = 333 * time.Millisecond

// readSpacing is the minimum interval between reads, run-wide. Reads are limited
// far more loosely than writes (5,000 an hour, and one point each against the
// 900 per minute an endpoint allows, so fifteen a second), and a course-sized
// org has few enough repositories that no command comes near the per-minute
// ceiling on its own. Ten a second keeps a margin under it whatever the run is
// doing, and costs a hundred-repository audit about twenty seconds.
const readSpacing = 100 * time.Millisecond

// maxInFlight caps how many requests the run has outstanding at once. GitHub
// allows no more than 100 concurrent requests across its REST and GraphQL APIs;
// half of that leaves room for the tool's own accounting being off and still sits
// far above anything the rates can produce, since requests in flight is only rate
// times latency (ten reads a second answered in a quarter second is three).
//
// So this engages only when responses hang, which is exactly the case the rates
// cannot cover. It is here rather than in the size of the worker pool because a
// documented limit should be enforced where the requests are made, not inferred
// from how much work happens to be running.
const maxInFlight = 50

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

// ContentPerHour is GitHub's documented hourly ceiling on content-creating
// requests. Nothing paces to it, because whether it governs repository
// generation is unproven and obeying it would turn a class provision that
// completes in twenty minutes into a two-hour one on a guess. A caller that
// expects to exceed it can say so instead.
const ContentPerHour = 500

// RequestsPerHour is GitHub's primary rate limit: every request an authenticated
// user makes counts against it, reads included. A command that expects to exceed
// it can say so, since the run will pause until the hour resets rather than fail.
const RequestsPerHour = 5000

// Cost counts the requests an operation will make, by the budget each draws on.
// A caller that can say what it is about to do can ask how long the pacing will
// take before it starts, which is what lets a command state its own duration up
// front rather than leaving the instructor to discover it mid-run.
type Cost struct {
	Reads   int
	Content int
	Access  int
}

// Duration reports how long issuing c takes at the client's rates. The three
// budgets are independent and drain in parallel, so a run is no faster than the
// slowest of them.
//
// It is a floor. It counts the waiting the rates impose and not the time GitHub
// takes to answer, nor the poll for a generated repository to become ready, nor
// any pause a rate limit causes.
func (c Cost) Duration() time.Duration {
	d := time.Duration(c.Reads) * readSpacing
	if w := time.Duration(c.Content) * contentSpacing; w > d {
		d = w
	}
	if w := time.Duration(c.Access) * accessSpacing; w > d {
		d = w
	}
	return d
}
