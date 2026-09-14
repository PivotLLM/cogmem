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
	"sync"
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
// answers in order, repeating the last one; with err set it fails every call;
// with errs set, call i fails with errs[i] when that entry is non-nil. It
// records every request so tests can assert on Exclude and JSONObject, and
// signals each call on called when one is provided. Safe for concurrent use so
// the manager tests can drive it from the worker pool.
type fakeModel struct {
	raw     string
	model   string
	err     error
	errs    []error
	replies []ModelReply
	called  chan struct{}

	mu       sync.Mutex
	requests []ModelRequest
}

func (m *fakeModel) Complete(_ context.Context, req ModelRequest) (ModelReply, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	m.mu.Unlock()
	if m.called != nil {
		select {
		case m.called <- struct{}{}:
		default:
		}
	}
	if m.err != nil {
		return ModelReply{}, m.err
	}
	if i < len(m.errs) && m.errs[i] != nil {
		return ModelReply{}, m.errs[i]
	}
	if len(m.replies) > 0 {
		return m.replies[min(i, len(m.replies)-1)], nil
	}
	name := m.model
	if name == "" {
		name = "fake-model"
	}
	return ModelReply{Content: m.raw, FinishReason: "stop", Model: name}, nil
}

// calls reports how many requests the model has received.
func (m *fakeModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// snapshot returns a copy of the recorded requests.
func (m *fakeModel) snapshot() []ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
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

// meaningfulMessages is what Observe keeps of sampleMessages: the tool row is
// plumbing and never reaches the inbox.
func meaningfulMessages() []Message {
	return sampleMessages()[:2]
}

func params() RunParams {
	return RunParams{
		ID:        "alice",
		Dir:       "/nonexistent-dir",
		Workspace: "/nonexistent-workspace",
		Trigger:   "message",
	}
}

const emptyOutput = `{"domain_ops":[],"memory_ops":[],"conflict_ledger":[]}`

// supersedeOutput is a valid model reply that replaces memoryID in domainID
// with text, citing the whole sample batch.
func supersedeOutput(domainID, memoryID, text string) string {
	return fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":%q,"confidence":0.95,"evidence":{"seq_start":1,"seq_end":2}}],"conflict_ledger":[]}`, domainID, memoryID, text)
}

// seedDomain creates a project domain with one active rule hook and returns the
// domain id and the hook id.
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

// decodeInput parses a request's user payload as the consolidation Input.
func decodeInput(t *testing.T, req ModelRequest) Input {
	t.Helper()
	var in Input
	if err := json.Unmarshal([]byte(req.User), &in); err != nil {
		t.Fatalf("user payload is not an Input: %v\n%s", err, req.User)
	}
	return in
}

// domainView finds a domain by id in the state the model was shown.
func domainView(t *testing.T, in Input, id string) DomainView {
	t.Helper()
	for _, d := range in.CurrentState.Domains {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("domain %q not in current_state %+v", id, in.CurrentState.Domains)
	return DomainView{}
}

func lastRun(t *testing.T, s *store.Store) store.Run {
	t.Helper()
	run, ok, err := s.LastRun(context.Background(), s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	return run
}

func watermark(t *testing.T, s *store.Store) store.ConsolidationState {
	t.Helper()
	st, err := s.GetState(context.Background(), s.DB(), store.InboxStateKey)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	return st
}

func listEvents(t *testing.T, s *store.Store) []store.Event {
	t.Helper()
	events, err := s.ListEvents(context.Background(), s.DB())
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}

// The README promises "up to four attempts in total". The constant is the
// product's contract with hosts, so a change to it must be deliberate.
func TestMaxModelAttemptsIsFour(t *testing.T) {
	if maxModelAttempts != 4 {
		t.Fatalf("maxModelAttempts = %d, want 4 (documented in README)", maxModelAttempts)
	}
}

func TestRunOnceHappyPath(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	const newText = "Always run gofmt and make test before committing."
	m := &fakeModel{raw: supersedeOutput(domainID, memoryID, newText), model: "test-model"}
	w := NewWorker(s, m, WithModelName("fallback-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "ok" || res.Applied != 1 || res.More || res.SeqStart != 1 || res.SeqEnd != 2 {
		t.Fatalf("result = %+v, want ok, applied 1, no more, seq [1,2]", res)
	}

	// What the model was asked.
	if m.calls() != 1 {
		t.Fatalf("model calls = %d, want 1", m.calls())
	}
	req := m.requests[0]
	if !req.JSONObject {
		t.Error("JSONObject = false, want true")
	}
	if len(req.Exclude) != 0 {
		t.Errorf("Exclude = %v, want empty on the first attempt", req.Exclude)
	}
	if req.System != DefaultPrompt() {
		t.Error("system prompt differs from the embedded contract although the workspace has no COGMEM.md")
	}
	in := decodeInput(t, req)
	if !slices.Equal(in.NewMessages, meaningfulMessages()) {
		t.Errorf("new_messages = %+v, want exactly %+v", in.NewMessages, meaningfulMessages())
	}
	if in.Curated != (Curated{}) {
		t.Errorf("curated = %+v, want empty for a missing workspace", in.Curated)
	}
	dv := domainView(t, in, domainID)
	if dv.Name != "ClawEh" || dv.Status != "active" || dv.Summary != "Go gateway project" {
		t.Errorf("domain view = %+v", dv)
	}
	if len(dv.Memories) != 1 || dv.Memories[0].ID != memoryID || dv.Memories[0].Type != "rule" ||
		dv.Memories[0].Text != "Run make test after changes." || dv.Memories[0].AgeDays != 0 {
		t.Errorf("memories shown = %+v, want the seeded rule at age 0", dv.Memories)
	}

	// Watermark and inbox.
	st := watermark(t, s)
	if st.ConsolidatedSeq != 2 {
		t.Fatalf("consolidated_seq = %d, want 2 (lastSeq)", st.ConsolidatedSeq)
	}
	// seq 3 is tool plumbing, which Observe never stores, so the inbox's highest
	// seq — and therefore last_seen — is 2.
	if st.LastSeenSeq != 2 {
		t.Fatalf("last_seen_seq = %d, want 2 (inbox max)", st.LastSeenSeq)
	}
	if left := inboxSeqs(t, s); len(left) != 0 {
		t.Fatalf("inbox after successful run = %v, want empty", left)
	}

	// Run record.
	run := lastRun(t, s)
	if run.Status != "ok" || run.OpsApplied != 1 || run.Model != "test-model" || run.Trigger != "message" ||
		run.SeqStart != 1 || run.SeqEnd != 2 || run.Error != "" || run.Note != "" {
		t.Fatalf("run = %+v, want ok/1 op/test-model/message/[1,2] with no error or note", run)
	}
	if run.InputTokens <= 0 || run.OutputTokens <= 0 || run.FinishedAt == nil {
		t.Errorf("run = %+v, want token estimates and a finish time", run)
	}

	// Old hook retired, new one active.
	ctx := context.Background()
	active, err := s.ListMemories(ctx, s.DB(), domainID, store.StatusActive)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if len(active) != 1 || active[0].Text != newText {
		t.Fatalf("active hooks = %+v", active)
	}
	old, err := s.GetMemory(ctx, s.DB(), memoryID)
	if err != nil || old.Status != store.StatusRetired {
		t.Fatalf("old hook = %+v err=%v, want retired", old, err)
	}

	// Audit ledger: one merge event naming the new memory.
	events := listEvents(t, s)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one", events)
	}
	ev := events[0]
	if ev.Type != "merge" || ev.DomainID != domainID || ev.MemoryID != active[0].ID ||
		ev.Actor != "sleep_cycle" || ev.Model != "test-model" ||
		ev.Evidence != `{"seq_start":1,"seq_end":2}` {
		t.Fatalf("event = %+v, want merge of %s by sleep_cycle/test-model with evidence [1,2]", ev, active[0].ID)
	}
}

// The workspace reaches the model: COGMEM.md is appended to the system prompt
// and the curated files travel in the user payload.
func TestRunOnceSendsWorkspaceToModel(t *testing.T) {
	ws := t.TempDir()
	const instructions = "Never record anything about medical matters."
	files := map[string]string{
		PromptFilename: instructions,
		"AGENTS.md":    "agents body",
		"USER.md":      "The user is Eric.",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{raw: emptyOutput}
	p := params()
	p.Workspace = ws
	res, err := NewWorker(s, m).RunOnce(context.Background(), p)
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
	req := m.requests[0]
	if !strings.HasPrefix(req.System, DefaultPrompt()) {
		t.Error("system prompt does not start with the embedded contract")
	}
	if !strings.Contains(req.System, "# AGENT-SPECIFIC INSTRUCTIONS") || !strings.Contains(req.System, instructions) {
		t.Error("system prompt lacks the appended COGMEM.md instructions")
	}
	if want, _ := BuildPrompt(PromptPath(ws)); req.System != want {
		t.Error("system prompt is not BuildPrompt of the workspace COGMEM.md")
	}
	in := decodeInput(t, req)
	want := Curated{AgentsMD: "agents body", UserMD: "The user is Eric."}
	if in.Curated != want {
		t.Errorf("curated = %+v, want %+v", in.Curated, want)
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
	if len(m.requests) != 4 {
		t.Fatalf("model calls = %d, want 4 (every attempt used)", len(m.requests))
	}
	for i, req := range m.requests {
		if !req.JSONObject {
			t.Fatalf("request %d: JSONObject = false, want true", i)
		}
	}
	if st := watermark(t, s); st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on invalid json, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("inbox after invalid_json = %v, want the 2 meaningful messages kept", left)
	}
	run := lastRun(t, s)
	if run.Status != "invalid_json" || run.Model != "fake-model" || run.OpsApplied != 0 {
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

	m := &fakeModel{replies: []ModelReply{
		{Content: "Sure! Here are the memory ops:", FinishReason: "stop", Model: "model-a"},
		{Content: supersedeOutput(domainID, memoryID, "Run gofmt and tests."), FinishReason: "stop", Model: "model-b"},
	}}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "ok" || res.Applied != 1 {
		t.Fatalf("result = %+v, want ok with applied 1", res)
	}
	if len(m.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(m.requests))
	}
	if len(m.requests[0].Exclude) != 0 {
		t.Fatalf("first request Exclude = %v, want empty", m.requests[0].Exclude)
	}
	if got := m.requests[1].Exclude; !slices.Equal(got, []string{"model-a"}) {
		t.Fatalf("second request Exclude = %v, want [model-a]", got)
	}
	if run := lastRun(t, s); run.Model != "model-b" {
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
	if run := lastRun(t, s); run.Model != "model-d" {
		t.Fatalf("run.Model = %q, want model-d (the last one tried)", run.Model)
	}
}

// A reply with no model name cannot be excluded (there is nothing to name),
// and a model already excluded is not listed twice.
func TestRunOnceExcludeSkipsUnnamedAndDuplicateModels(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{replies: []ModelReply{
		{Content: "nope", FinishReason: "stop"},
		{Content: "nope", FinishReason: "stop", Model: "model-a"},
		{Content: "nope", FinishReason: "stop", Model: "model-a"},
		{Content: "nope", FinishReason: "stop"},
	}}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil || res.Status != "invalid_json" {
		t.Fatalf("RunOnce = %+v, %v; want invalid_json", res, err)
	}
	wantExclude := [][]string{nil, nil, {"model-a"}, {"model-a"}}
	for i, req := range m.requests {
		if !slices.Equal(req.Exclude, wantExclude[i]) {
			t.Errorf("request %d Exclude = %v, want %v", i, req.Exclude, wantExclude[i])
		}
	}
	if run := lastRun(t, s); run.Model != "test-model" {
		t.Errorf("run.Model = %q, want the WithModelName fallback for an unnamed last reply", run.Model)
	}
}

// When the host's reply carries no model name the run records WithModelName,
// on both the accepted and the exhausted path.
func TestRunOnceModelNameFallback(t *testing.T) {
	cases := []struct {
		name       string
		content    func(domainID, memoryID string) string
		wantStatus string
	}{
		{"ok", func(d, h string) string { return supersedeOutput(d, h, "Run gofmt and tests.") }, "ok"},
		{"invalid_json", func(string, string) string { return "not json" }, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			domainID, memoryID := seedDomain(t, s)
			seedInbox(t, s, sampleMessages())
			m := &fakeModel{replies: []ModelReply{{Content: tc.content(domainID, memoryID), FinishReason: "stop"}}}
			w := NewWorker(s, m, WithModelName("configured-model"))
			res, err := w.RunOnce(context.Background(), params())
			if err != nil || res.Status != tc.wantStatus {
				t.Fatalf("RunOnce = %+v, %v; want %s", res, err, tc.wantStatus)
			}
			if run := lastRun(t, s); run.Status != tc.wantStatus || run.Model != "configured-model" {
				t.Fatalf("run = %+v, want status %s model configured-model", run, tc.wantStatus)
			}
		})
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
	if st := watermark(t, s); st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on host error, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("inbox after host error = %v, want the 2 meaningful messages kept", left)
	}
	run := lastRun(t, s)
	if run.Status != "error" || run.Error != "no memory model available" || run.Model != "test-model" {
		t.Fatalf("run = %+v, want status error with the host's message and the fallback model name", run)
	}
}

// A host error on a retry is recorded against the last model that answered,
// not the fallback name: the host's error reply names no model.
func TestRunOnceHostErrorAfterUnusableReplyNamesLastModel(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{
		replies: []ModelReply{{Content: "Sure!", FinishReason: "stop", Model: "model-a"}},
		errs:    []error{nil, errors.New("no model left to try")},
	}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err == nil || !strings.Contains(err.Error(), "no model left to try") {
		t.Fatalf("RunOnce err = %v, want the host's error wrapped", err)
	}
	if res.Status != "error" {
		t.Fatalf("status = %q, want error", res.Status)
	}
	if len(m.requests) != 2 || !slices.Equal(m.requests[1].Exclude, []string{"model-a"}) {
		t.Fatalf("requests = %+v, want 2 with model-a excluded on the second", m.requests)
	}
	run := lastRun(t, s)
	if run.Status != "error" || run.Model != "model-a" || run.Error != "no model left to try" {
		t.Fatalf("run = %+v, want status error, model model-a (last tried), the host's message", run)
	}
}

func TestRunOnceValidationAborted(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())

	// Evidence seq_end 99 is outside the batch [1,2] → Validate fails.
	raw := fmt.Sprintf(`{"domain_ops":[],"memory_ops":[{"op":"supersede","domain":%q,"old_id":%q,"type":"rule","text":"Out of range evidence.","confidence":0.9,"evidence":{"seq_start":1,"seq_end":99}}],"conflict_ledger":[]}`, domainID, memoryID)

	m := &fakeModel{raw: raw}
	w := NewWorker(s, m, WithModelName("test-model"))
	res, err := w.RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "aborted" || res.Applied != 0 {
		t.Fatalf("result = %+v, want aborted with nothing applied", res)
	}
	if len(m.requests) != 1 {
		t.Fatalf("model calls = %d, want 1 (a contract violation is not retried)", len(m.requests))
	}
	if st := watermark(t, s); st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on aborted, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("inbox after aborted = %v, want the 2 meaningful messages kept", left)
	}
	run := lastRun(t, s)
	if run.Status != "aborted" || run.Model != "fake-model" || !strings.Contains(run.Error, "outside batch") {
		t.Fatalf("run = %+v, want status aborted with the validation error", run)
	}
	old, err := s.GetMemory(context.Background(), s.DB(), memoryID)
	if err != nil || old.Status != store.StatusActive {
		t.Fatalf("seeded memory = %+v err=%v, want still active", old, err)
	}
}

// An output that passes Validate can still fail in the store: a create whose
// name collides with an existing active domain. The transaction rolls back,
// the run is recorded as error, and the inbox is kept for the next run.
func TestRunOnceApplyFailureRecordsError(t *testing.T) {
	s := openStore(t)
	seedDomain(t, s)
	seedInbox(t, s, sampleMessages())
	ctx := context.Background()
	before, err := s.ListDomains(ctx, s.DB())
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}

	raw := `{"domain_ops":[{"op":"create","tmp_id":"t1","name":"ClawEh","summary":"a second one","evidence":{"seq_start":1,"seq_end":2}}],"memory_ops":[],"conflict_ledger":[]}`
	w := NewWorker(s, &fakeModel{raw: raw})
	res, err := w.RunOnce(ctx, params())
	if err == nil || !errors.Is(err, store.ErrDuplicateName) {
		t.Fatalf("RunOnce err = %v, want ErrDuplicateName wrapped", err)
	}
	if res.Status != "error" || res.Applied != 0 {
		t.Fatalf("result = %+v, want error with nothing applied", res)
	}
	if st := watermark(t, s); st.ConsolidatedSeq != 0 {
		t.Fatalf("watermark advanced to %d on apply failure, want 0", st.ConsolidatedSeq)
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("inbox after apply failure = %v, want kept", left)
	}
	run := lastRun(t, s)
	if run.Status != "error" || !strings.Contains(run.Error, "already exists") {
		t.Fatalf("run = %+v, want status error with the store's message", run)
	}
	if run.OpsApplied != 0 {
		t.Fatalf("run.OpsApplied = %d after a rolled-back apply, want 0", run.OpsApplied)
	}
	after, err := s.ListDomains(ctx, s.DB())
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("domains = %d after a failed apply, want %d (rolled back)", len(after), len(before))
	}
	if events := listEvents(t, s); len(events) != 0 {
		t.Fatalf("events = %+v, want none after rollback", events)
	}
}

