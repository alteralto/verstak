// Package trace writes one JSON line per step of a job, with times from the job's start, so a
// run can be measured afterwards (where the minutes went: the agent, the tests, a retry).
package trace

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Writer is safe for use from several goroutines.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
	t0 time.Time
}

func New(w io.Writer) *Writer { return &Writer{w: w, t0: time.Now()} }

// Span records a step that began at start and ends now. kv are extra fields, key then value.
func (t *Writer) Span(name string, start time.Time, err error, kv ...any) {
	if t == nil {
		return
	}
	rec := map[string]any{
		"span":  name,
		"at_ms": start.Sub(t.t0).Milliseconds(),
		"ms":    time.Since(start).Milliseconds(),
		"ok":    err == nil,
	}
	if err != nil {
		rec["err"] = err.Error()
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			rec[k] = kv[i+1]
		}
	}
	line, _ := json.Marshal(rec)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w.Write(append(line, '\n'))
}
