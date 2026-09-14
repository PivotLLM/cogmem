// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

func sampleInput() Input {
	return Input{
		CurrentState: CurrentState{Domains: []DomainView{{
			ID: "d4", Name: "Layout", Version: 2,
			Memories: []MemoryView{{ID: "h9", Type: "rule", Text: "Never use the color blue.", Confidence: 0.9}},
		}}},
		NewMessages: []Message{{Seq: 512, Role: "user", Text: "Actually, use blue for the layout."}},
	}
}

func TestValidateHappyPath(t *testing.T) {
	in := sampleInput()
	out := Output{
		MemoryOps: []MemoryOp{{
			Op: "supersede", OldID: "h9", Domain: "d4", Type: "rule",
			Text: "Use blue for the layout.", Confidence: 0.95, Evidence: ev(512, 512),
		}},
		ConflictLedger: []LedgerEntry{{Resolved: "x", Reason: "y", Evidence: ev(512, 512)}},
	}
	if err := out.Validate(in); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	in := sampleInput()
	cases := []struct {
		name    string
		out     Output
		wantErr string
	}{
		{"evidence out of range", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: "ok", Evidence: ev(999, 999)}}},
			"memory_ops[0]: evidence [999,999] outside batch [512,512]"},
		{"evidence reversed", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: "ok", Evidence: ev(512, 511)}}},
			"memory_ops[0]: evidence seq_start 512 > seq_end 511"},
		{"unknown domain", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "dX", Type: "fact", Text: "ok", Evidence: ev(512, 512)}}},
			`memory_ops[0]: unknown domain "dX"`},
		{"unknown retire id", Output{MemoryOps: []MemoryOp{{Op: "retire", ID: "hZ", Reason: "x", Evidence: ev(512, 512)}}},
			`memory_ops[0]: retire unknown memory "hZ"`},
		{"unknown supersede old_id", Output{MemoryOps: []MemoryOp{{Op: "supersede", OldID: "hZ", Domain: "d4", Type: "rule", Text: "ok", Evidence: ev(512, 512)}}},
			`memory_ops[0]: supersede unknown old_id "hZ"`},
		{"invalid kind", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "bogus", Text: "ok", Evidence: ev(512, 512)}}},
			`memory_ops[0]: invalid type "bogus"`},
		{"invalid memory op", Output{MemoryOps: []MemoryOp{{Op: "delete", ID: "h9", Evidence: ev(512, 512)}}},
			`memory_ops[0]: invalid op "delete"`},
		{"create no tmp_id", Output{DomainOps: []DomainOp{{Op: "create", Name: "X", Evidence: ev(512, 512)}}},
			"domain_ops[0]: create needs a unique tmp_id"},
		{"create duplicate tmp_id", Output{DomainOps: []DomainOp{
			{Op: "create", TmpID: "t1", Name: "X", Evidence: ev(512, 512)},
			{Op: "create", TmpID: "t1", Name: "Y", Evidence: ev(512, 512)},
		}}, "domain_ops[1]: create needs a unique tmp_id"},
		{"create no name", Output{DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "  ", Evidence: ev(512, 512)}}},
			"domain_ops[0]: create needs a name"},
		{"update unknown domain", Output{DomainOps: []DomainOp{{Op: "update", ID: "dX", Summary: "s", Evidence: ev(512, 512)}}},
			`domain_ops[0]: update unknown domain "dX"`},
		{"archive unknown domain", Output{DomainOps: []DomainOp{{Op: "archive", ID: "dX", Evidence: ev(512, 512)}}},
			`domain_ops[0]: archive unknown domain "dX"`},
		{"invalid domain op", Output{DomainOps: []DomainOp{{Op: "delete", ID: "d4", Evidence: ev(512, 512)}}},
			`domain_ops[0]: invalid op "delete"`},
		{"triggers too long", Output{DomainOps: []DomainOp{{Op: "update", ID: "d4", Triggers: strings.Repeat("x", maxTriggersLen+1), Evidence: ev(512, 512)}}},
			"domain_ops[0]: triggers too long (513 > 512)"},
		{"keyword triggers too long", Output{DomainOps: []DomainOp{{Op: "update", ID: "d4", KeywordTriggers: strings.Repeat("x", maxTriggersLen+1), Evidence: ev(512, 512)}}},
			"domain_ops[0]: keyword_triggers too long (513 > 512)"},
		// Type is REQUIRED, not merely valid-if-present. The old contract
		// checked each field only when it was non-empty, so an op that named no
		// type and no status and no source passed every guard and was then
		// filled in with defaults — assistant_inferred + active, the one
		// combination the rules forbade.
		{"missing type", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Text: "ok", Evidence: ev(512, 512)}}},
			"memory_ops[0]: missing type"},
		{"empty text", Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: " ", Evidence: ev(512, 512)}}},
			"memory_ops[0]: empty text"},
		// review was a memory status and never a domain one; the domain
		// lifecycle is active/archived.
		{"domain status review", Output{DomainOps: []DomainOp{{
			Op: "create", TmpID: "t1", Name: "X", Status: "review", Evidence: ev(512, 512),
		}}}, `domain_ops[0]: invalid status "review"`},
		{"ledger evidence out of range", Output{ConflictLedger: []LedgerEntry{{Resolved: "x", Reason: "y", Evidence: ev(999, 999)}}},
			"conflict_ledger[0]: evidence [999,999] outside batch [512,512]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.out.Validate(in)
			if err == nil {
				t.Fatalf("expected rejection %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// With no messages in the batch there is no range any evidence can fall in.
func TestValidateRejectsEvidenceWithEmptyBatch(t *testing.T) {
	in := sampleInput()
	in.NewMessages = nil
	out := Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: "ok", Evidence: ev(1, 1)}}}
	err := out.Validate(in)
	if err == nil || !strings.Contains(err.Error(), "outside batch [0,0]") {
		t.Fatalf("err = %v, want evidence rejected against an empty batch", err)
	}
}

