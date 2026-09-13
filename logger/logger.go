// cogmem - Cognitive Memory
// License: MIT

// Package logger is cogmem's host-injectable logging seam. cogmem pulls in no
// host logging stack; a host installs a Backend via SetBackend so cogmem's
// structured events flow into the host's own logger with its formatting and
// routing. The zero state is a no-op: cogmem is silent until a backend is
// installed. The CF function names mirror the host logger API the code was
// written against, so call sites read the same in both places.
package logger

import "sync/atomic"

// Backend receives cogmem's structured log events.
type Backend interface {
	// Log records one event at the given level ("debug", "info", "warn",
	// "error"), tagged with a component and structured fields.
	Log(level, component, message string, fields map[string]any)
}

var backend atomic.Pointer[Backend]

// SetBackend installs the host's backend. Passing nil silences cogmem again.
func SetBackend(b Backend) {
	if b == nil {
		backend.Store(nil)
		return
	}
	backend.Store(&b)
}

func emit(level, component, message string, fields map[string]any) {
	if b := backend.Load(); b != nil {
		(*b).Log(level, component, message, fields)
	}
}

// DebugCF logs a debug event with a component and fields.
func DebugCF(component, message string, fields map[string]any) {
	emit("debug", component, message, fields)
}

// InfoCF logs an info event with a component and fields.
func InfoCF(component, message string, fields map[string]any) {
	emit("info", component, message, fields)
}

// WarnCF logs a warning event with a component and fields.
func WarnCF(component, message string, fields map[string]any) {
	emit("warn", component, message, fields)
}

// ErrorCF logs an error event with a component and fields.
func ErrorCF(component, message string, fields map[string]any) {
	emit("error", component, message, fields)
}

// InfoC logs an info event with a component and no fields.
func InfoC(component, message string) { emit("info", component, message, nil) }
