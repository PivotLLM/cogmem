// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// fakeLoader serves attachments from an in-memory map, recording every call so
// tests can assert a shared document is loaded exactly once.
type fakeLoader struct {
	files map[string]string
	calls []string
}

func (f *fakeLoader) load(ref string, maxBytes int) (Attachment, error) {
	f.calls = append(f.calls, ref)
	body, ok := f.files[ref]
	if !ok {
		return Attachment{}, errors.New("access denied: outside the agent's readable paths")
	}
	att := Attachment{Ref: ref, Content: body, Size: int64(len(body))}
	if maxBytes > 0 && len(body) > maxBytes {
		att.Content = body[:maxBytes]
		att.Truncated = true
	}
	return att, nil
}

func composeWith(t *testing.T, s *store.Store, opts ...Option) Result {
	t.Helper()
	res, err := New(s, opts...).Compose(context.Background(), RouteRequest{})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return res
}

func TestAttachmentFromStickyMemory(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	m := mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Write in my voice.",
		FileRef: "files/voice.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "# Voice\n\nShort sentences.\n"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	// The memory line names no file — the path appears once, with its content.
	if strings.Contains(res.Stable, "files/voice.md") {
		t.Fatalf("memory line should carry no file marker:\n%s", res.Stable)
	}
	want := "# Attached Documents\n\n" +
		"Full contents of files attached to memories currently in context, as of this turn. " +
		"Treat them as authoritative reference material for the memory that names them.\n\n" +
		"### Attached: files/voice.md\n" +
		"From memory " + m.ID + " (\"Write in my voice.\"), 26 bytes, current as of this turn.\n\n" +
		"# Voice\n\nShort sentences."
	if res.Attachments != want {
		t.Fatalf("attachments block:\n%s\nwant:\n%s", res.Attachments, want)
	}
}

func TestAttachmentFromRoutedDomain(t *testing.T) {
	s := newStore(t)
	d := mustDomain(t, s, store.CreateDomainParams{Name: "Writing"})
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeRule, Text: "Voice guide.",
		FileRef: "maestro/style.md",
	})

	fl := &fakeLoader{files: map[string]string{"maestro/style.md": "mount-sourced doc"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	if strings.Contains(res.Routed, "maestro/style.md") {
		t.Fatalf("routed memory line should carry no file marker:\n%s", res.Routed)
	}
	// A routed domain's document belongs to the ROUTED partition: the two blocks
	// travel to different places in the request, and the document header cites
	// the memory's id, so it has to sit with that memory.
	if !strings.Contains(res.RoutedAttachments, "### Attached: maestro/style.md") {
		t.Fatalf("routed attachments missing header:\n%s", res.RoutedAttachments)
	}
	if !strings.Contains(res.RoutedAttachments, "mount-sourced doc") {
		t.Fatalf("routed attachments missing mount content:\n%s", res.RoutedAttachments)
	}
	if res.Attachments != "" {
		t.Fatalf("no sticky memory owns a file, so the stable partition should be empty:\n%s", res.Attachments)
	}
}

func TestAttachmentDedupedAcrossMemories(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	var ids []string
	for i := 0; i < 3; i++ {
		m := mustMemory(t, s, store.AddMemoryParams{
			DomainID: gen.ID, Type: store.TypeFact, Text: fmt.Sprintf("note %d", i),
			FileRef: "files/voice.md",
		})
		ids = append(ids, m.ID)
	}

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "BODY"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	if len(fl.calls) != 1 {
		t.Fatalf("expected one load for a shared document, got %v", fl.calls)
	}
	if n := strings.Count(res.Attachments, "BODY"); n != 1 {
		t.Fatalf("expected document injected once, got %d copies:\n%s", n, res.Attachments)
	}
	// Several owners: provenance lists every id, sorted, and quotes no text.
	if !strings.Contains(res.Attachments, "From memories ") {
		t.Fatalf("multi-owner provenance missing:\n%s", res.Attachments)
	}
	for _, id := range ids {
		if !strings.Contains(res.Attachments, id) {
			t.Fatalf("provenance should name owner %s:\n%s", id, res.Attachments)
		}
	}
}

func TestAttachmentTruncationIsAnnounced(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "big doc",
		FileRef: "files/big.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/big.md": strings.Repeat("x", 100)}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithFileMaxBytes(10))

	if !strings.Contains(res.Attachments, "[TRUNCATED: showing the first 10 of 100 bytes. The rest of this document is NOT below — read the file directly if you need it.]\n\n"+strings.Repeat("x", 10)) {
		t.Fatalf("expected truncation notice followed by the 10-byte prefix:\n%s", res.Attachments)
	}
	if strings.Contains(res.Attachments, strings.Repeat("x", 11)) {
		t.Fatalf("more than the cap was injected:\n%s", res.Attachments)
	}
}

