// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"path/filepath"
	"time"

	"github.com/PivotLLM/cogmem/consolidate"
)

// Settings is everything a host configures about cognitive memory. A zero
// value means "the documented default" for every field; hosts map their own
// config onto it and never touch the option types directly.
type Settings struct {
	Prompt        PromptSettings
	Consolidation ConsolidationSettings
	Retention     RetentionSettings
}

// PromptSettings tunes per-turn prompt composition.
type PromptSettings struct {
	TopKDomains       int     // routed domains per turn; 0 = default
	MaxChars          int     // routed block budget; 0 = default
	MinConfidence     float64 // memories below this are not rendered; 0 = default
	IncludeDebugTrace bool    // append the routing trace to the routed block
	FileMaxBytes      int     // per attached document; 0 = default
	FileTotalMaxBytes int     // all attachments in one turn; 0 = default
}

// ConsolidationSettings tunes the background sleep cycle.
type ConsolidationSettings struct {
	EveryNMessages   int    // run after this many meaningful messages; <=0 disables
	IdleMinutes      int    // run after this long idle; <=0 disables
	Nightly          bool   // run once a day
	NightlyAt        string // "HH:MM" local; "" = 03:00
	ProposeDomains   bool
	AutoPromote      bool
	DebugDump        bool // write each run's payloads under <workspace>/cogmem-dumps
	MaxBatchMessages int
	MaxInputTokens   int
	PerMessageChars  int
}

// RetentionSettings bounds how long transient rows are kept, in days. These
// are RESOLVED values: 0 means keep forever. A host that wants "unset means
// 30 days" resolves that before building Settings.
type RetentionSettings struct {
	EventDays   int
	RetiredDays int
}

// ComposerOptions translates the prompt settings into composer options.
func (s Settings) ComposerOptions() []Option {
	var opts []Option
	if s.Prompt.TopKDomains > 0 {
		opts = append(opts, WithTopKDomains(s.Prompt.TopKDomains))
	}
	if s.Prompt.MaxChars > 0 {
		opts = append(opts, WithMaxChars(s.Prompt.MaxChars))
	}
	if s.Prompt.MinConfidence > 0 {
		opts = append(opts, WithMinConfidence(s.Prompt.MinConfidence))
	}
	if s.Prompt.FileMaxBytes > 0 {
		opts = append(opts, WithFileMaxBytes(s.Prompt.FileMaxBytes))
	}
	if s.Prompt.FileTotalMaxBytes > 0 {
		opts = append(opts, WithFileTotalMaxBytes(s.Prompt.FileTotalMaxBytes))
	}
	return opts
}

// WorkerOptions translates the consolidation and retention settings into
// worker options for a session under workspace.
func (s Settings) WorkerOptions(workspace string) []consolidate.Option {
	bo := consolidate.DefaultBatchOptions()
	if s.Consolidation.MaxBatchMessages > 0 {
		bo.MaxMessages = s.Consolidation.MaxBatchMessages
	}
	if s.Consolidation.MaxInputTokens > 0 {
		bo.MaxInputTokens = s.Consolidation.MaxInputTokens
	}
	if s.Consolidation.PerMessageChars > 0 {
		bo.PerMessageChars = s.Consolidation.PerMessageChars
	}
	opts := []consolidate.Option{
		consolidate.WithBatchOptions(bo),
		consolidate.WithProposeDomains(s.Consolidation.ProposeDomains),
		consolidate.WithAutoPromote(s.Consolidation.AutoPromote),
		consolidate.WithRetention(s.Retention.EventDays, s.Retention.RetiredDays),
	}
	if s.Consolidation.DebugDump && workspace != "" {
		opts = append(opts, consolidate.WithDebugDump(filepath.Join(workspace, "cogmem-dumps")))
	}
	return opts
}

// ManagerOptions translates the consolidation triggers into manager options.
func (s Settings) ManagerOptions() []consolidate.ManagerOption {
	var opts []consolidate.ManagerOption
	if s.Consolidation.EveryNMessages > 0 {
		opts = append(opts, consolidate.WithEveryNMessages(s.Consolidation.EveryNMessages))
	}
	if s.Consolidation.IdleMinutes > 0 {
		opts = append(opts, consolidate.WithIdle(time.Duration(s.Consolidation.IdleMinutes)*time.Minute))
	}
	if s.Consolidation.Nightly {
		at := s.Consolidation.NightlyAt
		if at == "" {
			at = "03:00"
		}
		opts = append(opts, consolidate.WithNightlyAt(at), consolidate.WithNightlyJitter(15*time.Minute))
	}
	return opts
}
