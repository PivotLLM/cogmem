// cogmem - Cognitive Memory
// License: MIT

// Package consolidate implements the background "sleep cycle": it reads new
// archive messages, assembles the consolidation input, calls the configured
// model, validates the JSON response against the contract, and applies the
// resulting operations to the cogmem store in one transaction.
//
// This file defines the input/output contract (the JSON the model sees and
// returns) and its strict validator. The model only *proposes*; an invalid
// payload is rejected wholesale so a weak model can never corrupt memory.
package consolidate

import (
	"fmt"
	"strings"

	"github.com/PivotLLM/cogmem/store"
)

// Input is the JSON object sent to the consolidation model (as the user message).
type Input struct {
	Curated      Curated      `json:"curated"`
	CurrentState CurrentState `json:"current_state"`
	NewMessages  []Message    `json:"new_messages"`
}

// Curated is the verbatim, authoritative human layer (read-only to the model).
type Curated struct {
	AgentsMD   string `json:"AGENTS_md"`
	SoulMD     string `json:"SOUL_md"`
	IdentityMD string `json:"IDENTITY_md"`
	UserMD     string `json:"USER_md"`
	MemoryMD   string `json:"MEMORY_md"`
}

// CurrentState is the existing learned memory the model may update.
type CurrentState struct {
	Domains []DomainView `json:"domains"`
}

// DomainView is a compact projection of a domain for the model.
type DomainView struct {
	ID              string            `json:"id"`
	Sticky          bool              `json:"sticky"`
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	Version         int64             `json:"version"`
	Summary         string            `json:"summary"`
	State           store.DomainState `json:"state"`
	Triggers        string            `json:"triggers,omitempty"`
	KeywordTriggers string            `json:"keyword_triggers,omitempty"`
	Memories        []MemoryView      `json:"memories"`
}

// MemoryView is a compact projection of a hook for the model.
type MemoryView struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	// AgeDays is how long ago this memory was asserted, so the model can tell
	// which of two contradicting memories is the newer one.
	//
	// Without it, rule 3 — a newer instruction overrides an older one — was
	// unenforceable against stored memories: the view carried no time at all,
	// so a live agent asked to resolve a contradiction correctly declined to
	// guess. Days rather than a timestamp because the question is "which is
	// newer", and a small integer answers it without the model doing date
	// arithmetic it is bad at.
	//
	// Measured from creation, not last update: updating moves when a memory is
	// retyped or has a document attached, neither of which says anything about
	// when the claim was made. Consolidation supersedes rather than edits, so a
	// re-asserted memory gets a fresh creation time.
	AgeDays int `json:"age_days"`
}

