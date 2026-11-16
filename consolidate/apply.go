// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/PivotLLM/cogmem/store"
)

// ApplyContext carries the metadata recorded on every applied operation.
type ApplyContext struct {
	Actor      string // sleep_cycle, mcp_tool, ...
	Model      string
	PromptHash string
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	// Applied is the number of operations applied; 0 on error, since the
	// transaction rolled back.
	Applied int
	// DroppedKeywords are proposed keyword phrases left out of a domain's
	// list because they are too generic. Dropping them does not fail the run.
	DroppedKeywords []DroppedKeyword
}

// DroppedKeyword is one proposed keyword phrase Apply did not store.
type DroppedKeyword struct {
	DomainID   string
	DomainName string // the domain's name, for messages an operator reads
	Phrase     string
	Reason     string
}

// Apply writes a *validated* Output to the store in a single transaction:
// assigning ids to created domains, mapping tmp_ids referenced by hooks,
// applying every op, and appending an audit event per op. Call Output.Validate
// before Apply.
//
// A domain op's keyword_triggers sets consolidation's own keyword list only.
// Phrases already in the agent's list or blocked by the agent are left out, and
// generic ones (see store.CheckKeywords) are dropped and reported; neither the
// agent's list nor the blocked list is ever changed here.
func Apply(ctx context.Context, st *store.Store, out Output, ac ApplyContext) (ApplyResult, error) {
	var res ApplyResult
	err := st.WithTx(ctx, func(tx *sql.Tx) error {
		tmp := map[string]string{} // tmp_id -> assigned domain id

		for _, op := range out.DomainOps {
			switch op.Op {
			case "create":
				auto, dropped, err := autoKeywords(ctx, st, tx, store.Domain{}, op.KeywordTriggers)
				if err != nil {
					return err
				}
				d, err := st.CreateDomain(ctx, tx, store.CreateDomainParams{
					Sticky:       op.Sticky != nil && *op.Sticky,
					Name:         op.Name,
					Status:       store.Status(orDefault(op.Status, "active")),
					Summary:      op.Summary,
					Triggers:     op.Triggers,
					AutoKeywords: auto,
				})
				if err != nil {
					return err
				}
				res.DroppedKeywords = appendDropped(res.DroppedKeywords, d.ID, d.Name, dropped)
				tmp[op.TmpID] = d.ID
				if err := logOp(ctx, st, tx, ac, "create", d.ID, "", op.Reason, op.Evidence); err != nil {
					return err
				}
			case "update":
				p := store.UpdateDomainParams{}
				if op.Summary != "" {
					s := op.Summary
					p.Summary = &s
				}
				if op.State != nil {
					p.State = op.State
				}
				if op.Status != "" {
					s := store.Status(op.Status)
					p.Status = &s
				}
				if op.Triggers != "" {
					t := op.Triggers
					p.Triggers = &t
				}
				if op.KeywordTriggers != "" {
					cur, err := st.GetDomain(ctx, tx, op.ID, false)
					if err != nil {
						return err
					}
					auto, dropped, err := autoKeywords(ctx, st, tx, cur, op.KeywordTriggers)
					if err != nil {
						return err
					}
					// Nothing survived the filters: keep the stored list rather
					// than empty it, which the model never asked for.
					if auto != "" {
						p.AutoKeywords = &auto
					}
					res.DroppedKeywords = appendDropped(res.DroppedKeywords, op.ID, cur.Name, dropped)
				}
				if op.Sticky != nil {
					p.Sticky = op.Sticky
				}
				// An update left with nothing to change (every proposed keyword
				// filtered out) writes nothing, so it does not bump the version.
				if p != (store.UpdateDomainParams{}) {
					if err := st.UpdateDomain(ctx, tx, op.ID, p); err != nil {
						return err
					}
				}
				if err := logOp(ctx, st, tx, ac, "update", op.ID, "", op.Reason, op.Evidence); err != nil {
					return err
				}
			case "archive":
				if err := st.ArchiveDomain(ctx, tx, op.ID); err != nil {
					return err
				}
				if err := logOp(ctx, st, tx, ac, "archive", op.ID, "", op.Reason, op.Evidence); err != nil {
					return err
				}
			}
			res.Applied++
		}

		for _, op := range out.MemoryOps {
			domainID := op.Domain
			if mapped, ok := tmp[op.Domain]; ok {
				domainID = mapped
			}
			switch op.Op {
			case "add":
				h, err := st.AddMemory(ctx, tx, memoryParams(domainID, op))
				if err != nil {
					return err
				}
				if err := logOp(ctx, st, tx, ac, "create", domainID, h.ID, op.Reason, op.Evidence); err != nil {
					return err
				}
			case "supersede":
				h, err := st.SupersedeMemory(ctx, tx, op.OldID, memoryParams(domainID, op))
				if err != nil {
					return err
				}
				if err := logOp(ctx, st, tx, ac, "merge", domainID, h.ID, op.Reason, op.Evidence); err != nil {
					return err
				}
			case "retire":
				if err := st.RetireMemory(ctx, tx, op.ID, op.Reason); err != nil {
					return err
				}
				if err := logOp(ctx, st, tx, ac, "retire", "", op.ID, op.Reason, op.Evidence); err != nil {
					return err
				}
			}
			res.Applied++
		}

		for _, e := range out.ConflictLedger {
			if err := st.LogEvent(ctx, tx, store.Event{
				Type: "conflict_resolved", Reason: e.Resolved + " — " + e.Reason,
				Evidence: evidenceJSON(e.Evidence), Actor: ac.Actor,
				Model: ac.Model, PromptHash: ac.PromptHash,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// autoKeywords is the consolidation-owned keyword list to store for domain cur
// (zero for a domain being created) given the model's proposal: the proposed
// phrases minus those already in the agent's list, those the agent blocked,
// and generic ones, which are returned so the run can report them. cur must be
// read inside the apply transaction. Phrases cur already routes on are exempt
// from the shared-use rule (see store.CheckKeywords).
func autoKeywords(ctx context.Context, st *store.Store, tx *sql.Tx, cur store.Domain, proposed string) (string, []store.KeywordRejection, error) {
	accepted, rejected, err := st.CheckKeywords(ctx, tx, cur.ID, []string{proposed})
	if err != nil {
		return "", nil, err
	}
	excluded := map[string]bool{}
	for _, p := range cur.AgentKeywordPhrases() {
		excluded[p] = true
	}
	for _, p := range cur.BlockedKeywordPhrases() {
		excluded[p] = true
	}
	var keep []string
	for _, p := range accepted {
		if !excluded[p] {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ","), rejected, nil
}

func appendDropped(out []DroppedKeyword, domainID, domainName string, rejected []store.KeywordRejection) []DroppedKeyword {
	for _, r := range rejected {
		out = append(out, DroppedKeyword{DomainID: domainID, DomainName: domainName, Phrase: r.Phrase, Reason: r.Reason})
	}
	return out
}

// memoryParams builds the store write for one model operation.
//
// Status is not taken from the model: a consolidated memory is active, and the
// only other status (retired) is a lifecycle transition the model reaches
// through a retire op rather than by asking for it at creation. Type is the one
// classification the model states, and Validate has already checked it.
func memoryParams(domainID string, op MemoryOp) store.AddMemoryParams {
	return store.AddMemoryParams{
		DomainID:       domainID,
		Type:           store.MemoryType(op.Type),
		Text:           op.Text,
		Status:         store.StatusActive,
		Confidence:     op.Confidence,
		Origin:         store.OriginConsolidation,
		SourceSeqStart: i64ptr(op.Evidence.SeqStart),
		SourceSeqEnd:   i64ptr(op.Evidence.SeqEnd),
	}
}

func logOp(ctx context.Context, st *store.Store, tx *sql.Tx, ac ApplyContext, typ, domainID, memoryID, reason string, ev store.Evidence) error {
	return st.LogEvent(ctx, tx, store.Event{
		Type: typ, DomainID: domainID, MemoryID: memoryID, Reason: reason,
		Evidence: evidenceJSON(ev), Actor: ac.Actor, Model: ac.Model, PromptHash: ac.PromptHash,
	})
}

func evidenceJSON(e store.Evidence) string {
	b, err := json.Marshal(e)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func i64ptr(v int64) *int64 { return &v }
