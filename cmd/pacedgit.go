package cmd

import (
	"context"
	"sync"
	"time"
)

// cloneSpacing is the minimum gap between the git operations that reach GitHub.
//
// Git operations are governed separately from the REST API, and GitHub
// publishes no ceiling for the pattern collect has: one clone each of many
// different repositories, in quick succession. Its own guidance says only that
// heavy Git traffic may be slowed rather than refused, which is not what has
// been observed. Real class-sized collections have completed reliably at three
// seconds serialized, and failed when pushed faster.
//
// So this is an upper bound on what is needed rather than a measured threshold,
// and it can come down if a run is ever instrumented to find the real one. It
// costs a collection a few minutes, which is cheap for a command run once per
// assignment.
const cloneSpacing = 3 * time.Second

// pacedGit rate-limits the git operations that talk to GitHub and leaves the
// local ones alone, so the rate holds however many repositories the run is
// working through. It serializes those operations as well as spacing them,
// because serial is the shape the spacing was measured in: nothing is known
// about several clones running at once three seconds apart, so the run does not
// go there.
//
// Everything else in gitRunner (reading a worktree, checking a tag) is embedded
// and passes straight through.
type pacedGit struct {
	gitRunner
	spacing time.Duration
	sleep   func(time.Duration)
	now     func() time.Time

	mu   sync.Mutex
	next time.Time
}

// newPacedGit wraps a runner so its GitHub-facing operations are paced.
func newPacedGit(inner gitRunner, spacing time.Duration) *pacedGit {
	return &pacedGit{gitRunner: inner, spacing: spacing, sleep: time.Sleep, now: time.Now}
}

// Clone takes its turn, then holds the next one off for the spacing measured
// from when this one finished, which is where the gap was measured from.
func (p *pacedGit) Clone(ctx context.Context, org, repo, dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold(ctx)
	err := p.gitRunner.Clone(ctx, org, repo, dir)
	p.next = p.now().Add(p.spacing)
	return err
}

// Fetch is paced with Clone, and against the same turn: both reach GitHub, so
// the rate has to cover them together rather than each separately.
func (p *pacedGit) Fetch(ctx context.Context, dir, ref string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold(ctx)
	forced, err := p.gitRunner.Fetch(ctx, dir, ref)
	p.next = p.now().Add(p.spacing)
	return forced, err
}

// hold waits for this operation's turn. It is called with the lock held, so the
// operations queue rather than all waking together. An interrupted run stops
// waiting at once: the queue behind a cancelled run is long, and unwinding it
// three seconds at a time would leave the instructor watching a Ctrl-C do
// nothing.
func (p *pacedGit) hold(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if d := p.next.Sub(p.now()); d > 0 {
		p.sleep(d)
	}
}
