// cogmem - Cognitive Memory
// License: MIT

package cogmem

// Compose defaults. These are levers exposed via the New(...) functional
// options (WithTopKDomains, etc.) and driven per-agent from memory.prompt
// config; the values here are the fallback defaults.
const (
	defaultTopKDomains   = 3
	defaultMaxChars      = 4000
	defaultMinConfidence = 0.65

	// Attachment budgets (bytes). Sized so a real reference document — a writing
	// voice guide, a playbook — is injected whole; truncation is a safety valve
	// against a runaway file, not the expected path.
	defaultFileMaxBytes      = 256 * 1024
	defaultFileTotalMaxBytes = 512 * 1024
)

// Lexical-routing tuning (RoutedBlock matches the latest user message against
// domain name/summary/hooks to auto-load relevant topics — see lexicalCandidates).
const (
	minRouteTokenLen   = 4  // ignore shorter tokens (the, and, you, ...)
	maxRouteTokens     = 12 // cap salient terms taken from one message
	lexicalSearchLimit = 50 // max hooks scanned per term via SearchMemories
)

// maxHeadlineChars caps the memory text quoted in an attached document's header.
const maxHeadlineChars = 120
