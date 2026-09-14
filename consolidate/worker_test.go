// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// seedInbox feeds messages through Observe, the way the host does per turn.
// Returns the store so calls can be chained onto openStore.
func seedInbox(t *testing.T, s *store.Store, msgs []Message) *store.Store {
	t.Helper()
	for _, m := range msgs {
		if _, err := Observe(context.Background(), s, m.Seq, m.Role, m.Text, 0); err != nil {
			t.Fatalf("observe seq %d: %v", m.Seq, err)
		}
	}
	return s
}

// inboxSeqs lists the seqs still waiting in the inbox, ascending.
func inboxSeqs(t *testing.T, s *store.Store) []int64 {
	t.Helper()
	rows, err := s.InboxRange(context.Background(), s.DB(), 0, 1<<62)
	if err != nil {
		t.Fatalf("inbox range: %v", err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

// fakeModel is a scripted ModelCaller. With only raw set it answers every
// request with that content as "fake-model" (or model); with replies set it
// answers in order, repeating the last one; with err set it fails every call.
// It records every request so tests can assert on Exclude and JSONObject.
type fakeModel struct {
	raw     string
	model   string
	err     error
	replies []ModelReply

	requests []ModelRequest
}

func (m *fakeModel) Complete(ctx context.Context, req ModelRequest) (ModelReply, error) {
	m.requests = append(m.requests, req)
	if m.err != nil {
		return ModelReply{}, m.err
	}
	if len(m.replies) > 0 {
		i := min(len(m.requests)-1, len(m.replies)-1)
		return m.replies[i], nil
	}
	name := m.model
	if name == "" {
		name = "fake-model"
	}
	return ModelReply{Content: m.raw, FinishReason: "stop", Model: name}, nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.cogmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleMessages() []Message {
	return []Message{
		{Seq: 1, Role: "user", Text: "Please always run gofmt before committing."},
		{Seq: 2, Role: "assistant", Text: "Understood, I'll run gofmt first."},
		{Seq: 3, Role: "tool", Text: "ignored plumbing"},
	}
}

func params() RunParams {
	return RunParams{
		ID:        "alice",
		Dir:       "/nonexistent-dir",
		Workspace: "/nonexistent-workspace",
		Trigger:   "message",
	}
}

// seedDomain creates a project domain with one active rule hook and returns the
// domain id, the hook id, and the highest hook id we may supersede.
func seedDomain(t *testing.T, s *store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
		Name: "ClawEh", Status: store.StatusActive,
		Summary: "Go gateway project",
	})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	h, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeRule, Text: "Run make test after changes.",
		Status: store.StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("seed hook: %v", err)
	}
	return d.ID, h.ID
}

func TestRunOnceHappyPath(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	// A valid supersede: replace the existing rule with a new one, evidence in batch.
	raw := fmt.Sprintf(`{
		"domain_ops": [],
		"memory_ops": [{
			"op": "supersede",
			"domain": %q,
			"old_id": %q,
			"type": "rule",
			"text": "Always run gofmt and make test before committing.",
			"status": "active",
			"source": "user_explicit",
			"confidence": 0.95,
			"evidence": {"seq_start": 1, "seq_end": 2}
		}],
		"conflict_ledger": []
	}`, domainID, memoryID)

	w := NewWorker(s, &fakeModel{raw: raw}, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if res.Applied < 1 {
		t.Fatalf("applied = %d, want >=1", res.Applied)
	}

	ctx := context.Background()
	st, _ := s.GetState(ctx, s.DB(), store.InboxStateKey)
	if st.ConsolidatedSeq != 2 {
		t.Fatalf("consolidated_seq = %d, want 2 (lastSeq)", st.ConsolidatedSeq)
	}
	// seq 3 is tool plumbing, which Observe never stores, so the inbox's highest
	// seq — and therefore last_seen — is 2.
	if st.LastSeenSeq != 2 {
		t.Fatalf("last_seen_seq = %d, want 2 (inbox max)", st.LastSeenSeq)
	}

	run, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if run.Status != "ok" || run.OpsApplied < 1 {
		t.Fatalf("run = %+v, want status ok applied>=1", run)
	}

	// Old hook retired, new one active.
	active, _ := s.ListMemories(ctx, s.DB(), domainID, store.StatusActive)
	if len(active) != 1 || active[0].Text != "Always run gofmt and make test before committing." {
		t.Fatalf("active hooks = %+v", active)
	}
}

func TestRunOnceDrainsInboxOnSuccess(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	raw := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":"Run gofmt and tests.","status":"active","source":"user_explicit","evidence":{"seq_start":1,"seq_end":2}}],"conflict_ledger":[]}`, domainID, memoryID)

	w := NewWorker(s, &fakeModel{raw: raw}, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	// Observe keeps only meaningful roles, so seqs 1 and 2 were in the inbox and
	// both are covered by the run (lastSeq 2). Nothing should remain.
	if left := inboxSeqs(t, s); len(left) != 0 {
		t.Fatalf("inbox after successful run = %v, want empty", left)
	}
}

func TestRunOnceKeepsInboxOnInvalidJSON(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	w := NewWorker(s, &fakeModel{raw: "not json at all"})
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "invalid_json" {
		t.Fatalf("status = %q, want invalid_json", res.Status)
	}
	if left := inboxSeqs(t, s); len(left) != 2 {
		t.Fatalf("inbox after invalid_json = %v, want the 2 meaningful messages kept", left)
	}
}

func TestRunOnceKeepsInboxOnAborted(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	// Evidence seq_end 99 is outside the batch [1,2] → Validate fails → aborted.
	raw := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":"Out of range.","status":"active","source":"user_explicit","evidence":{"seq_start":1,"seq_end":99}}],"conflict_ledger":[]}`, domainID, memoryID)

	w := NewWorker(s, &fakeModel{raw: raw})
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", res.Status)
	}
	if left := inboxSeqs(t, s); len(left) != 2 {
		t.Fatalf("inbox after aborted = %v, want the 2 meaningful messages kept", left)
	}
}

func TestObserve_FiltersAndTruncates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if stored, err := Observe(ctx, s, 1, "tool", "plumbing", 0); err != nil || stored {
		t.Fatalf("tool role: stored=%v err=%v, want dropped", stored, err)
	}
	long := "0123456789abcdef"
	if stored, err := Observe(ctx, s, 2, "user", long, 8); err != nil || !stored {
		t.Fatalf("user role: stored=%v err=%v", stored, err)
	}
	// A replayed seq (retried turn) must not duplicate or overwrite.
	if _, err := Observe(ctx, s, 2, "user", "second copy", 0); err != nil {
		t.Fatal(err)
	}
	rows, err := s.InboxRange(ctx, s.DB(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Seq != 2 {
		t.Fatalf("inbox = %+v, want exactly seq 2", rows)
	}
	if rows[0].Text != "01234567 …[truncated]" {
		t.Fatalf("text = %q, want truncated at 8 chars", rows[0].Text)
	}
}

func TestRunOnceInvalidJSON(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{raw: "not json at all"}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "invalid_json" {
		t.Fatalf("status = %q, want invalid_json", res.Status)
	}
	if len(m.requests) != maxModelAttempts {
		t.Fatalf("model calls = %d, want %d (every attempt used)", len(m.requests), maxModelAttempts)
	}
	for i, req := range m.requests {
		if !req.JSONObject {
			t.Fatalf("request %d: JSONObject = false, want true", i)
		}
	}
	ctx := context.Background()
	st, _ := s.GetState(ctx, s.DB(), store.InboxStateKey)
	if st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on invalid json, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); len(left) != 2 {
		t.Fatalf("inbox after invalid_json = %v, want the 2 meaningful messages kept", left)
	}
	run, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if run.Status != "invalid_json" || run.Model != "fake-model" {
		t.Fatalf("run = %+v, want status invalid_json model fake-model", run)
	}
	// The recorded reason is the worker's own sentence, never the JSON
	// parser's ("invalid character 'o' in literal null" and friends).
	if run.Error == "" || strings.Contains(run.Error, "invalid character") || strings.Contains(run.Error, "unexpected end") {
		t.Fatalf("run.Error = %q, want a clean message without parser text", run.Error)
	}
}

func TestRunOnceRetriesUnusableReplyOnAnotherModel(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	good := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":"Run gofmt and tests.","status":"active","source":"user_explicit","evidence":{"seq_start":1,"seq_end":2}}],"conflict_ledger":[]}`, domainID, memoryID)
	m := &fakeModel{replies: []ModelReply{
		{Content: "Sure! Here are the memory ops:", FinishReason: "stop", Model: "model-a"},
		{Content: good, FinishReason: "stop", Model: "model-b"},
	}}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "ok" || res.Applied < 1 {
		t.Fatalf("result = %+v, want ok with applied>=1", res)
	}
	if len(m.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(m.requests))
	}
	if len(m.requests[0].Exclude) != 0 {
		t.Fatalf("first request Exclude = %v, want empty", m.requests[0].Exclude)
	}
	if got := m.requests[1].Exclude; len(got) != 1 || got[0] != "model-a" {
		t.Fatalf("second request Exclude = %v, want [model-a]", got)
	}
	run, ok, err := s.LastRun(context.Background(), s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if run.Model != "model-b" {
		t.Fatalf("run.Model = %q, want model-b (the model whose reply was accepted)", run.Model)
	}
}

func TestRunOnceExcludeGrowsWithEachUnusableModel(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	// Four distinct unusable replies: prose, an empty body, whitespace, and
	// truncated JSON. Each model must be excluded from every later attempt.
	m := &fakeModel{replies: []ModelReply{
		{Content: "I cannot help with that.", FinishReason: "stop", Model: "model-a"},
		{Content: "", FinishReason: "stop", Model: "model-b"},
		{Content: "  \n\t", FinishReason: "stop", Model: "model-c"},
		{Content: `{"domain_ops":[`, FinishReason: "length", Model: "model-d"},
	}}
	w := NewWorker(s, m)
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "invalid_json" {
		t.Fatalf("status = %q, want invalid_json", res.Status)
	}
	if len(m.requests) != maxModelAttempts {
		t.Fatalf("model calls = %d, want %d", len(m.requests), maxModelAttempts)
	}
	wantExclude := [][]string{
		nil,
		{"model-a"},
		{"model-a", "model-b"},
		{"model-a", "model-b", "model-c"},
	}
	for i, req := range m.requests {
		if !slices.Equal(req.Exclude, wantExclude[i]) {
			t.Fatalf("request %d Exclude = %v, want %v", i, req.Exclude, wantExclude[i])
		}
	}
	run, ok, err := s.LastRun(context.Background(), s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if run.Model != "model-d" {
		t.Fatalf("run.Model = %q, want model-d (the last one tried)", run.Model)
	}
}

func TestRunOnceHostErrorRecordsError(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{err: errors.New("no memory model available")}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err == nil || !strings.Contains(err.Error(), "no memory model available") {
		t.Fatalf("RunOnce err = %v, want the host's error wrapped", err)
	}
	if res.Status != "error" {
		t.Fatalf("status = %q, want error", res.Status)
	}
	if len(m.requests) != 1 {
		t.Fatalf("model calls = %d, want 1 (a host error is not retried here)", len(m.requests))
	}
	ctx := context.Background()
	st, _ := s.GetState(ctx, s.DB(), store.InboxStateKey)
	if st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on host error, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); len(left) != 2 {
		t.Fatalf("inbox after host error = %v, want the 2 meaningful messages kept", left)
	}
	run, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if run.Status != "error" || run.Error != "no memory model available" || run.Model != "test-model" {
		t.Fatalf("run = %+v, want status error with the host's message and the fallback model name", run)
	}
}

func TestRunOnceValidationAborted(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	// Evidence seq_end 99 is outside the batch [1,2] → Validate fails.
	raw := fmt.Sprintf(`{
		"domain_ops": [],
		"memory_ops": [{
			"op": "supersede",
			"domain": %q,
			"old_id": %q,
			"type": "rule",
			"text": "Out of range evidence.",
			"status": "active",
			"source": "user_explicit",
			"evidence": {"seq_start": 1, "seq_end": 99}
		}],
		"conflict_ledger": []
	}`, domainID, memoryID)

	w := NewWorker(s, &fakeModel{raw: raw}, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", res.Status)
	}
	st, _ := s.GetState(context.Background(), s.DB(), store.InboxStateKey)
	if st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on aborted, want 0", st.ConsolidatedSeq)
	}
}

func TestRunOnceIdleNoMessages(t *testing.T) {
	s := openStore(t)
	w := NewWorker(s, &fakeModel{raw: "{}"})
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "idle" {
		t.Fatalf("status = %q, want idle", res.Status)
	}
}

func TestRunOnceIdleAlreadyConsolidated(t *testing.T) {
	s := openStore(t)
	seedInbox(t, s, sampleMessages())
	// Watermark already past max seq.
	if err := s.SetWatermark(context.Background(), s.DB(), store.InboxStateKey, 3, 3); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	w := NewWorker(s, &fakeModel{raw: "{}"})
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "idle" {
		t.Fatalf("status = %q, want idle", res.Status)
	}
}

func TestRunOnceBusyWhenLeased(t *testing.T) {
	s := openStore(t)
	seedInbox(t, s, sampleMessages())
	// Hold the lease as someone else.
	ok, err := s.AcquireLease(context.Background(), s.DB(), leaseName, "other", leaseTTL)
	if err != nil || !ok {
		t.Fatalf("pre-acquire lease: ok=%v err=%v", ok, err)
	}
	w := NewWorker(s, &fakeModel{raw: "{}"})
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "busy" {
		t.Fatalf("status = %q, want busy", res.Status)
	}
}

func TestRunOnceDebugDump(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())
	dir := t.TempDir()

	raw := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":"Run gofmt and tests.","status":"active","source":"user_explicit","evidence":{"seq_start":1,"seq_end":2}}],"conflict_ledger":[]}`, domainID, memoryID)

	w := NewWorker(s, &fakeModel{raw: raw}, WithDebugDump(dir))
	if _, err := w.RunOnce(context.Background(), params()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("debug dump files = %d, want 1", len(entries))
	}
}
