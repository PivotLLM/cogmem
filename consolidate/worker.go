// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/PivotLLM/cogmem/logger"
	"github.com/PivotLLM/cogmem/store"
)

// leaseTTL bounds how long a single RunOnce may hold the per-archive lease.
const leaseTTL = 10 * time.Minute

// Observe records one conversation message into the store's inbox so the next
// consolidation run can read it. It is the host's per-message call: the host
// appends the message to its own transcript, gets a seq, and hands a copy here.
// Only meaningful roles (user, assistant) are kept — tool plumbing never reaches
// the model — and the text is capped at perMessageChars (0 = uncapped) so the
// inbox holds what a run would send, not whole file dumps. Returns whether the
// message was stored.
func Observe(ctx context.Context, st *store.Store, seq int64, role, text string, perMessageChars int) (bool, error) {
	if !MeaningfulRole(role) {
		return false, nil
	}
	text, _ = TruncateText(text, perMessageChars)
	if err := st.AppendInbox(ctx, st.DB(), seq, role, text); err != nil {
		return false, err
	}
	return true, nil
}

// ModelRequest is one call to a model. The host owns which model answers,
// credentials, transport, retries on transport errors and cooldowns.
type ModelRequest struct {
	System     string
	User       string
	JSONObject bool     // ask for a JSON-object response where the provider supports it
	Exclude    []string // model names the caller has learned to avoid for this call
}

// ModelReply is the answer. Model names which model produced it.
type ModelReply struct {
	Content      string
	FinishReason string
	Model        string
}

// ModelCaller is how the worker reaches a model. The host implements it and
// owns the model chain: it returns an error only when no model is available
// after honouring req.Exclude, or on a transport failure it could not route
// around. It does not validate the content. cogmem owns its output format, so
// the worker parses each reply itself and, when one is empty or not a
// consolidation Output, excludes that model and asks again (see
// maxModelAttempts). The interface is decoupled from providers so the worker
// never imports a provider package.
type ModelCaller interface {
	Complete(ctx context.Context, req ModelRequest) (ModelReply, error)
}

// maxModelAttempts bounds how many replies one run will ask for before giving
// up. Each unusable reply adds its model to Exclude for the next attempt.
const maxModelAttempts = 4

// errInvalidOutput is what callModel returns when every attempt produced text
// that did not parse as an Output. Its text is the clean, human-readable
// message recorded on the run; the raw JSON-parser errors only reach the log.
var errInvalidOutput = errors.New("memory model output was not valid JSON")

// Worker runs the consolidation "sleep cycle" against one store: it drains the
// store's inbox into memory operations.
type Worker struct {
	st    *store.Store
	model ModelCaller

	batchOpts      BatchOptions
	proposeDomains bool
	autoPromote    bool
	debugDump      string
	modelName      string

	// Retention windows in days; 0 means keep forever. Applied on each run,
	// because consolidation is already the regular sweep over a store and a
	// second scheduler for a delete this cheap would earn nothing.
	eventDays   int
	retiredDays int
}

// Option configures a Worker (functional-options pattern, per dev standards).
type Option func(*Worker)

// WithBatchOptions sets the batching levers (count/token/char budgets).
func WithBatchOptions(o BatchOptions) Option { return func(w *Worker) { w.batchOpts = o } }

// WithProposeDomains lets the model create new domains (informational lever).
func WithProposeDomains(v bool) Option { return func(w *Worker) { w.proposeDomains = v } }

// WithAutoPromote is currently informational: inferred items already land in
// review via the contract, so promotion stays a human/MCP action.
func WithAutoPromote(v bool) Option { return func(w *Worker) { w.autoPromote = v } }

// WithDebugDump writes the system/user/raw payloads of each run to dir.
func WithDebugDump(dir string) Option { return func(w *Worker) { w.debugDump = dir } }

// WithRetention sets how long event and retired memories are kept, in days.
// 0 for either means keep forever.
func WithRetention(eventDays, retiredDays int) Option {
	return func(w *Worker) { w.eventDays, w.retiredDays = eventDays, retiredDays }
}

// WithModelName sets the model name recorded on a run when the host's reply
// carries none.
func WithModelName(name string) Option { return func(w *Worker) { w.modelName = name } }

