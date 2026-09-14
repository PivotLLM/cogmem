// cogmem - Cognitive Memory
// License: MIT

// Package store is the per-session persistence layer for cognitive memory.
// It owns the .cogmem.db SQLite database (modernc.org/sqlite, pure Go, no CGO)
// and holds the storage types shared by the composer, the consolidation worker,
// and the cogmem MCP tools. It is the leaf package: nothing in cogmem imports
// its importers, so there are no cycles.
package store

import "time"

// Status is the lifecycle state of a domain or memory.
type Status string

const (
	StatusActive   Status = "active"   // used in prompts
	StatusArchived Status = "archived" // domains
	StatusRetired  Status = "retired"  // memories
)

// MemoryType classifies a memory, and is the only classification the model is
// asked for. Four of the five are standing knowledge and load into the prompt;
// TypeEvent is the exception and never does. Volatile project status lives on
// the domain's state, not here.
type MemoryType string

const (
	// TypeFact is something true: "Eric lives in Ottawa."
	TypeFact MemoryType = "fact"
	// TypePreference is how the user likes things done.
	TypePreference MemoryType = "preference"
	// TypeRule is a hard directive governing output or behaviour toward the
	// user: "Do not use the word thuddy."
	TypeRule MemoryType = "rule"
	// TypeEvent is something observed at a point in time — a trip, a delivery
	// status, a scheduled run. It goes stale immediately and accumulates without
	// bound, so it is NEVER loaded into the prompt: domains report how many they
	// hold and it is reached through search. This is the one type that changes
	// behaviour rather than merely describing.
	TypeEvent MemoryType = "event"
	// TypeOperational is the assistant's own housekeeping: where it files
	// things, how it works, rules it sets for its own method. The discriminator
	// against TypeRule is who it serves — a rule governs behaviour toward the
	// user, operational is the assistant's own bookkeeping.
	TypeOperational MemoryType = "operational"
)

// ValidMemoryTypes reports whether t is one of the five known types.
func ValidMemoryTypes(t MemoryType) bool {
	switch t {
	case TypeFact, TypePreference, TypeRule, TypeEvent, TypeOperational:
		return true
	default:
		return false
	}
}

// Prompt reports whether a memory of this type is loaded into the prompt as
// standing knowledge. Everything except TypeEvent is.
func (t MemoryType) Prompt() bool { return t != TypeEvent }

// Origin records which actor created a memory. Surfaced to the user and into
// the prompt so the assistant knows where a memory came from.
type Origin string

const (
	OriginChat          Origin = "chat"          // the agent wrote it during a conversation (cogmem tools)
	OriginConsolidation Origin = "consolidation" // the background sleep-cycle worker wrote it
	OriginUser          Origin = "user"          // a human created it directly (WebUI / import)
)

// normalizeOrigin returns a valid Origin, defaulting unknown/empty to OriginChat.
func normalizeOrigin(o Origin) Origin {
	switch o {
	case OriginChat, OriginConsolidation, OriginUser:
		return o
	default:
		return OriginChat
	}
}

// DomainState is the structured JSON payload stored in domains.state_json.
type DomainState struct {
	Blockers    []string       `json:"blockers,omitempty"`
	NextActions []string       `json:"next_actions,omitempty"`
	Constraints []string       `json:"constraints,omitempty"`
	Fields      map[string]any `json:"fields,omitempty"`
}

// Domain is one coherent body of learned knowledge in a session.
type Domain struct {
	ID string
	// StickyPriority is stored in the legacy "type" column (TEXT, parsed as int):
	// 0 / non-numeric = not sticky; > 0 = sticky (injected into every prompt). The
	// magnitude is reserved as a future sort key. See Sticky.
	StickyPriority  int
	Name            string
	Status          Status
	Version         int64
	Summary         string
	State           DomainState
	SchemaName      string
	SchemaVersion   int
	LastActiveAt    int64  // unix seconds; recency/last-used signal (0 = never recorded)
	Triggers        string // comma-delimited tool-name substrings; activates this domain when a matching tool is used (see TriggerTokens)
	KeywordTriggers string // comma-delimited phrases; activates this domain when one appears (whole-phrase, word-boundary) in the message text (see KeywordPhrases)
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ArchivedAt      *time.Time
	Memories        []Memory // populated by GetDomain / ListMemories; nil otherwise
}

// Sticky reports whether this domain is injected into every prompt.
func (d Domain) Sticky() bool { return d.StickyPriority > 0 }

// LastActive returns when the domain was last active (created, written, loaded,
// or read) and false if it was never recorded.
func (d Domain) LastActive() (time.Time, bool) {
	if d.LastActiveAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(d.LastActiveAt, 0), true
}

// Memory is a short, addressable unit of learned memory inside a domain.
type Memory struct {
	ID                 string
	DomainID           string
	Type               MemoryType
	Text               string
	Status             Status
	Confidence         float64
	Origin             Origin
	SourceSession      *string
	SourceSeqStart     *int64
	SourceSeqEnd       *int64
	SupersedesMemoryID *string
	RetireReason       *string
	// FileRef optionally points at a markdown file whose full contents are
	// attached to the prompt whenever this memory is rendered. It is an
	// agent-relative path (e.g. "files/voice.md", "maestro/style.md"); the store
	// never touches the filesystem — resolution and the permission check belong
	// to the caller (see cogmem.AttachmentLoader).
	FileRef   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Evidence references the archive seq range that justifies a memory change.
type Evidence struct {
	SeqStart int64 `json:"seq_start"`
	SeqEnd   int64 `json:"seq_end"`
}

// IsZero reports whether no evidence was given. A memory op that only removes
// something — a retire — is allowed to carry none, because the evidence rule
// exists to stop the model asserting content no message supports, and a retire
// asserts nothing.
func (e Evidence) IsZero() bool { return e.SeqStart == 0 && e.SeqEnd == 0 }

// Event is one row of the append-only audit ledger.
type Event struct {
	ID         string
	Type       string // create, update, archive, retire, merge, reject, conflict_resolved, gap
	DomainID   string
	MemoryID   string
	OldJSON    string
	NewJSON    string
	Reason     string
	Evidence   string // marshaled evidence/json
	Actor      string // sleep_cycle, mcp_tool, migration, operator
	Model      string
	PromptHash string
}

// Run is one consolidation-run debug record (for comparing models).
type Run struct {
	ID           string
	Trigger      string // message, idle, nightly, manual
	Model        string
	SeqStart     int64
	SeqEnd       int64
	InputTokens  int
	OutputTokens int
	Status       string // ok, invalid_json, aborted, error
	OpsApplied   int
	// Error is why the run FAILED. Leave it empty on a successful run.
	Error string
	// Note is something worth telling the operator about a run that
	// SUCCEEDED — e.g. a contract deviation that was safely auto-repaired.
	// Kept separate from Error so the UI can show it without dressing a
	// success up as a failure, which is exactly what happened when the two
	// shared one column.
	Note       string
	PromptHash string
	StartedAt  time.Time
	FinishedAt *time.Time
}
