// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"sync"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/logger"
)

// waitTimeout bounds every wait in this package's tests. It is a failure
// deadline, never an assertion: a test that needs it has already gone wrong.
const waitTimeout = 5 * time.Second

// logEntry is one captured log event.
type logEntry struct {
	level   string
	message string
	fields  map[string]any
}

// captureBackend is a logger.Backend that records every event and streams
// them on a channel so a test can block until a specific line appears. The
// logger backend is process-global, so tests that install one must not call
// t.Parallel.
type captureBackend struct {
	mu      sync.Mutex
	entries []logEntry
	ch      chan logEntry
	// onLog, when set, runs synchronously inside the logging call. It lets a
	// test react at an exact point in product code (e.g. advance a fake clock
	// the moment a job is enqueued) without a timing window.
	onLog func(logEntry)
}

// installCapture installs a captureBackend for the duration of the test and
// silences the logger again on cleanup.
func installCapture(t *testing.T) *captureBackend {
	t.Helper()
	c := &captureBackend{ch: make(chan logEntry, 1024)}
	logger.SetBackend(c)
	t.Cleanup(func() { logger.SetBackend(nil) })
	return c
}

func (c *captureBackend) Log(level, _ string, message string, fields map[string]any) {
	e := logEntry{level: level, message: message, fields: fields}
	c.mu.Lock()
	c.entries = append(c.entries, e)
	hook := c.onLog
	c.mu.Unlock()
	if hook != nil {
		hook(e)
	}
	select {
	case c.ch <- e:
	default:
	}
}

// wait blocks until an entry with the given message is logged, discarding
// earlier entries from the stream (they remain in entries for count).
func (c *captureBackend) wait(t *testing.T, message string) logEntry {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case e := <-c.ch:
			if e.message == message {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for log %q; logged: %v", message, c.messages())
		}
	}
}

// count reports how many entries carry the given message.
func (c *captureBackend) count(message string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.message == message {
			n++
		}
	}
	return n
}

func (c *captureBackend) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.level+": "+e.message)
	}
	return out
}