func TestAttachmentBudgetExhaustionIsAnnounced(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	// Which of the two loads first is not fixed: memories render in id order and
	// ids are random. The invariant under test is the budget, not the order — one
	// document fits, the other is announced as excluded rather than silently
	// dropped. (This used to pin the order with Priority, a field that was
	// written, returned by the API, and read by nothing.)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeFact, Text: "first",
		FileRef: "files/a.md",
	})
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeFact, Text: "second",
		FileRef: "files/b.md",
	})

	fl := &fakeLoader{files: map[string]string{
		"files/a.md": strings.Repeat("a", 50),
		"files/b.md": strings.Repeat("b", 50),
	}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithFileTotalMaxBytes(50))

	loadedA := strings.Contains(res.Attachments, strings.Repeat("a", 50))
	loadedB := strings.Contains(res.Attachments, strings.Repeat("b", 50))
	if loadedA == loadedB {
		t.Fatalf("exactly one document should fit a 50-byte budget (a=%v b=%v):\n%s",
			loadedA, loadedB, res.Attachments)
	}
	if !strings.Contains(res.Attachments, "not included: the per-turn attachment budget (50 bytes) is exhausted. Read the file directly if you need it.") {
		t.Fatalf("expected budget notice:\n%s", res.Attachments)
	}
	// The excluded document is never loaded: the budget check precedes the call.
	if len(fl.calls) != 1 {
		t.Fatalf("only the fitting document should be loaded, got %v", fl.calls)
	}
}

// When the remaining budget is smaller than the per-file cap, the next document
// is cut to what is left rather than dropped: the sticky document takes 50 of
// a 70-byte budget, so the routed one is loaded with a 20-byte limit.
func TestAttachmentPartialFitUsesRemainingBudget(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Sticky rule.",
		FileRef: "files/first.md",
	})
	topic := mustDomain(t, s, store.CreateDomainParams{Name: "Writing"})
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: topic.ID, Type: store.TypeRule, Text: "Routed rule.",
		FileRef: "files/second.md",
	})

	fl := &fakeLoader{files: map[string]string{
		"files/first.md":  strings.Repeat("A", 50),
		"files/second.md": strings.Repeat("B", 50),
	}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithFileMaxBytes(100), WithFileTotalMaxBytes(70))

	if !strings.Contains(res.Attachments, strings.Repeat("A", 50)) {
		t.Fatalf("sticky document should load whole:\n%s", res.Attachments)
	}
	if !strings.Contains(res.RoutedAttachments, "[TRUNCATED: showing the first 20 of 50 bytes.") {
		t.Fatalf("routed document should be truncated to the remaining 20 bytes:\n%s", res.RoutedAttachments)
	}
	if !strings.Contains(res.RoutedAttachments, "\n\n"+strings.Repeat("B", 20)) || strings.Contains(res.RoutedAttachments, strings.Repeat("B", 21)) {
		t.Fatalf("routed document should carry exactly 20 bytes:\n%s", res.RoutedAttachments)
	}
	if strings.Contains(res.RoutedAttachments, "budget") {
		t.Fatalf("a partially fitting document must not be reported as budget-dropped:\n%s", res.RoutedAttachments)
	}
	if got := fmt.Sprint(fl.calls); got != "[files/first.md files/second.md]" {
		t.Fatalf("load order = %s, want sticky first then routed", got)
	}
}

func TestUnreadableAttachmentIsReportedNotSilent(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	m := mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "voice",
		FileRef: "/etc/shadow.md",
	})

	fl := &fakeLoader{files: map[string]string{}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	want := "### Attached: /etc/shadow.md\nFrom memory " + m.ID + " (\"voice\") — not included: access denied: outside the agent's readable paths"
	if !strings.Contains(res.Attachments, want) {
		t.Fatalf("expected an explicit unavailable note %q:\n%s", want, res.Attachments)
	}
}

// A long memory text is cut to maxHeadlineChars (trimmed, with an ellipsis) in
// the document header, and only its first line is used.
func TestDocumentHeaderTrimsLongMemoryText(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	long := strings.Repeat("word ", 60) + "\nsecond line"
	m := mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: long,
		FileRef: "files/voice.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "BODY"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	// 120 chars of "word " is 24 words ending in a space; the trim drops it.
	headline := strings.TrimSpace(strings.Repeat("word ", 24)) + "…"
	if len(headline) != maxHeadlineChars-1+len("…") {
		t.Fatalf("test setup: headline is %d chars", len(headline))
	}
	want := "### Attached: files/voice.md\nFrom memory " + m.ID + " (\"" + headline + "\"), 4 bytes, current as of this turn.\n\nBODY"
	if !strings.Contains(res.Attachments, want) {
		t.Fatalf("header not rendered as expected:\n%s\nwant to contain:\n%s", res.Attachments, want)
	}
	if strings.Contains(res.Attachments, "second line") {
		t.Fatalf("headline must use the first line only:\n%s", res.Attachments)
	}
}

