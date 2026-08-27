package gh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// requestLog records one JSON line per API request: what was sent, what came
// back, and the rate-limit headers that came with it.
//
// It exists because GitHub documents some of the limits a bulk run brushes
// against and not others. The per-minute and hourly ceilings on content creation
// are published; the threshold behind the 422 that answers a burst of repository
// generation is not, and neither is what it counts. A log of a real run is the
// only way to see where that line actually falls, so the pacing can be set from
// measurement rather than from inference.
type requestLog struct {
	mu sync.Mutex
	w  io.Writer
}

// logEntry is one request as recorded. The rate-limit headers are kept under
// short names so a run's log can be read without unfolding GitHub's header
// spelling on every line.
type logEntry struct {
	Time    string            `json:"time"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Attempt int               `json:"attempt"`
	Status  int               `json:"status,omitempty"`
	Millis  int64             `json:"ms"`
	HeldMs  int64             `json:"held_ms,omitempty"`
	Limits  map[string]string `json:"limits,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// record appends one entry. A nil log, the default, records nothing. A write
// that fails abandons the log and reports the failure to its caller once: the
// caller decides what to do about it, and every later record is a silent no-op
// rather than the same failure repeated for every remaining request.
func (l *requestLog) record(e logEntry) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		l.w = nil
		return fmt.Errorf("encoding the %s %s log entry: %w", e.Method, e.Path, err)
	}
	if _, err := l.w.Write(append(line, '\n')); err != nil {
		l.w = nil
		return fmt.Errorf("writing the %s %s log entry: %w", e.Method, e.Path, err)
	}
	return nil
}

// limitHeaders are the response headers that say where a request left the
// account against GitHub's limits.
var limitHeaders = map[string]string{
	"X-RateLimit-Limit":     "limit",
	"X-RateLimit-Remaining": "remaining",
	"X-RateLimit-Used":      "used",
	"X-RateLimit-Reset":     "reset",
	"X-RateLimit-Resource":  "resource",
	"Retry-After":           "retry_after",
}

// limitSnapshot picks the rate-limit headers out of a response, or reports nil
// when it carried none.
func limitSnapshot(h http.Header) map[string]string {
	var out map[string]string
	for header, name := range limitHeaders {
		v := h.Get(header)
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(limitHeaders))
		}
		out[name] = v
	}
	return out
}
