// cogmem - Cognitive Memory
// License: MIT

package logger

import (
	"reflect"
	"testing"
)

// event is one captured Log call, every argument kept.
type event struct {
	level, component, message string
	fields                    map[string]any
}

type capture struct {
	events []event
}

func (c *capture) Log(level, component, message string, fields map[string]any) {
	c.events = append(c.events, event{level, component, message, fields})
}

func TestSilentUntilBackendInstalled(t *testing.T) {
	SetBackend(nil)
	InfoCF("cogmem", "dropped", nil) // must not panic
	c := &capture{}
	SetBackend(c)
	defer SetBackend(nil)
	WarnCF("cogmem", "kept", map[string]any{"k": 1})
	if len(c.events) != 1 {
		t.Fatalf("events = %+v, want exactly one", c.events)
	}
	if got := c.events[0]; got.level != "warn" || got.component != "cogmem" || got.message != "kept" {
		t.Fatalf("event = %+v", got)
	}
}

// Every level function passes its level, component, message and the fields map
// through to the backend unchanged. InfoC has no fields and passes nil.
func TestEveryLevelPassesEverythingThrough(t *testing.T) {
	c := &capture{}
	SetBackend(c)
	defer SetBackend(nil)

	fields := map[string]any{"domain": "dK3M9P", "count": 3, "ok": true}
	DebugCF("store", "debug msg", fields)
	InfoCF("session", "info msg", fields)
	WarnCF("consolidate", "warn msg", fields)
	ErrorCF("tools", "error msg", fields)
	InfoC("composer", "info without fields")

	want := []event{
		{"debug", "store", "debug msg", fields},
		{"info", "session", "info msg", fields},
		{"warn", "consolidate", "warn msg", fields},
		{"error", "tools", "error msg", fields},
		{"info", "composer", "info without fields", nil},
	}
	if len(c.events) != len(want) {
		t.Fatalf("captured %d events, want %d: %+v", len(c.events), len(want), c.events)
	}
	for i := range want {
		got := c.events[i]
		if got.level != want[i].level || got.component != want[i].component || got.message != want[i].message {
			t.Errorf("event %d = %q/%q/%q, want %q/%q/%q", i,
				got.level, got.component, got.message,
				want[i].level, want[i].component, want[i].message)
		}
		if !reflect.DeepEqual(got.fields, want[i].fields) {
			t.Errorf("event %d fields = %v, want %v", i, got.fields, want[i].fields)
		}
	}
}

// Installing nil after a backend silences cogmem again: nothing reaches the
// old backend once it has been removed.
func TestSetBackendNilSilencesAgain(t *testing.T) {
	c := &capture{}
	SetBackend(c)
	InfoC("cogmem", "before")
	SetBackend(nil)
	ErrorCF("cogmem", "after", map[string]any{"x": 1})
	if len(c.events) != 1 || c.events[0].message != "before" {
		t.Fatalf("events after SetBackend(nil) = %+v, want only the one logged before", c.events)
	}
}
