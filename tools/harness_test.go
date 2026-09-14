// cogmem - Cognitive Memory
// License: MIT

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PivotLLM/cogmem/store"
	"github.com/PivotLLM/toolspec"
)

// harness is a set of handlers bound to a temp workspace, with the memory
// directory exposed so a test can open the same store the handlers use and
// assert on what they wrote.
type harness struct {
	h   map[string]toolspec.ToolHandler
	ws  string
	dir string
}

// newHarness builds the handlers for a fresh workspace. mutate, if given,
// adjusts the Host before the definitions are built (to install an attachment
// checker, drop the workspace, and so on).
func newHarness(t *testing.T, mutate func(*Host)) harness {
	t.Helper()
	ws := t.TempDir()
	host := Host{Dir: filepath.Join(ws, "cogmem"), Workspace: ws}
	if mutate != nil {
		mutate(&host)
	}
	defs := Definitions(host)
	m := make(map[string]toolspec.ToolHandler, len(defs))
	for _, d := range defs {
		m[d.Name] = d.Handler
	}
	return harness{h: m, ws: ws, dir: host.Dir}
}

// store opens the handlers' store directly. The directory is created if no
// handler has run yet.
func (hs harness) store(t *testing.T) *store.Store {
	t.Helper()
	if err := os.MkdirAll(hs.dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := store.Open(store.DBPath(hs.dir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// call runs a tool and returns its result.
func (hs harness) call(t *testing.T, tool string, args map[string]any) *toolspec.Result {
	t.Helper()
	h, ok := hs.h[tool]
	if !ok {
		t.Fatalf("no tool %q", tool)
	}
	return run(t, h, newCall(testSession, args))
}

// ok runs a tool that must succeed and returns its text.
func (hs harness) ok(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	res := hs.call(t, tool, args)
	if res.IsError {
		t.Fatalf("%s(%v) failed: %s", tool, args, res.ForLLM)
	}
	return res.ForLLM
}

// fail runs a tool that must fail and asserts the exact error text, both in
// the model-facing result and the wrapped error.
func (hs harness) fail(t *testing.T, tool string, args map[string]any, want string) {
	t.Helper()
	res := hs.call(t, tool, args)
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want error %q: %s", tool, args, want, res.ForLLM)
	}
	if res.ForLLM != want {
		t.Errorf("%s(%v) error = %q, want %q", tool, args, res.ForLLM, want)
	}
	if res.Err == nil || res.Err.Error() != want {
		t.Errorf("%s(%v) Err = %v, want %q", tool, args, res.Err, want)
	}
}

// createDomain creates a domain through the tool and returns its id.
func (hs harness) createDomain(t *testing.T, args map[string]any) string {
	t.Helper()
	return extractID(t, hs.ok(t, "domain_create", args), "d")
}

// createMemory creates a memory through the tool and returns its id.
func (hs harness) createMemory(t *testing.T, args map[string]any) string {
	t.Helper()
	return extractID(t, hs.ok(t, "memory_create", args), "h")
}

var ctx = context.Background()
