// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// fakeModel returns a canned raw string regardless of input.
type fakeModel struct {
	raw   string
	model string
	err   error
}

func (m *fakeModel) Consolidate(ctx context.Context, system, userJSON string) (string, string, error) {
	name := m.model
	if name == "" {
		name = "fake-model"
	}
	return m.raw, name, m.err
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
		AgentID: "alice", SessionKey: "agent:alice:main",
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

	w := NewWorker(s, &fakeModel{raw: "not json at all"}, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "invalid_json" {
		t.Fatalf("status = %q, want invalid_json", res.Status)
	}
	st, _ := s.GetState(context.Background(), s.DB(), store.InboxStateKey)
	if st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on invalid json, want 0", st.ConsolidatedSeq)
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