// The two types added with the redesign must be accepted: event (never loaded
// into the prompt) and operational (the assistant's own housekeeping).
func TestValidateAcceptsEventAndOperational(t *testing.T) {
	in := sampleInput()
	for _, typ := range []string{"fact", "preference", "rule", "event", "operational"} {
		out := Output{MemoryOps: []MemoryOp{{
			Op: "add", Domain: "d4", Type: typ, Text: "ok",
			Confidence: 0.9, Evidence: ev(512, 512),
		}}}
		if err := out.Validate(in); err != nil {
			t.Errorf("type %q rejected: %v", typ, err)
		}
	}
}

func TestValidateTmpIDReference(t *testing.T) {
	in := sampleInput()
	out := Output{
		DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "New", Evidence: ev(512, 512)}},
		MemoryOps: []MemoryOp{{Op: "add", Domain: "t1", Type: "fact", Text: "a fact", Evidence: ev(512, 512)}},
	}
	if err := out.Validate(in); err != nil {
		t.Fatalf("tmp_id reference rejected: %v", err)
	}
}

// TestValidateUpdateIsPatch confirms a domain update no longer requires a version
// (the patch model dropped expected_version) and accepts an optional sticky flag.
func TestValidateUpdateIsPatch(t *testing.T) {
	in := sampleInput()
	yes := true
	out := Output{
		DomainOps: []DomainOp{{Op: "update", ID: "d4", Sticky: &yes, Summary: "new", Evidence: ev(512, 512)}},
	}
	if err := out.Validate(in); err != nil {
		t.Fatalf("versionless update patch rejected: %v", err)
	}
}

func TestSelectBatchCountCap(t *testing.T) {
	msgs := make([]Message, 10)
	for i := range msgs {
		msgs[i] = Message{Seq: int64(i + 1), Role: "user", Text: "hello"}
	}
	batch, last, more := SelectBatch(msgs, BatchOptions{MaxMessages: 4, MaxInputTokens: 100000})
	if len(batch) != 4 || last != 4 || !more {
		t.Fatalf("count cap: len=%d last=%d more=%v", len(batch), last, more)
	}
}

