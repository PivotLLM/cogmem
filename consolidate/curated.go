// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"os"
	"path/filepath"
)

// curatedFiles maps each Curated field to its workspace-relative path. All five
// curated files live at the workspace root. Missing files are tolerated (empty
// string), so the human layer is always best-effort.
var curatedFiles = []struct {
	relPath string
	set     func(*Curated, string)
}{
	{"AGENTS.md", func(c *Curated, v string) { c.AgentsMD = v }},
	{"SOUL.md", func(c *Curated, v string) { c.SoulMD = v }},
	{"IDENTITY.md", func(c *Curated, v string) { c.IdentityMD = v }},
	{"USER.md", func(c *Curated, v string) { c.UserMD = v }},
	{"MEMORY.md", func(c *Curated, v string) { c.MemoryMD = v }},
}

// ReadCurated reads the verbatim human-authored layer from a workspace. Every
// file is optional: a missing or unreadable file yields an empty string.
func ReadCurated(workspace string) Curated {
	var c Curated
	for _, f := range curatedFiles {
		if b, err := os.ReadFile(filepath.Join(workspace, f.relPath)); err == nil {
			f.set(&c, string(b))
		}
	}
	return c
}
