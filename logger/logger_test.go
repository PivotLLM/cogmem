// cogmem - Cognitive Memory
// License: MIT

package logger

import "testing"

type capture struct {
	events []string
}

func (c *capture) Log(level, component, message string, fields map[string]any) {
	c.events = append(c.events, level+":"+component+":"+message)
}

func TestSilentUntilBackendInstalled(t *testing.T) {
	SetBackend(nil)
	InfoCF("cogmem", "dropped", nil) // must not panic
	c := &capture{}
	SetBackend(c)
	defer SetBackend(nil)
	WarnCF("cogmem", "kept", map[string]any{"k": 1})
	if len(c.events) != 1 || c.events[0] != "warn:cogmem:kept" {
		t.Fatalf("events = %v", c.events)
	}
}
