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
	if limit < 1 {
		limit = 1
	}
	results := make([]R, len(items))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item T) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = fn(ctx, item)
		}(i, item)
	}
	wg.Wait()
	return results
}