func TestSelectBatchTokenCapAndTruncate(t *testing.T) {
	big := strings.Repeat("x", 1000)
	msgs := []Message{
		{Seq: 1, Role: "user", Text: big},
		{Seq: 2, Role: "user", Text: big},
		{Seq: 3, Role: "user", Text: big},
	}
	// ~250 tokens per message; budget fits ~2.
	batch, last, more := SelectBatch(msgs, BatchOptions{MaxMessages: 100, MaxInputTokens: 520, PerMessageChars: 100000})
	if len(batch) != 2 || last != 2 || !more {
		t.Fatalf("token cap: len=%d last=%d more=%v", len(batch), last, more)
	}
	// Truncation marker.
	if _, cut := TruncateText(big, 100); !cut {
		t.Fatalf("expected truncation")
	}
	out, _ := TruncateText(big, 100)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("missing truncation marker")
	}
}

func TestSelectBatchAlwaysProgresses(t *testing.T) {
	// Single oversized message must still be returned (progress guarantee).
	msgs := []Message{{Seq: 1, Role: "user", Text: strings.Repeat("y", 100000)}}
	batch, last, more := SelectBatch(msgs, BatchOptions{MaxMessages: 10, MaxInputTokens: 10, PerMessageChars: 100})
	if len(batch) != 1 || last != 1 || more {
		t.Fatalf("progress: len=%d last=%d more=%v", len(batch), last, more)
	}
}

