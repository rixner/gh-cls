package cmd

import (
	"context"
	"sort"
	"testing"
)

func TestRunConcurrentProgressReportsEveryItemExactlyOnce(t *testing.T) {
	// The callback is what a long run's progress output hangs off, so it must fire
	// once per item and never concurrently with itself. Both the unguarded counter
	// and the unguarded slice below are races if runConcurrentProgress serializes
	// nothing, which `go test -race` reports.
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}

	calls := 0
	var seen []int
	results := runConcurrentProgress(context.Background(), 8, items,
		func(_ context.Context, i int) int { return i * 2 },
		func(r int) {
			calls++
			seen = append(seen, r)
		})

	if calls != len(items) {
		t.Errorf("callback ran %d times, want one per item (%d)", calls, len(items))
	}
	if len(results) != len(items) {
		t.Fatalf("got %d results, want %d", len(results), len(items))
	}
	for i, r := range results {
		if r != i*2 {
			t.Errorf("results are not in input order: results[%d] = %d", i, r)
		}
	}
	// Completion order is not input order, so compare as sets.
	sort.Ints(seen)
	for i, r := range seen {
		if r != i*2 {
			t.Fatalf("the callback did not see every result: %v", seen)
		}
	}
}

func TestRunConcurrentTakesNoCallback(t *testing.T) {
	// runConcurrent delegates with a nil callback, which must not panic.
	got := runConcurrent(context.Background(), 4, []int{1, 2, 3}, func(_ context.Context, i int) int { return i + 1 })
	if len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Errorf("runConcurrent = %v, want [2 3 4]", got)
	}
}
