// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// defaultPrompt is the consolidation system prompt: the assistant's role, the
// core rules, and the output schema. It is embedded and NOT overridable.
//
// It used to be replaceable wholesale by a per-agent COGMEM.md, which meant the
// machine contract — the output schema every response is validated against —
// was operator-editable and, worse, frozen at whatever version an agent's
// workspace happened to be seeded with. A contract change then reached no
// existing agent: their copy still described the old shape. That survived by
// luck rather than design, because the old shape still parsed. A required field
// the old prompt did not emit would have failed validation on every operation,
// and one invalid operation rejects the whole payload — silent, total
// consolidation failure across every agent, showing up only in run records.
//
// So the contract is fixed and per-agent files now ADD to it. See BuildPrompt.
//
//go:embed default_prompt.md
var defaultPrompt string

// DefaultPrompt returns the embedded consolidation prompt.
func DefaultPrompt() string { return defaultPrompt }

// PromptPath returns the per-agent instruction file path for a workspace.
func PromptPath(workspace string) string { return filepath.Join(workspace, PromptFilename) }

// PromptResult reports what BuildPrompt did with an agent's instruction file.
type PromptResult struct {
	// Appended is true when per-agent instructions were added.
	Appended bool
	// Ignored is true when a file was present but not used. Reason says why.
	Ignored bool
	Reason  string
}

// stripHTMLComments removes <!-- ... --> blocks.
//
// The seeded file explains to the OPERATOR what it is for, and that explanation
// must not reach the model: an unedited file would otherwise add several
// hundred characters of "write your instructions below" to every consolidation
// prompt — instructions addressed to the wrong reader, and noise in a prompt
// whose whole point is precision. Commenting the guidance keeps the file
// self-describing while leaving it genuinely inert until someone writes in it.
func stripHTMLComments(s string) string {
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return s[:i] // unterminated: drop the rest rather than feed it through
		}
		s = s[:i] + s[i+j+len("-->"):]
	}
}

// enginePromptMarkers appear only in a copy of the engine prompt itself, never
// in instructions an operator would write for one assistant.
var enginePromptMarkers = []string{"# OUTPUT SCHEMA", "# CORE RULES"}

// BuildPrompt returns the consolidation prompt for one agent: the embedded
// contract, plus that agent's instructions appended when it has any.
//
// A file that is a copy of the engine prompt is IGNORED rather than appended.
// Every workspace was previously seeded with exactly that, so appending one
// would give the model the whole old prompt — including a second, contradictory
// output schema — bolted onto the current one. That is worse than the wholesale
// override it replaces, so such a file is skipped and the caller told why.
func BuildPrompt(path string) (string, PromptResult) {
	if path == "" {
		return defaultPrompt, PromptResult{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultPrompt, PromptResult{}
	}
	extra := strings.TrimSpace(stripHTMLComments(string(b)))
	if extra == "" {
		return defaultPrompt, PromptResult{}
	}
	for _, m := range enginePromptMarkers {
		if strings.Contains(extra, m) {
			return defaultPrompt, PromptResult{
				Ignored: true,
				Reason: "the file is a copy of the built-in consolidation prompt, which is no " +
					"longer overridable; reduce it to instructions specific to this assistant, " +
					"or delete it",
			}
		}
	}
	var b2 strings.Builder
	b2.WriteString(defaultPrompt)
	b2.WriteString("\n\n# AGENT-SPECIFIC INSTRUCTIONS\n\n")
	b2.WriteString("The operator wrote the following for THIS assistant. Apply it when deciding\n")
	b2.WriteString("what is worth remembering and how to describe it. It refines the rules above;\n")
	b2.WriteString("it cannot change the output schema, and where it conflicts with the contract\n")
	b2.WriteString("the contract wins.\n\n")
	b2.WriteString(extra)
	b2.WriteString("\n")
	return b2.String(), PromptResult{Appended: true}
}
