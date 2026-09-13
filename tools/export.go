// cogmem - Cognitive Memory
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package tools

import (
	"context"

	"github.com/PivotLLM/cogmem/portable"
	"github.com/PivotLLM/cogmem/store"
)

// renderFullExport renders the agent's entire cognitive memory as the portable
// YAML document, and returns it with the domain and memory counts for the
// tool's summary line.
//
// This used to render Markdown, which read well and could not be read back —
// so it was only ever a thing to look at. The YAML document is the same format
// the WebUI exports and imports, which makes the tool a backup rather than a
// report.
func renderFullExport(ctx context.Context, s *store.Store) (doc string, nDomains, nMemories int, err error) {
	d, err := portable.Export(ctx, s)
	if err != nil {
		return "", 0, 0, err
	}
	out, err := portable.Marshal(d)
	if err != nil {
		return "", 0, 0, err
	}
	for _, dom := range d.Domains {
		nMemories += len(dom.Memories)
	}
	return string(out), len(d.Domains), nMemories, nil
}

// exportFilename is where the export tool writes, under the agent's files/ dir.
const exportFilename = "MEMORY_EXPORT.yaml"
