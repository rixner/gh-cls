package cmd

import (
	"context"
	"sync"
)

// runConcurrent applies fn to every item using at most limit concurrent
// workers, returning results in input order. Every item is attempted regardless
// of others' failures (fn reports per-item errors in its result), which suits
// the idempotent, partial-progress-tolerant bulk operations here.
func runConcurrent[T, R any](ctx context.Context, limit int, items []T, fn func(context.Context, T) R) []R {
	return runConcurrentProgress(ctx, limit, items, fn, nil)
}

// runConcurrentProgress is runConcurrent with a callback run as each item
// finishes, so a bulk operation can report progress instead of going silent for
// minutes. onDone (nil for none) is called once per item, in completion order
// rather than input order, and never concurrently with itself, so a callback
// writing to a shared io.Writer needs no locking of its own. It runs on the
// worker's goroutine, so a slow callback slows that worker: keep it to a line of
// output.
func runConcurrentProgress[T, R any](ctx context.Context, limit int, items []T, fn func(context.Context, T) R, onDone func(R)) []R {
	if limit < 1 {
		limit = 1
	}
	results := make([]R, len(items))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item T) {
			defer wg.Done()
			defer func() { <-sem }()
			r := fn(ctx, item)
			results[i] = r
			if onDone != nil {
				mu.Lock()
				defer mu.Unlock()
				onDone(r)
			}
		}(i, item)
	}
	wg.Wait()
	return results
}
