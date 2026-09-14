// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/PivotLLM/cogmem/store"
)

// ApplyContext carries the metadata recorded on every applied operation.
type ApplyContext struct {
	Actor      string // sleep_cycle, mcp_tool, ...
	Model      string
	PromptHash string
}

// Apply writes a *validated* Output to the store in a single transaction:
// assigning ids to created domains, mapping tmp_ids referenced by hooks,
// applying every op, and appending an audit event per op. It returns the number
// of operations applied, which is 0 on error: the transaction rolled back, so
// nothing was applied. Call Output.Validate before Apply.
func Apply(ctx context.Context, st *store.Store, out Output, ac ApplyContext) (int, error) {
	applied := 0
	err := st.WithTx(ctx, func(tx *sql.Tx) error {
		tmp := map[string]string{} // tmp_id -> assigned domain id

		for _, op := range out.DomainOps {
			switch op.Op {
			case "create":
				d, err := st.CreateDomain(ctx, tx, store.CreateDomainParams{
					Sticky:          op.Sticky != nil && *op.Sticky,
					Name:            op.Name,
					Status:          store.Status(orDefault(op.Status, "active")),
					Summary:         op.Summary,
					Triggers:        op.Triggers,
					KeywordTriggers: op.KeywordTriggers,
				})
				if err != nil {
					return err
				}
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
					k := op.KeywordTriggers
					p.KeywordTriggers = &k
				}
				if op.Sticky != nil {
					p.Sticky = op.Sticky
				}
				if err := st.UpdateDomain(ctx, tx, op.ID, p); err != nil {
					return err
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
			applied++
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
			applied++
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
		return 0, err
	}
	return applied, nil
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