// Exact duplicates of an active memory are retired before the model sees the
// state, so it is never asked to tidy what the store can tidy itself.
func TestRunOnceDedupesBeforeModel(t *testing.T) {
	logs := installCapture(t)
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	backdate(t, s, memoryID, 1) // the original is clearly the earlier one
	dup := addMemory(t, s, domainID, store.TypeRule, "Run make test after changes.")
	seedInbox(t, s, sampleMessages())

	m := &fakeModel{raw: emptyOutput}
	res, err := NewWorker(s, m).RunOnce(context.Background(), params())
	if err != nil || res.Status != "ok" {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}

	e := logs.wait(t, "consolidation: retired duplicate memories")
	if e.level != "info" || e.fields["retired"] != 1 || e.fields["id"] != "alice" {
		t.Errorf("log = %+v, want info with retired=1 id=alice", e)
	}
	ctx := context.Background()
	got, err := s.GetMemory(ctx, s.DB(), dup.ID)
	if err != nil || got.Status != store.StatusRetired || got.RetireReason == nil || *got.RetireReason != "duplicate" {
		t.Fatalf("duplicate = %+v err=%v, want retired as duplicate", got, err)
	}
	orig, err := s.GetMemory(ctx, s.DB(), memoryID)
	if err != nil || orig.Status != store.StatusActive {
		t.Fatalf("original = %+v err=%v, want still active", orig, err)
	}
	dv := domainView(t, decodeInput(t, m.requests[0]), domainID)
	if len(dv.Memories) != 1 || dv.Memories[0].ID != memoryID {
		t.Fatalf("model saw memories %+v, want only the original", dv.Memories)
	}
}