// NewWorker builds a Worker over a store and a model caller.
func NewWorker(st *store.Store, model ModelCaller, opts ...Option) *Worker {
	w := &Worker{
		st:        st,
		model:     model,
		batchOpts: DefaultBatchOptions(),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// RunParams identifies one consolidation run.
type RunParams struct {
	ID        string // label for logs and run records
	Dir       string // the memory's directory
	Workspace string // where the curated files and COGMEM.md are read from
	Trigger   string // message, idle, nightly, manual
}

// RunResult reports the outcome of a single RunOnce.
type RunResult struct {
	Applied  int
	More     bool
	Status   string // busy, idle, ok, error, invalid_json, aborted
	SeqStart int64
	SeqEnd   int64
}

// leaseName guards a store against two concurrent runs. One store, one lease.
const leaseName = "consolidate:" + store.InboxStateKey

// RunOnce performs one consolidation pass: it leases the store, selects the
// next batch of inbox messages past the watermark, asks the model to propose
// memory operations, validates them against the contract, and applies the
// valid result in one transaction. The watermark advances only on a successful
// apply, and only then is the covered part of the inbox deleted.
func (w *Worker) RunOnce(ctx context.Context, p RunParams) (RunResult, error) {
	owner := w.leaseOwner(p.ID)
	ok, err := w.st.AcquireLease(ctx, w.st.DB(), leaseName, owner, leaseTTL)
	if err != nil {
		return RunResult{}, fmt.Errorf("consolidate: acquire lease: %w", err)
	}
	if !ok {
		return RunResult{Status: "busy"}, nil
	}
	defer func() { _ = w.st.ReleaseLease(ctx, w.st.DB(), leaseName, owner) }()

	state, err := w.st.GetState(ctx, w.st.DB(), store.InboxStateKey)
	if err != nil {
		return RunResult{}, fmt.Errorf("consolidate: get state: %w", err)
	}
	consolidated := state.ConsolidatedSeq

	_, maxSeq, err := w.st.InboxBounds(ctx, w.st.DB())
	if err != nil {
		return RunResult{}, fmt.Errorf("consolidate: inbox bounds: %w", err)
	}
	if maxSeq <= consolidated {
		return RunResult{Status: "idle", SeqStart: consolidated + 1}, nil
	}

	src, err := w.st.InboxRange(ctx, w.st.DB(), consolidated+1, maxSeq)
	if err != nil {
		return RunResult{}, fmt.Errorf("consolidate: inbox range: %w", err)
	}
	msgs := make([]Message, 0, len(src))
	for _, m := range src {
		// Observe already filtered roles; a row written by an older host is
		// filtered again here so the model never sees plumbing.
		if !MeaningfulRole(m.Role) {
			continue
		}
		msgs = append(msgs, Message{Seq: m.Seq, Role: m.Role, Text: m.Text})
	}

	batch, lastSeq, more := SelectBatch(msgs, w.batchOpts)
	if len(batch) == 0 {
		return RunResult{Status: "idle", SeqStart: consolidated + 1}, nil
	}

	// Cleanup before the model sees current_state: retire exact-duplicate active
	// memories (e.g. a runaway loop that wrote the same fact repeatedly). Cheap
	// and idempotent; best-effort so a dedup error never blocks consolidation.
	if n, derr := w.st.DedupeActiveMemories(ctx); derr != nil {
		logger.WarnCF("cogmem", "dedupe active memories failed", map[string]any{
			"id": p.ID, "error": derr.Error(),
		})
	} else if n > 0 {
		logger.InfoCF("cogmem", "consolidation: retired duplicate memories", map[string]any{
			"id": p.ID, "retired": n,
		})
	}

	w.applyRetention(ctx, p.ID)

	in := Input{
		Curated:      ReadCurated(p.Workspace),
		CurrentState: w.currentState(ctx),
		NewMessages:  batch,
	}

	promptPath := PromptPath(p.Workspace)
	system, prompt := BuildPrompt(promptPath)
	if prompt.Ignored {
		logger.WarnCF("cogmem", "Ignoring per-agent consolidation instructions: "+prompt.Reason,
			map[string]any{"path": promptPath})
	}
	userJSON, err := json.Marshal(in)
	if err != nil {
		return RunResult{}, fmt.Errorf("consolidate: marshal input: %w", err)
	}

	result := RunResult{More: more, SeqStart: consolidated + 1, SeqEnd: lastSeq}
	started := time.Now()
	inputTokens := EstimateTokens(system + string(userJSON))

	out, reply, err := w.callModel(ctx, p.ID, system, string(userJSON))
	model := w.modelLabel(reply.Model)
	raw := reply.Content
	outputTokens := EstimateTokens(raw)
	if err != nil {
		if errors.Is(err, errInvalidOutput) {
			// Every attempt came back unusable. Record the clean message, never
			// the parser's ("unexpected end of JSON input"), and keep the inbox
			// for the next run.
			w.recordRun(ctx, p, model, "invalid_json", 0, consolidated+1, lastSeq, inputTokens, outputTokens, err.Error(), "", started)
			w.dump(p, system, string(userJSON), raw, 0)
			result.Status = "invalid_json"
			return result, nil
		}
		// The host had no model left to try (or a transport failure it could
		// not route around).
		w.recordRun(ctx, p, model, "error", 0, consolidated+1, lastSeq, inputTokens, outputTokens, err.Error(), "", started)
		result.Status = "error"
		return result, fmt.Errorf("consolidate: model call: %w", err)
	}

	// Repair safe, mechanically-fixable deviations (e.g. an inferred item the
	// model marked active → review) before validating, so a single such mistake
	// doesn't discard the whole batch and lose real memories. Genuinely malformed
	// batches still fail Validate below.
	repairs := out.Normalize()

	if verr := out.Validate(in); verr != nil {
		w.recordRun(ctx, p, model, "aborted", 0, consolidated+1, lastSeq, inputTokens, outputTokens, verr.Error(), "", started)
		w.dump(p, system, string(userJSON), raw, 0)
		result.Status = "aborted"
		return result, nil
	}

	applied, err := Apply(ctx, w.st, out, ApplyContext{
		Actor: actorSleepCycle,
		Model: model,
	})
	if err != nil {
		w.recordRun(ctx, p, model, "error", applied, consolidated+1, lastSeq, inputTokens, outputTokens, err.Error(), "", started)
		result.Status = "error"
		return result, fmt.Errorf("consolidate: apply: %w", err)
	}

	if err := w.st.SetWatermark(ctx, w.st.DB(), store.InboxStateKey, lastSeq, maxSeq); err != nil {
		return result, fmt.Errorf("consolidate: set watermark: %w", err)
	}

	// The watermark is the source of truth; the inbox rows behind it are now
	// just weight. Best-effort: a delete failure is recorded on the run but the
	// run itself stands, and the next run's delete covers the same rows again.
	drainErr := ""
	if _, err := w.st.DeleteInboxThrough(ctx, w.st.DB(), lastSeq); err != nil {
		drainErr = "drain inbox: " + err.Error()
	}
	// Auto-repairs are a NOTE: the run succeeded and the deviation was safely
	// corrected. drainErr is a real error — the run itself was fine, but the
	// cleanup genuinely failed — so it stays in Error.
	runNote := ""
	if len(repairs) > 0 {
		runNote = "auto-repaired: " + strings.Join(repairs, "; ")
	}
	w.recordRun(ctx, p, model, "ok", applied, consolidated+1, lastSeq, inputTokens, outputTokens, drainErr, runNote, started)
	w.dump(p, system, string(userJSON), raw, applied)

	result.Applied = applied
	result.Status = "ok"
	return result, nil
}

// callModel asks the host for a reply and validates it as a consolidation
// Output. An empty or unparseable reply is logged, its model is added to
// Exclude, and the host is asked again, up to maxModelAttempts in total. On
// success reply is the accepted answer; on failure it is the last one tried,
// so its Model still names the model to record. A host error is returned as
// is; exhausting the attempts returns errInvalidOutput.
func (w *Worker) callModel(ctx context.Context, runID, system, user string) (Output, ModelReply, error) {
	req := ModelRequest{System: system, User: user, JSONObject: true}
	var last ModelReply
	for attempt := 1; attempt <= maxModelAttempts; attempt++ {
		reply, err := w.model.Complete(ctx, req)
		if err != nil {
			if reply.Model == "" {
				reply.Model = last.Model
			}
			return Output{}, reply, err
		}
		last = reply

		out, perr := ParseOutput(reply.Content)
		if perr == nil {
			return out, reply, nil
		}
		reason := "not valid JSON"
		if strings.TrimSpace(reply.Content) == "" {
			reason = "empty"
		}
		logger.WarnCF("cogmem", "consolidation: model reply unusable", map[string]any{
			"id":            runID,
			"model":         w.modelLabel(reply.Model),
			"attempt":       attempt,
			"max_attempts":  maxModelAttempts,
			"reason":        reason,
			"parse_error":   perr.Error(),
			"finish_reason": reply.FinishReason,
			"content_chars": len(reply.Content),
		})
		if reply.Model != "" && !slices.Contains(req.Exclude, reply.Model) {
			req.Exclude = append(req.Exclude, reply.Model)
		}
	}
	return Output{}, last, fmt.Errorf("%w after %d attempts", errInvalidOutput, maxModelAttempts)
}

// modelLabel names a model for run records and logs: the reply's own name, or
// the configured WithModelName when the reply carried none.
func (w *Worker) modelLabel(name string) string {
	if name != "" {
		return name
	}
	return w.modelName
}

// currentState projects the active domains and their active memories into the
// compact view the model sees.
func (w *Worker) currentState(ctx context.Context) CurrentState {
	cs := CurrentState{}
	domains, err := w.st.ListDomains(ctx, w.st.DB(), store.StatusActive)
	if err != nil {
		return cs
	}
	for _, d := range domains {
		dv := DomainView{
			ID:              d.ID,
			Sticky:          d.Sticky(),
			Name:            d.Name,
			Status:          string(d.Status),
			Version:         d.Version,
			Summary:         d.Summary,
			State:           d.State,
			Triggers:        d.Triggers,
			KeywordTriggers: d.KeywordTriggers,
		}
		// Prompt memories only: the model de-duplicates against what it sees, and
		// a domain holding hundreds of event memories would crowd out everything
		// else in the state view.
		hooks, err := w.st.ListPromptMemories(ctx, w.st.DB(), d.ID)
		if err == nil {
			for _, h := range hooks {
				dv.Memories = append(dv.Memories, MemoryView{
					ID:         h.ID,
					Type:       string(h.Type),
					Text:       h.Text,
					Confidence: h.Confidence,
					AgeDays:    int(time.Since(h.CreatedAt).Hours() / 24),
				})
			}
		}
		cs.Domains = append(cs.Domains, dv)
	}
	return cs
}

// applyRetention deletes memories that have aged out.
//
// Events go because they stop being useful long before they stop accumulating —
// an hourly "nothing changed" note reached 300 rows on one agent. Retired
// memories go because retiring leaves the row behind, so a store that retires
// steadily grows forever while showing nothing for it.
//
// Best-effort and non-fatal: a purge failure must never stop the consolidation
// run that was actually asked for.
func (w *Worker) applyRetention(ctx context.Context, id string) {
	if n, err := w.st.PurgeExpiredEvents(ctx, w.st.DB(), w.eventDays); err != nil {
		logger.WarnCF("cogmem", "purge expired events failed", map[string]any{
			"id": id, "error": err.Error(),
		})
	} else if n > 0 {
		logger.InfoCF("cogmem", "consolidation: deleted expired event memories", map[string]any{
			"id": id, "deleted": n, "older_than_days": w.eventDays,
		})
	}
	if n, err := w.st.PurgeRetiredMemories(ctx, w.st.DB(), w.retiredDays); err != nil {
		logger.WarnCF("cogmem", "purge retired memories failed", map[string]any{
			"id": id, "error": err.Error(),
		})
	} else if n > 0 {
		logger.InfoCF("cogmem", "consolidation: deleted retired memories", map[string]any{
			"id": id, "deleted": n, "retired_more_than_days_ago": w.retiredDays,
		})
	}
}

// recordRun writes the run record. errMsg is why the run FAILED; note is
// something worth reporting about a run that SUCCEEDED. They are separate
// arguments on purpose: passing a note as an error is what made the memory page
// show a successful auto-repair in red.
func (w *Worker) recordRun(ctx context.Context, p RunParams, model, status string, applied int, seqStart, seqEnd int64, inTok, outTok int, errMsg, note string, started time.Time) {
	finished := time.Now()
	_ = w.st.RecordRun(ctx, w.st.DB(), store.Run{
		Trigger:      p.Trigger,
		Model:        model,
		SeqStart:     seqStart,
		SeqEnd:       seqEnd,
		InputTokens:  inTok,
		OutputTokens: outTok,
		Status:       status,
		OpsApplied:   applied,
		Error:        errMsg,
		Note:         note,
		StartedAt:    started,
		FinishedAt:   &finished,
	})
}

// dump writes the run payloads for offline model comparison when a debug dir is
// configured. Best-effort: failures are ignored.
func (w *Worker) dump(p RunParams, system, userJSON, raw string, applied int) {
	if w.debugDump == "" {
		return
	}
	if err := os.MkdirAll(w.debugDump, 0o755); err != nil {
		return
	}
	rec := struct {
		System   string `json:"system"`
		UserJSON string `json:"user_json"`
		Raw      string `json:"raw"`
		Applied  int    `json:"applied"`
	}{system, userJSON, raw, applied}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	name := fmt.Sprintf("%s-%s.json", time.Now().UTC().Format("20060102T150405.000"), uuid.NewString()[:8])
	_ = os.WriteFile(filepath.Join(w.debugDump, name), b, 0o644)
}

func (w *Worker) leaseOwner(label string) string {
	id := uuid.NewString()
	if label == "" {
		return id
	}
	return fmt.Sprintf("%s-%d-%s", label, os.Getpid(), id)
}

// ParseOutput trims the raw model text, strips a leading/trailing ```json fence
// if present, and unmarshals it into an Output.
func ParseOutput(raw string) (Output, error) {
	s := strings.TrimSpace(raw)
	s = stripFence(s)
	var out Output
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return Output{}, err
	}
	return out, nil
}

func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (``` or ```json).
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	// Drop a trailing closing fence.
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}