// The contract is embedded and cannot be replaced. Per-agent instructions are
// APPENDED to it, so a change to the schema or the core rules reaches every
// agent at once instead of stopping at whatever version each workspace happened
// to be seeded with.
func TestBuildPromptAppendsAgentInstructions(t *testing.T) {
	// No file: the contract alone.
	got, res := BuildPrompt("")
	if got != DefaultPrompt() || res.Appended || res.Ignored {
		t.Fatalf("empty path: appended=%v ignored=%v", res.Appended, res.Ignored)
	}

	// A missing file is not an error either.
	got, res = BuildPrompt(filepath.Join(t.TempDir(), "missing.md"))
	if got != DefaultPrompt() || res.Appended {
		t.Fatalf("missing file: appended=%v", res.Appended)
	}

	// Real instructions are added, and the contract survives intact.
	f := filepath.Join(t.TempDir(), "COGMEM.md")
	if err := os.WriteFile(f, []byte("Never record anything about medical matters."), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, res = BuildPrompt(f)
	if !res.Appended {
		t.Error("instructions were not appended")
	}
	if !strings.Contains(got, "Never record anything about medical matters.") {
		t.Error("the agent's instructions are missing from the prompt")
	}
	if !strings.Contains(got, "# OUTPUT SCHEMA") || !strings.Contains(got, "# CORE RULES") {
		t.Error("the contract was lost when instructions were appended")
	}
	if !strings.Contains(got, "the contract wins") {
		t.Error("precedence is not stated, so a conflicting instruction has no resolution")
	}

	// Whitespace only is the same as nothing.
	blank := filepath.Join(t.TempDir(), "COGMEM.md")
	if err := os.WriteFile(blank, []byte("\n  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, res := BuildPrompt(blank); res.Appended {
		t.Error("a whitespace-only file was treated as instructions")
	}
}

// Every workspace was seeded with a copy of the engine prompt. Appending one
// would give the model the whole old prompt — including a second, contradictory
// output schema — bolted onto the current one, which is worse than the
// wholesale override this replaces. Such a file is ignored, and the caller is
// told why so an operator can act on it.
func TestBuildPromptIgnoresACopyOfTheEnginePrompt(t *testing.T) {
	f := filepath.Join(t.TempDir(), "COGMEM.md")
	if err := os.WriteFile(f, []byte(DefaultPrompt()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, res := BuildPrompt(f)
	if !res.Ignored {
		t.Fatal("a copy of the engine prompt was appended rather than ignored")
	}
	if res.Appended {
		t.Error("Ignored and Appended are both set")
	}
	if res.Reason == "" {
		t.Error("no reason given, so the warning cannot say what to do about it")
	}
	if got != DefaultPrompt() {
		t.Error("the prompt was altered even though the file was ignored")
	}
	// Only ONE schema section, so the model is never shown two contracts.
	if n := strings.Count(got, "# OUTPUT SCHEMA"); n != 1 {
		t.Errorf("prompt contains %d output schemas, want 1", n)
	}

	// An edited copy that still carries the schema is caught the same way: what
	// makes it unusable is the duplicated contract, not being byte-identical.
	edited := filepath.Join(t.TempDir(), "COGMEM.md")
	if err := os.WriteFile(edited,
		[]byte("Be sparing.\n"+DefaultPrompt()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, res := BuildPrompt(edited); !res.Ignored {
		t.Error("an edited copy carrying the schema was appended")
	}
}

func ev(a, b int64) store.Evidence { return store.Evidence{SeqStart: a, SeqEnd: b} }

// Normalize no longer repairs anything. The one repair it had downgraded an
// inferred memory the model marked active to review, and both the status field
// and the review state are gone — the model states type and nothing else, so
// there is no longer a field it can set to a value needing correction.
//
// The function survives because the repair-and-note path is the right shape for
// the next safely-correctable deviation, and its callers already handle an empty
// result. This test pins that it reports no notes and mutates nothing.
func TestOutput_Normalize_IsANoOp(t *testing.T) {
	out := Output{MemoryOps: []MemoryOp{
		{Op: "add", Domain: "d1", Type: "fact", Text: "a"},
		{Op: "add", Domain: "d1", Type: "event", Text: "b"},
		{Op: "supersede", OldID: "h1", Domain: "d1", Type: "operational", Text: "c"},
	}}
	before := append([]MemoryOp(nil), out.MemoryOps...)

	if notes := out.Normalize(); len(notes) != 0 {
		t.Errorf("Normalize reported %d notes, want none: %v", len(notes), notes)
	}
	for i := range before {
		if out.MemoryOps[i] != before[i] {
			t.Errorf("memory_ops[%d] was mutated: %+v -> %+v", i, before[i], out.MemoryOps[i])
		}
	}
}

// A retire may carry no evidence, because it removes a memory that already
// exists rather than asserting anything a message would have to justify.
//
// This is what makes housekeeping possible at all: merging two memories that
// say the same thing, or dropping one a newer memory contradicts, is tidying
// the current conversation never raised and so cannot cite. Requiring evidence
// there made every such op invalid — and one invalid op rejects the whole
// payload, so an agent following the rule would have aborted entire runs.
func TestRetireMayOmitEvidence(t *testing.T) {
	in := sampleInput()
	out := Output{MemoryOps: []MemoryOp{
		{Op: "retire", ID: "h9", Reason: "duplicate of h31"},
	}}
	if err := out.Validate(in); err != nil {
		t.Fatalf("retire without evidence rejected: %v", err)
	}
}

// Evidence given on a retire is still checked: omitting it is allowed, but
// pointing at a range outside the batch is a mistake either way.
func TestRetireWithBadEvidenceIsStillRejected(t *testing.T) {
	in := sampleInput()
	out := Output{MemoryOps: []MemoryOp{
		{Op: "retire", ID: "h9", Reason: "x", Evidence: ev(999, 999)},
	}}
	if err := out.Validate(in); err == nil {
		t.Error("out-of-range evidence on a retire was accepted")
	}
}

// The exemption is for retire ONLY. An add or supersede writes text, which is
// exactly what the evidence rule exists to keep anchored to a real message.
func TestAddAndSupersedeStillRequireEvidence(t *testing.T) {
	in := sampleInput()
	for _, op := range []MemoryOp{
		{Op: "add", Domain: "d4", Type: "fact", Text: "invented"},
		{Op: "supersede", OldID: "h9", Domain: "d4", Type: "rule", Text: "invented"},
	} {
		out := Output{MemoryOps: []MemoryOp{op}}
		if err := out.Validate(in); err == nil {
			t.Errorf("%s without evidence was accepted", op.Op)
		}
	}
}

// A retire still has to name a memory that exists — the id check is what makes
// dropping the evidence requirement safe.
func TestRetireStillNeedsAKnownID(t *testing.T) {
	in := sampleInput()
	out := Output{MemoryOps: []MemoryOp{{Op: "retire", ID: "hNOPE", Reason: "x"}}}
	if err := out.Validate(in); err == nil {
		t.Error("retire of an unknown memory was accepted")
	}
}

// The shipped prompt must actually ask for the housekeeping, and must ask for
// it the safe way: retiring duplicates rather than rewriting several distinct
// memories into one vaguer summary.
func TestDefaultPromptAsksForHousekeeping(t *testing.T) {
	p := DefaultPrompt()
	for _, want := range []string{
		"Tidy the domains you touch",
		"may omit `evidence`",
		"Retire; do not rewrite",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

// A conflict-ledger entry may omit evidence, for the same reason a retire may:
// it records a decision rather than asserting a memory, and a housekeeping
// resolution has no message in the batch to cite.
//
// This is not hypothetical. Rule 9 asked a live agent to tidy a domain; it did,
// filed the resolution in the ledger as instructed, and the whole payload was
// rejected for `evidence [0,0] outside batch` — losing the entire consolidation
// run rather than the single entry.
func TestConflictLedgerMayOmitEvidence(t *testing.T) {
	in := sampleInput()
	out := Output{
		MemoryOps: []MemoryOp{{Op: "retire", ID: "h9", Reason: "contradicted by h31"}},
		ConflictLedger: []LedgerEntry{{
			Resolved: "retired h9; h31 is the current instruction",
			Reason:   "h31 is newer and says the opposite",
		}},
	}
	if err := out.Validate(in); err != nil {
		t.Fatalf("housekeeping resolution rejected: %v", err)
	}
}

// Evidence given on a ledger entry is still checked.
func TestConflictLedgerWithBadEvidenceIsStillRejected(t *testing.T) {
	in := sampleInput()
	out := Output{ConflictLedger: []LedgerEntry{
		{Resolved: "x", Reason: "y", Evidence: ev(999, 999)},
	}}
	if err := out.Validate(in); err == nil {
		t.Error("out-of-range ledger evidence was accepted")
	}
}

// The model must be able to tell which of two contradicting memories is newer.
//
// current_state carried no time at all, so rule 3 — a newer instruction
// overrides an older one — was unenforceable against stored memories. Asked to
// resolve a contradiction between two rules, a live agent deduplicated
// correctly and then, rightly, declined to guess which conflicting instruction
// was current. age_days is what it was missing.
func TestPromptExplainsAgeDays(t *testing.T) {
	p := DefaultPrompt()
	for _, want := range []string{
		"`age_days`",
		"listed oldest first",
		"smaller `age_days` is the",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt does not explain %q", want)
		}
	}
}

// Days, not a timestamp: the question is which is newer, and a small integer
// answers it without the model doing date arithmetic.
func TestMemoryViewCarriesAgeInDays(t *testing.T) {
	b, err := json.Marshal(MemoryView{ID: "h1", Type: "rule", Text: "x", Confidence: 0.9, AgeDays: 180})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"age_days":180`) {
		t.Errorf("age_days missing from the wire format: %s", got)
	}
	for _, unwanted := range []string{"created", "timestamp", "T00:00"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("view carries %q; days were chosen over a timestamp: %s", unwanted, got)
		}
	}
}