func TestRunOnceIdleNoMessages(t *testing.T) {
	s := openStore(t)
	m := &fakeModel{raw: "{}"}
	res, err := NewWorker(s, m).RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "idle" || res.SeqStart != 1 {
		t.Fatalf("result = %+v, want idle from seq 1", res)
	}
	if m.calls() != 0 {
		t.Fatalf("model calls = %d, want 0", m.calls())
	}
	if _, ok, err := s.LastRun(context.Background(), s.DB()); err != nil || ok {
		t.Fatalf("run recorded for idle: ok=%v err=%v", ok, err)
	}
}

func TestRunOnceIdleAlreadyConsolidated(t *testing.T) {
	s := openStore(t)
	seedInbox(t, s, sampleMessages())
	// Watermark already past max seq.
	if err := s.SetWatermark(context.Background(), s.DB(), store.InboxStateKey, 3, 3); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	m := &fakeModel{raw: "{}"}
	res, err := NewWorker(s, m).RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "idle" || res.SeqStart != 4 {
		t.Fatalf("result = %+v, want idle from seq 4", res)
	}
	if m.calls() != 0 {
		t.Fatalf("model calls = %d, want 0", m.calls())
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
	m := &fakeModel{raw: "{}"}
	res, err := NewWorker(s, m).RunOnce(context.Background(), params())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Status != "busy" || res.SeqStart != 0 {
		t.Fatalf("result = %+v, want busy with no seq range", res)
	}
	if m.calls() != 0 {
		t.Fatalf("model calls = %d, want 0", m.calls())
	}
	if left := inboxSeqs(t, s); !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("inbox = %v, want untouched", left)
	}
}

