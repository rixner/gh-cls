package cmd

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestSyncWriterKeepsConcurrentLinesWhole(t *testing.T) {
	// A bulk run prints from the worker that just finished a repository and from
	// the client that just ran into a rate limit, at the same moment. Both go
	// through this, so neither can land in the middle of the other's line, and
	// the underlying writer (a buffer here, as in every command test) never sees
	// two concurrent writes.
	var buf bytes.Buffer
	w := &syncWriter{w: &buf}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fmt.Fprintf(w, "  [%d] a line long enough to be split if it were not serialized\n", i)
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	want := make([]string, 50)
	for i := range want {
		want[i] = fmt.Sprintf("  [%d] a line long enough to be split if it were not serialized", i)
	}
	sort.Strings(lines)
	sort.Strings(want)
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}
