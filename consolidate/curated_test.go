// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCurated(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("agents body"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("user body"), 0o644); err != nil {
		t.Fatalf("write USER.md: %v", err)
	}
	// SOUL.md, IDENTITY.md, MEMORY.md intentionally missing.

	c := ReadCurated(ws)
	if c.AgentsMD != "agents body" {
		t.Fatalf("AgentsMD = %q", c.AgentsMD)
	}
	if c.UserMD != "user body" {
		t.Fatalf("UserMD = %q", c.UserMD)
	}
	if c.SoulMD != "" || c.IdentityMD != "" || c.MemoryMD != "" {
		t.Fatalf("missing files should be empty: %+v", c)
	}
}

func TestReadCuratedMissingWorkspace(t *testing.T) {
	c := ReadCurated("/nonexistent-workspace-xyz")
	if c.AgentsMD != "" || c.UserMD != "" {
		t.Fatalf("missing workspace should yield empty Curated: %+v", c)
	}
}
