// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/consolidate"
	"github.com/PivotLLM/cogmem/store"
)

// e2eModel is the host's ModelCaller: it records every request and answers
// with one fixed consolidation Output. It closes done once it has replied so
// the test can wait for the run without sleeping.
type e2eModel struct {
	generalID string
	done      chan struct{}
	once      sync.Once

	mu   sync.Mutex
	reqs []consolidate.ModelRequest
}

func (m *e2eModel) Complete(_ context.Context, req consolidate.ModelRequest) (consolidate.ModelReply, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	defer m.once.Do(func() { close(m.done) })
	// No Model in the reply: the run must fall back to WithModelName.
	return consolidate.ModelReply{Content: e2eReply(m.generalID), FinishReason: "stop"}, nil
}

func (m *e2eModel) requests() []consolidate.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]consolidate.ModelRequest(nil), m.reqs...)
}

const (
	e2eDomainName   = "Deploy Pipeline"
	e2eSummary      = "Steps for shipping builds"
	e2eToolTrigger  = "deploytool"
	e2eKeyword      = "release train"
	e2eTopicMemory  = "Deploys go through the staging cluster first."
	e2eStickyMemory = "Confirm before running a production deploy."
	e2eReason       = "recurring deploy discussion"
)

// e2eReply is the model's Output: a new topic domain with both trigger kinds,
// a memory in it via the tmp_id, and a sticky rule in General.
func e2eReply(generalID string) string {
	return fmt.Sprintf(`{
  "domain_ops": [{
    "op": "create", "tmp_id": "tmp_deploy", "name": %q, "summary": %q,
    "sticky": false, "triggers": %q, "keyword_triggers": %q, "reason": %q,
    "evidence": {"seq_start": 1, "seq_end": 5}
  }],
  "memory_ops": [
    {"op": "add", "domain": "tmp_deploy", "type": "fact", "text": %q,
     "confidence": 0.9, "evidence": {"seq_start": 1, "seq_end": 2}},
    {"op": "add", "domain": %q, "type": "rule", "text": %q,
     "confidence": 0.95, "evidence": {"seq_start": 4, "seq_end": 5}}
  ],
  "conflict_ledger": []
}`, e2eDomainName, e2eSummary, e2eToolTrigger, e2eKeyword, e2eReason,
		e2eTopicMemory, generalID, e2eStickyMemory)
}