func TestRunOnceDebugDump(t *testing.T) {
	s := openStore(t)
	domainID, memoryID := seedDomain(t, s)
	seedInbox(t, s, sampleMessages())
	dir := t.TempDir()

	raw := supersedeOutput(domainID, memoryID, "Run gofmt and tests.")
	w := NewWorker(s, &fakeModel{raw: raw}, WithDebugDump(dir))
	if _, err := w.RunOnce(context.Background(), params()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("debug dump files = %d, want 1", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var rec struct {
		System   string `json:"system"`
		UserJSON string `json:"user_json"`
		Raw      string `json:"raw"`
		Applied  int    `json:"applied"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("dump is not JSON: %v", err)
	}
	if rec.System != DefaultPrompt() {
		t.Error("dump system != the prompt sent")
	}
	if rec.Raw != raw {
		t.Errorf("dump raw = %q, want the model's reply verbatim", rec.Raw)
	}
	if rec.Applied != 1 {
		t.Errorf("dump applied = %d, want 1", rec.Applied)
	}
	in := decodeInput(t, ModelRequest{User: rec.UserJSON})
	if !slices.Equal(in.NewMessages, meaningfulMessages()) {
		t.Errorf("dump user_json new_messages = %+v, want %+v", in.NewMessages, meaningfulMessages())
	}
}