// Message is one archive message in the batch.
type Message struct {
	Seq  int64  `json:"seq"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// Output is the strict JSON the model must return.
type Output struct {
	DomainOps      []DomainOp    `json:"domain_ops"`
	MemoryOps      []MemoryOp    `json:"memory_ops"`
	ConflictLedger []LedgerEntry `json:"conflict_ledger"`
}

// DomainOp is a create/update/archive operation on a domain.
type DomainOp struct {
	Op              string             `json:"op"`
	TmpID           string             `json:"tmp_id,omitempty"`
	ID              string             `json:"id,omitempty"`
	Sticky          *bool              `json:"sticky,omitempty"` // optional; sets/clears always-in-prompt
	Name            string             `json:"name,omitempty"`
	Summary         string             `json:"summary,omitempty"`
	Status          string             `json:"status,omitempty"`
	Triggers        string             `json:"triggers,omitempty"`         // comma-delimited tool-name substrings (auto-load on tool use)
	KeywordTriggers string             `json:"keyword_triggers,omitempty"` // comma-delimited message-text phrases (auto-load when mentioned)
	Reason          string             `json:"reason,omitempty"`
	State           *store.DomainState `json:"state,omitempty"`
	Evidence        store.Evidence     `json:"evidence"`
}

// MemoryOp is an add/supersede/retire operation on a hook.
type MemoryOp struct {
	Op     string `json:"op"`
	Domain string `json:"domain,omitempty"` // existing domain id or a tmp_id
	OldID  string `json:"old_id,omitempty"`
	// Type is the one classification the model states. Status is derived (a
	// consolidated memory is active) and there is no longer a source field, so
	// this is the whole of the model's judgement about what a memory is.
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	ID         string         `json:"id,omitempty"` // for retire
	Reason     string         `json:"reason,omitempty"`
	Evidence   store.Evidence `json:"evidence"`
}

// LedgerEntry records one contradiction resolution.
type LedgerEntry struct {
	Resolved string         `json:"resolved"`
	Reason   string         `json:"reason"`
	Evidence store.Evidence `json:"evidence"`
}

var validMemoryTypes = map[string]bool{
	"fact": true, "preference": true, "rule": true, "event": true, "operational": true,
}

// validDomainStatuses is the domain lifecycle, which is separate from a
// memory's: a domain is active or archived. It previously also accepted
// "review", which was a memory status that a domain could never usefully hold.
var validDomainStatuses = map[string]bool{"active": true, "archived": true}

// maxTriggersLen caps the comma-delimited tool-trigger string a domain op may set.
const maxTriggersLen = 512

// Validate enforces the contract against the input. A single violation rejects
// the whole payload (the worker then leaves the watermark unchanged and retries).
func (o Output) Validate(in Input) error {
	domainIDs := map[string]bool{}
	memoryIDs := map[string]bool{}
	for _, d := range in.CurrentState.Domains {
		domainIDs[d.ID] = true
		for _, h := range d.Memories {
			memoryIDs[h.ID] = true
		}
	}
	var minSeq, maxSeq int64
	if len(in.NewMessages) > 0 {
		minSeq = in.NewMessages[0].Seq
		maxSeq = in.NewMessages[0].Seq
		for _, m := range in.NewMessages {
			if m.Seq < minSeq {
				minSeq = m.Seq
			}
			if m.Seq > maxSeq {
				maxSeq = m.Seq
			}
		}
	}

	evOK := func(e store.Evidence) error {
		if e.SeqStart > e.SeqEnd {
			return fmt.Errorf("evidence seq_start %d > seq_end %d", e.SeqStart, e.SeqEnd)
		}
		if len(in.NewMessages) == 0 || e.SeqStart < minSeq || e.SeqEnd > maxSeq {
			return fmt.Errorf("evidence [%d,%d] outside batch [%d,%d]", e.SeqStart, e.SeqEnd, minSeq, maxSeq)
		}
		return nil
	}

	// Collect tmp ids created this payload (hooks may reference them).
	tmpIDs := map[string]bool{}
	for i, op := range o.DomainOps {
		if err := evOK(op.Evidence); err != nil {
			return fmt.Errorf("domain_ops[%d]: %w", i, err)
		}
		if len(op.Triggers) > maxTriggersLen {
			return fmt.Errorf("domain_ops[%d]: triggers too long (%d > %d)", i, len(op.Triggers), maxTriggersLen)
		}
		if len(op.KeywordTriggers) > maxTriggersLen {
			return fmt.Errorf("domain_ops[%d]: keyword_triggers too long (%d > %d)", i, len(op.KeywordTriggers), maxTriggersLen)
		}
		switch op.Op {
		case "create":
			if op.TmpID == "" || tmpIDs[op.TmpID] {
				return fmt.Errorf("domain_ops[%d]: create needs a unique tmp_id", i)
			}
			if strings.TrimSpace(op.Name) == "" {
				return fmt.Errorf("domain_ops[%d]: create needs a name", i)
			}
			if op.Status != "" && !validDomainStatuses[op.Status] {
				return fmt.Errorf("domain_ops[%d]: invalid status %q", i, op.Status)
			}
			tmpIDs[op.TmpID] = true
		case "update":
			if !domainIDs[op.ID] {
				return fmt.Errorf("domain_ops[%d]: update unknown domain %q", i, op.ID)
			}
		case "archive":
			if !domainIDs[op.ID] {
				return fmt.Errorf("domain_ops[%d]: archive unknown domain %q", i, op.ID)
			}
		default:
			return fmt.Errorf("domain_ops[%d]: invalid op %q", i, op.Op)
		}
	}

	for i, op := range o.MemoryOps {
		// Evidence is required for anything that WRITES text, because that is
		// what the rule is for: no asserted memory without a message justifying
		// it. A retire asserts nothing — it removes a memory that already
		// exists, and its id must already be known — so it may carry no
		// evidence at all.
		//
		// This is what lets the model tidy: merging two memories that say the
		// same thing, or dropping one a newer memory contradicts, is housekeeping
		// the current conversation did not raise and so cannot cite. Requiring
		// evidence there would have made every such op invalid, and one invalid
		// op rejects the whole payload — so the model would have aborted entire
		// runs trying to follow the rule.
		if op.Op != "retire" || !op.Evidence.IsZero() {
			if err := evOK(op.Evidence); err != nil {
				return fmt.Errorf("memory_ops[%d]: %w", i, err)
			}
		}
		switch op.Op {
		case "add", "supersede":
			if !domainIDs[op.Domain] && !tmpIDs[op.Domain] {
				return fmt.Errorf("memory_ops[%d]: unknown domain %q", i, op.Domain)
			}
			// Required, not merely valid-if-present. The previous contract
			// checked each field only when it was non-empty, so an op that
			// omitted everything passed every guard and was then filled in with
			// defaults the rules forbade.
			if op.Type == "" {
				return fmt.Errorf("memory_ops[%d]: missing type", i)
			}
			if !validMemoryTypes[op.Type] {
				return fmt.Errorf("memory_ops[%d]: invalid type %q", i, op.Type)
			}
			if strings.TrimSpace(op.Text) == "" {
				return fmt.Errorf("memory_ops[%d]: empty text", i)
			}
			if op.Op == "supersede" && !memoryIDs[op.OldID] {
				return fmt.Errorf("memory_ops[%d]: supersede unknown old_id %q", i, op.OldID)
			}
		case "retire":
			if !memoryIDs[op.ID] {
				return fmt.Errorf("memory_ops[%d]: retire unknown memory %q", i, op.ID)
			}
		default:
			return fmt.Errorf("memory_ops[%d]: invalid op %q", i, op.Op)
		}
	}

	for i, e := range o.ConflictLedger {
		// Optional for the same reason as a retire: a ledger entry RECORDS a
		// decision, it does not assert a memory. When the decision is
		// housekeeping — two stored memories contradict each other, and the
		// stale one goes — there is no message in this batch to cite, because
		// the conversation never raised it.
		//
		// Found the hard way: rule 9 asked the model to tidy, it did, it filed
		// the resolution in the ledger as instructed, and the whole payload was
		// rejected for evidence [0,0]. One invalid entry aborts the run, so the
		// agent lost the entire consolidation rather than the one entry.
		if e.Evidence.IsZero() {
			continue
		}
		if err := evOK(e.Evidence); err != nil {
			return fmt.Errorf("conflict_ledger[%d]: %w", i, err)
		}
	}
	return nil
}

// Normalize repairs safe, unambiguous contract deviations in place, so a single
// mechanically-fixable mistake doesn't make Validate reject an otherwise-good
// batch (which would silently drop real memories). It returns a human-readable
// note per repair, for the run record/log.
//
// There are currently no repairs. The one that existed downgraded an inferred
// memory the model had marked active to review, and both the status field and
// the review state are gone — the model no longer states anything that can be
// wrong in a way an automatic correction could fix.
//
// The function is kept because the repair-and-note path is the right shape for
// the next contract deviation that turns out to be safely correctable, and its
// callers (the worker's run record, the memory page's note field) already
// handle an empty result.
//
// Genuinely ambiguous violations (unknown domain, missing or invalid type, empty
// text, dangling supersede/retire references) are NOT repaired — Validate
// rejects those, since there is no safe automatic correction.
func (o *Output) Normalize() []string {
	return nil
}