// TestEndToEnd_ObserveConsolidateRecall wires the public surfaces together the
// way a host does: a Session observes messages and nudges a Manager, whose
// WorkerFactory opens its own store handle and runs the worker against a fake
// model; the memories the run writes are then recalled through the Session.
func TestEndToEnd_ObserveConsolidateRecall(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	dir := filepath.Join(ws, "cogmem")

	fake := &e2eModel{done: make(chan struct{})}

	var (
		handlesMu sync.Mutex
		handles   []*store.Store
		jobs      []consolidate.Job
	)
	t.Cleanup(func() {
		handlesMu.Lock()
		defer handlesMu.Unlock()
		for _, h := range handles {
			_ = h.Close()
		}
	})
	factory := func(job consolidate.Job) (*consolidate.Worker, error) {
		st, err := store.Open(store.DBPath(job.Dir))
		if err != nil {
			return nil, err
		}
		handlesMu.Lock()
		handles = append(handles, st)
		jobs = append(jobs, job)
		handlesMu.Unlock()
		return consolidate.NewWorker(st, fake, consolidate.WithModelName("e2e-model")), nil
	}
	m := consolidate.NewManager(factory, consolidate.WithEveryNMessages(4))
	m.Start(ctx)
	t.Cleanup(m.Stop)

	sess := NewSession(SessionOptions{
		ID: "e2e", Dir: dir, Workspace: ws, Manager: m,
		Settings: Settings{Prompt: PromptSettings{TopKDomains: 1}},
	})
	defer sess.Close()
	st := sess.Store()
	if st == nil {
		t.Fatal("session store did not open")
	}
	general := mustGeneral(t, st)
	fake.generalID = general.ID
	// A pre-existing topic domain that recency would pick; it lets each Recall
	// below prove a real signal chose the new domain rather than filler.
	decoy := mustDomain(t, st, store.CreateDomainParams{Name: "Decoy", Summary: "nothing to see"})

	// Four meaningful messages with tool plumbing in the middle; the fourth
	// meaningful one (seq 5) reaches the threshold.
	observed := []consolidate.Message{
		{Seq: 1, Role: "user", Text: "We are setting up the deploy pipeline for the new service."},
		{Seq: 2, Role: "assistant", Text: "Understood. Deploys will go through staging first."},
		{Seq: 4, Role: "user", Text: "Always confirm with me before a production deploy."},
		{Seq: 5, Role: "assistant", Text: "Noted, I will confirm before any production deploy."},
	}
	sess.Observe(ctx, 1, observed[0].Role, observed[0].Text)
	sess.Observe(ctx, 2, observed[1].Role, observed[1].Text)
	sess.Observe(ctx, 3, "tool", `{"result":"ok"}`)
	sess.Observe(ctx, 4, observed[2].Role, observed[2].Text)
	sess.Observe(ctx, 5, observed[3].Role, observed[3].Text)

	// --- wait for the run: the model replied, then the run record landed ---
	select {
	case <-fake.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the consolidation model was never called")
	}
	var run store.Run
	for deadline := time.Now().Add(15 * time.Second); ; {
		r, ok, err := st.LastRun(ctx, st.DB())
		if err != nil {
			t.Fatalf("last run: %v", err)
		}
		if ok {
			run = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// --- the run ---
	if run.Status != "ok" || run.Trigger != "message" || run.Model != "e2e-model" {
		t.Fatalf("run = %+v, want status ok, trigger message, model e2e-model", run)
	}
	if run.OpsApplied != 3 || run.SeqStart != 1 || run.SeqEnd != 5 || run.Error != "" {
		t.Fatalf("run = %+v, want 3 ops over seqs 1..5 with no error", run)
	}
	handlesMu.Lock()
	gotJobs := append([]consolidate.Job(nil), jobs...)
	handlesMu.Unlock()
	wantJob := consolidate.Job{ID: "e2e", Dir: dir, Workspace: ws}
	if len(gotJobs) != 1 || gotJobs[0] != wantJob {
		t.Fatalf("factory jobs = %+v, want exactly %+v", gotJobs, wantJob)
	}
	if n, err := st.InboxCount(ctx, st.DB()); err != nil || n != 0 {
		t.Fatalf("inbox count = %d err=%v, want drained", n, err)
	}
	state, err := st.GetState(ctx, st.DB(), store.InboxStateKey)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.ConsolidatedSeq != 5 || state.LastSeenSeq != 5 {
		t.Fatalf("watermark = %+v, want consolidated 5, last seen 5", state)
	}

	// --- the model saw exactly the meaningful messages ---
	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("model called %d times, want 1", len(reqs))
	}
	req := reqs[0]
	if !req.JSONObject || req.System == "" || len(req.Exclude) != 0 {
		t.Fatalf("request = {JSONObject:%v System:%d bytes Exclude:%v}", req.JSONObject, len(req.System), req.Exclude)
	}
	var in consolidate.Input
	if err := json.Unmarshal([]byte(req.User), &in); err != nil {
		t.Fatalf("user payload is not an Input: %v\n%s", err, req.User)
	}
	if !reflect.DeepEqual(in.NewMessages, observed) {
		t.Fatalf("new_messages = %+v, want %+v", in.NewMessages, observed)
	}
	var sawGeneral, sawDecoy bool
	for _, d := range in.CurrentState.Domains {
		switch d.ID {
		case general.ID:
			sawGeneral = d.Sticky
		case decoy.ID:
			sawDecoy = !d.Sticky
		}
	}
	if !sawGeneral || !sawDecoy || len(in.CurrentState.Domains) != 2 {
		t.Fatalf("current_state should show sticky General and topic Decoy: %+v", in.CurrentState.Domains)
	}

	// --- what the run wrote ---
	deploy, err := st.DomainByName(ctx, st.DB(), e2eDomainName)
	if err != nil {
		t.Fatalf("new domain: %v", err)
	}
	if deploy.Sticky() || deploy.Triggers != e2eToolTrigger || deploy.KeywordTriggers != e2eKeyword || deploy.Summary != e2eSummary {
		t.Fatalf("new domain = %+v", deploy)
	}
	topicMems, err := st.ListMemories(ctx, st.DB(), deploy.ID, store.StatusActive)
	if err != nil || len(topicMems) != 1 {
		t.Fatalf("topic memories = %+v err=%v, want one", topicMems, err)
	}
	if tm := topicMems[0]; tm.Text != e2eTopicMemory || tm.Origin != store.OriginConsolidation || tm.Type != store.TypeFact ||
		tm.SourceSeqStart == nil || *tm.SourceSeqStart != 1 || tm.SourceSeqEnd == nil || *tm.SourceSeqEnd != 2 {
		t.Fatalf("topic memory = %+v", tm)
	}
	topicMem := topicMems[0]

	events, err := st.ListEvents(ctx, st.DB())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var created *store.Event
	var sleepCycle int
	for i := range events {
		e := events[i]
		if e.Actor == "sleep_cycle" {
			sleepCycle++
		}
		if e.Type == "create" && e.DomainID == deploy.ID && e.MemoryID == "" {
			created = &events[i]
		}
	}
	if created == nil {
		t.Fatalf("no create event for domain %s in %+v", deploy.ID, events)
	}
	if created.Actor != "sleep_cycle" || created.Model != "e2e-model" || created.Reason != e2eReason ||
		created.Evidence != `{"seq_start":1,"seq_end":5}` {
		t.Fatalf("create event = %+v", *created)
	}
	if sleepCycle != 3 {
		t.Fatalf("%d sleep_cycle events, want one per applied op (3)", sleepCycle)
	}

	// --- recall through the same session ---
	// The run made the new domain the most recently active topic; age it so
	// the decoy is the recency pick and only a signal can route the new one.
	ageDomain(t, st, deploy.ID)
	mustTouch(t, st, decoy.ID)

	// (a) A plain message: the sticky rule and the topic index are in the
	// system block; the routed slot goes to the decoy by recency.
	inj := sess.Recall(ctx, "hello")
	stable, ns := injectionText(inj, PlaceSystemStable)
	routed, nr := injectionText(inj, PlaceCurrentUser)
	if ns != 1 || nr != 1 {
		t.Fatalf("injections = %+v, want one stable and one routed", inj)
	}
	for _, want := range []string{
		"COGMEM domain General is sticky:\n\n- (rule) " + e2eStickyMemory + " [origin: consolidation]\n",
		"## Topics (index)\n",
		"- " + deploy.ID + " · " + e2eDomainName + " — " + e2eSummary + "\n",
	} {
		if !strings.Contains(stable+"\n", want) {
			t.Fatalf("stable block missing %q:\n%s", want, stable)
		}
	}
	if !strings.Contains(routed, "## Active Context: "+decoy.ID+" · Decoy\n") || strings.Contains(routed, e2eTopicMemory) {
		t.Fatalf("with no signal, recency should route the decoy, not the new domain:\n%s", routed)
	}

	// (b) A tool whose name contains the trigger routes the new domain.
	sess.RecordToolUse("mcp_" + e2eToolTrigger + "_something")
	routed, nr = injectionText(sess.Recall(ctx, "unrelated"), PlaceCurrentUser)
	wantRouted := "## Active Context: " + deploy.ID + " · " + e2eDomainName + "\n" +
		"Summary: " + e2eSummary + "\n" +
		"- (" + topicMem.ID + ") (fact) " + e2eTopicMemory + " [origin: consolidation]"
	if nr != 1 || routed != wantRouted {
		t.Fatalf("tool-triggered routed block (n=%d):\n%q\nwant:\n%q", nr, routed, wantRouted)
	}

	// (c) A fresh session on the same directory, no tools used: the keyword
	// phrase in the message routes the new domain.
	ageDomain(t, st, deploy.ID) // the tool match above touched it
	mustTouch(t, st, decoy.ID)
	fresh := NewSession(SessionOptions{
		ID: "e2e-fresh", Dir: dir, Workspace: ws,
		Settings: Settings{Prompt: PromptSettings{TopKDomains: 1}},
	})
	defer fresh.Close()
	routed, nr = injectionText(fresh.Recall(ctx, "Is the "+e2eKeyword+" on schedule?"), PlaceCurrentUser)
	if nr != 1 || routed != wantRouted {
		t.Fatalf("keyword-triggered routed block (n=%d):\n%q\nwant:\n%q", nr, routed, wantRouted)
	}
	ageDomain(t, st, deploy.ID) // the keyword match touched it too
	mustTouch(t, st, decoy.ID)
	routed, _ = injectionText(fresh.Recall(ctx, "hello again"), PlaceCurrentUser)
	if strings.Contains(routed, e2eTopicMemory) {
		t.Fatalf("without the phrase the new domain must not route:\n%s", routed)
	}
}
