// cogmem - Cognitive Memory
// License: MIT

package consolidate

// PromptFilename holds per-agent memory instructions. It is seeded into each
// agent workspace by internal/workspace.Populate (write-if-missing) and is
// operator-editable. Its contents are APPENDED to the embedded consolidation
// prompt, which owns the rules and the output schema and is not overridable —
// see BuildPrompt.
const PromptFilename = "COGMEM.md"

// Batching defaults. These are levers, surfaced per-agent via config and the
// worker's functional options; the values here are the fallback defaults.
const (
	defaultMaxMessages     = 200
	defaultMaxInputTokens  = 96000
	defaultPerMessageChars = 12000
	defaultMaxOutputTokens = 8000

	// approxCharsPerToken is the tokenizer-free estimate (~4 chars/token).
	approxCharsPerToken = 4
)

// Actor labels recorded in the audit ledger.
const (
	actorSleepCycle = "sleep_cycle"
)
