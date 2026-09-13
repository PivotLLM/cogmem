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
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	m, err := s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Write in my voice.",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/voice.md",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "# Voice\n\nShort sentences.\n"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	// The memory line names no file — the path appears once, with its content.
	if strings.Contains(res.Stable, "files/voice.md") {
		t.Fatalf("memory line should carry no file marker:\n%s", res.Stable)
	}
	for _, want := range []string{
		"# Attached Documents",
		"### Attached: files/voice.md",
		`From memory ` + m.ID + ` ("Write in my voice.")`,
		"current as of this turn",
		"Short sentences.",
	} {
		if !strings.Contains(res.Attachments, want) {
			t.Fatalf("attachments block missing %q:\n%s", want, res.Attachments)
		}
	}
}

func TestAttachmentFromRoutedDomain(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, store.CreateDomainParams{AgentID: "a", Name: "Writing"})
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeRule, Text: "Voice guide.",
		Status: store.StatusActive, Confidence: 0.9,
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
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	for i := 0; i < 3; i++ {
		_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
			DomainID: gen.ID, Type: store.TypeFact, Text: fmt.Sprintf("note %d", i),
			Status: store.StatusActive, Confidence: 0.9,
			FileRef: "files/voice.md",
		})
	}

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "BODY"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	if len(fl.calls) != 1 {
		t.Fatalf("expected one load for a shared document, got %v", fl.calls)
	}
	if n := strings.Count(res.Attachments, "BODY"); n != 1 {
		t.Fatalf("expected document injected once, got %d copies:\n%s", n, res.Attachments)
	}
}

func TestAttachmentTruncationIsAnnounced(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "big doc",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/big.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/big.md": strings.Repeat("x", 100)}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load), WithFileMaxBytes(10))

	if !strings.Contains(res.Attachments, "TRUNCATED: showing the first 10 of 100 bytes") {
		t.Fatalf("expected truncation notice:\n%s", res.Attachments)
	}
}

func TestAttachmentBudgetExhaustionIsAnnounced(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	// Which of the two loads first is not fixed: memories render in id order and
	// ids are random. The invariant under test is the budget, not the order — one
	// document fits, the other is announced as excluded rather than silently
	// dropped. (This used to pin the order with Priority, a field that was
	// written, returned by the API, and read by nothing.)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeFact, Text: "first",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/a.md",
	})
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeFact, Text: "second",
		Status: store.StatusActive, Confidence: 0.9,
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
	if !strings.Contains(res.Attachments, "not included: the per-turn attachment budget (50 bytes) is exhausted") {
		t.Fatalf("expected budget notice:\n%s", res.Attachments)
	}
}

func TestUnreadableAttachmentIsReportedNotSilent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "voice",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "/etc/shadow.md",
	})

	fl := &fakeLoader{files: map[string]string{}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	if !strings.Contains(res.Attachments, "not included: access denied") {
		t.Fatalf("expected an explicit unavailable note:\n%s", res.Attachments)
	}
}

// A pending memory's document is named in the attachments section with a single
// line explaining why it is absent — never its contents.
func TestDocumentHeaderTrimsLongMemoryText(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	long := strings.Repeat("word ", 60) + "\nsecond line"
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: long,
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/voice.md",
	})

	fl := &fakeLoader{files: map[string]string{"files/voice.md": "BODY"}}
	res := composeWith(t, s, WithAttachmentLoader(fl.load))

	header := strings.SplitN(res.Attachments, "\n\nBODY", 2)[0]
	lines := strings.Split(strings.TrimSpace(header), "\n")
	last := lines[len(lines)-1]
	if len(last) > maxHeadlineChars+80 {
		t.Fatalf("header line not trimmed (%d chars): %q", len(last), last)
	}
	if !strings.Contains(last, "…") {
		t.Fatalf("expected an ellipsis on the trimmed headline: %q", last)
	}
}

func TestNoLoaderMeansNoAttachmentsBlock(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "voice",
		Status: store.StatusActive, Confidence: 0.9,
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
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypePreference, Text: "Be concise.",
		Status: store.StatusActive, Confidence: 0.9,
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
	ctx := context.Background()
	db := s.DB()
	// Two topic domains; maxChars is tight enough that only the first section fits.
	for _, name := range []string{"First", "Second"} {
		d, _ := s.CreateDomain(ctx, db, store.CreateDomainParams{
			AgentID: "a", Name: name,
			Summary: strings.Repeat("summary ", 20),
		})
		_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
			DomainID: d.ID, Type: store.TypeFact, Text: name + " note",
			Status: store.StatusActive, Confidence: 0.9,
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
	ctx := context.Background()
	db := s.DB()

	gen, _ := s.GeneralDomain(ctx, db)
	sticky, _ := s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Always use the house voice.",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/voice.md",
	})
	topic, _ := s.CreateDomain(ctx, db, store.CreateDomainParams{AgentID: "a", Name: "Writing"})
	routed, _ := s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: topic.ID, Type: store.TypeRule, Text: "Chapter drafts follow the voice guide.",
		Status: store.StatusActive, Confidence: 0.9,
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
	ctx := context.Background()
	db := s.DB()

	gen, _ := s.GeneralDomain(ctx, db)
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: gen.ID, Type: store.TypeRule, Text: "Sticky rule.",
		Status: store.StatusActive, Confidence: 0.9,
		FileRef: "files/first.md",
	})
	topic, _ := s.CreateDomain(ctx, db, store.CreateDomainParams{AgentID: "a", Name: "Writing"})
	_, _ = s.AddMemory(ctx, db, store.AddMemoryParams{
		DomainID: topic.ID, Type: store.TypeRule, Text: "Routed rule.",
		Status: store.StatusActive, Confidence: 0.9,
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
	if !strings.Contains(res.RoutedAttachments, "budget") {
		t.Errorf("the dropped document should say why it is missing:\n%s", res.RoutedAttachments)
	}
}