func TestNoLoaderMeansNoAttachmentsBlock(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "voice",
		FileRef: "files/voice.md",
	})

	res := composeWith(t, s)

	if res.Attachments != "" {
		t.Fatalf("no loader configured, expected no attachments block:\n%s", res.Attachments)
	}
	if strings.Contains(res.Stable, "files/voice.md") {
		t.Fatalf("memory line should never carry a file marker:\n%s", res.Stable)
	}
}

func TestMemoryWithoutFileRefAddsNothing(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypePreference, Text: "Be concise.",
	})

	fl := &fakeLoader{files: map[string]string{}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	if res.Attachments != "" || len(fl.calls) != 0 {
		t.Fatalf("plain memory must not trigger attachment work: %q %v", res.Attachments, fl.calls)
	}
	if strings.Contains(res.Stable, "Attached") {
		t.Fatalf("unexpected attachment text:\n%s", res.Stable)
	}
}

func TestDroppedRoutedDomainDoesNotAttach(t *testing.T) {
	s := newStore(t)
	// Two topic domains; maxChars is tight enough that only the first section fits.
	for _, name := range []string{"First", "Second"} {
		d := mustDomain(t, s, store.CreateDomainParams{
			Name:    name,
			Summary: strings.Repeat("summary ", 20),
		})
		mustMemory(t, s, store.AddMemoryParams{
			DomainID: d.ID, Type: store.TypeFact, Text: name + " note",
			FileRef: "files/" + strings.ToLower(name) + ".md",
		})
	}

	fl := &fakeLoader{files: map[string]string{"files/first.md": "A", "files/second.md": "B"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithMaxChars(200), WithTopKDomains(5))

	if len(res.Loaded) != 1 {
		t.Fatalf("expected the char budget to drop a domain, loaded=%v", res.Loaded)
	}
	if len(fl.calls) != 1 {
		t.Fatalf("only the rendered domain's document should load, got %v", fl.calls)
	}
}

// TestAttachmentSharedByBothBlocks covers a document cited from a sticky AND a
// routed memory. It must appear exactly once — duplicating it would double its
// cost — and it goes with the stable partition, which keeps it in the cached
// part of the request. Its provenance still names both owners, so the routed
// memory is not orphaned.
func TestAttachmentSharedByBothBlocks(t *testing.T) {
	s := newStore(t)

	gen := mustGeneral(t, s)
	sticky := mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Always use the house voice.",
		FileRef: "files/voice.md",
	})
	topic := mustDomain(t, s, store.CreateDomainParams{Name: "Writing"})
	routed := mustMemory(t, s, store.AddMemoryParams{
		DomainID: topic.ID, Type: store.TypeRule, Text: "Chapter drafts follow the voice guide.",
		FileRef: "files/voice.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "VOICEBODY"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	total := strings.Count(res.Attachments, "VOICEBODY") + strings.Count(res.RoutedAttachments, "VOICEBODY")
	if total != 1 {
		t.Fatalf("shared document should appear exactly once, got %d copies\nstable:\n%s\nrouted:\n%s",
			total, res.Attachments, res.RoutedAttachments)
	}
	if !strings.Contains(res.Attachments, "VOICEBODY") {
		t.Errorf("a document with a sticky owner belongs in the stable (cached) partition:\n%s", res.Attachments)
	}
	if res.RoutedAttachments != "" {
		t.Errorf("the routed partition should be empty when the only document is shared:\n%s", res.RoutedAttachments)
	}
	// Provenance names both owners so the routed memory can still be tied to it.
	for _, id := range []string{sticky.ID, routed.ID} {
		if !strings.Contains(res.Attachments, id) {
			t.Errorf("provenance should name owner %s:\n%s", id, res.Attachments)
		}
	}
}

// TestAttachmentBudgetSharedAcrossPartitions guards the property the split could
// most easily break: one budget spent sticky-first, not one budget per
// partition. Rendering them independently would silently double the per-turn
// attachment cost.
func TestAttachmentBudgetSharedAcrossPartitions(t *testing.T) {
	s := newStore(t)

	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Sticky rule.",
		FileRef: "files/first.md",
	})
	topic := mustDomain(t, s, store.CreateDomainParams{Name: "Writing"})
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: topic.ID, Type: store.TypeRule, Text: "Routed rule.",
		FileRef: "files/second.md",
	})

	fl := &fakeLoader{files: map[string]string{
		"files/first.md":  strings.Repeat("A", 50),
		"files/second.md": strings.Repeat("B", 50),
	}}
	// Budget fits the first document only.
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithFileTotalMaxBytes(50))

	if !strings.Contains(res.Attachments, strings.Repeat("A", 50)) {
		t.Errorf("sticky document should get first claim on the shared budget:\n%s", res.Attachments)
	}
	if strings.Contains(res.RoutedAttachments, strings.Repeat("B", 50)) {
		t.Errorf("routed document should not fit — the budget is shared, not per-partition:\n%s", res.RoutedAttachments)
	}
	if !strings.Contains(res.RoutedAttachments, "not included: the per-turn attachment budget (50 bytes) is exhausted") {
		t.Errorf("the dropped document should say why it is missing:\n%s", res.RoutedAttachments)
	}
}
