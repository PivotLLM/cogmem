// cogmem - Cognitive Memory
// License: MIT

// Package portable is cogmem's round-trip format: a YAML document holding every
// domain and memory in a store, which can be written out and read back.
//
// YAML rather than JSON because memory text is prose, often several sentences
// and sometimes several paragraphs. YAML writes that as a literal block that
// stays readable and editable; JSON collapses it to one line with escaped
// newlines, which is what made the previous Markdown export a thing you could
// only look at. This one can be edited in a text editor and imported back.
//
// Identifiers are written for a human reading the file and IGNORED on import —
// see Import — so a document can be loaded into any agent.
package portable

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/PivotLLM/cogmem/store"
)

// FormatVersion is the version of this document format, written into every
// export and checked on import. It is independent of the database schema
// version: the point of the format is to outlive schema changes.
const FormatVersion = 1

// Document is a complete memory export.
type Document struct {
	FormatVersion int      `yaml:"format_version"`
	ExportedAt    string   `yaml:"exported_at"`
	SourcePath    string   `yaml:"source_path,omitempty"`
	Domains       []Domain `yaml:"domains"`
}

// Domain is one exported domain and its memories.
type Domain struct {
	// ID is informational. Import mints a new one — nothing outside a single
	// database references these, so preserving them would only risk collisions
	// when a document is loaded into an agent that already uses the id.
	ID              string   `yaml:"id,omitempty"`
	Name            string   `yaml:"name"`
	Sticky          bool     `yaml:"sticky,omitempty"`
	Status          string   `yaml:"status"`
	Summary         string   `yaml:"summary,omitempty"`
	Triggers        string   `yaml:"triggers,omitempty"`
	KeywordTriggers string   `yaml:"keyword_triggers,omitempty"`
	Blockers        []string `yaml:"blockers,omitempty"`
	NextActions     []string `yaml:"next_actions,omitempty"`
	Constraints     []string `yaml:"constraints,omitempty"`
	Memories        []Memory `yaml:"memories,omitempty"`
}

// Memory is one exported memory.
type Memory struct {
	ID           string  `yaml:"id,omitempty"` // informational; see Domain.ID
	Type         string  `yaml:"type"`
	Status       string  `yaml:"status"`
	Confidence   float64 `yaml:"confidence"`
	Origin       string  `yaml:"origin"`
	FileRef      string  `yaml:"file_ref,omitempty"`
	RetireReason string  `yaml:"retire_reason,omitempty"`
	CreatedAt    string  `yaml:"created_at,omitempty"`
	// Text is last so the block scalar that usually renders it does not push
	// the short scalar fields off the far side of a long memory.
	Text string `yaml:"text"`
}

// MarshalYAML renders a memory with its text as a literal block (|-) whenever
// that is more readable than a quoted scalar — which is most of the time, and
// is the reason this format is YAML at all.
//
// yaml.v3 will not emit literal style for a string it cannot round-trip that
// way (trailing whitespace on a line, for instance); it falls back to a quoted
// scalar on its own, so this is a preference rather than an instruction.
func (m Memory) MarshalYAML() (any, error) {
	type plain Memory // no recursion
	node := &yaml.Node{}
	if err := node.Encode(plain(m)); err != nil {
		return nil, err
	}
	if !wantsBlock(m.Text) {
		return node, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "text" {
			node.Content[i+1].Style = yaml.LiteralStyle
			break
		}
	}
	return node, nil
}

// wantsBlock reports whether text is better as a literal block than a scalar.
// Multi-line always is. A long single line is too: the alternative wraps at the
// emitter's column limit with continuation indentation, which is harder to read
// and much harder to edit than one block.
func wantsBlock(text string) bool {
	if strings.Contains(text, "\n") {
		return true
	}
	if len(text) <= 80 {
		return false
	}
	// A literal block cannot represent a string with trailing spaces on a line
	// or one that does not survive the round trip; leave those to the emitter.
	return text == strings.TrimRight(text, " \t")
}

// Export reads the whole store into a Document. Every domain and every memory,
// whatever their status — an export is a backup, so it holds the retired ones
// too.
func Export(ctx context.Context, st *store.Store) (Document, error) {
	doc := Document{
		FormatVersion: FormatVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		SourcePath:    st.Path(),
	}
	domains, err := st.ListDomains(ctx, st.DB())
	if err != nil {
		return Document{}, fmt.Errorf("cogmem: export domains: %w", err)
	}
	for _, d := range domains {
		out := Domain{
			ID:              d.ID,
			Name:            d.Name,
			Sticky:          d.Sticky(),
			Status:          string(d.Status),
			Summary:         d.Summary,
			Triggers:        d.Triggers,
			KeywordTriggers: d.KeywordTriggers,
			Blockers:        d.State.Blockers,
			NextActions:     d.State.NextActions,
			Constraints:     d.State.Constraints,
		}
		mems, err := st.ListMemories(ctx, st.DB(), d.ID)
		if err != nil {
			return Document{}, fmt.Errorf("cogmem: export memories of %s: %w", d.ID, err)
		}
		for _, m := range mems {
			em := Memory{
				ID:         m.ID,
				Type:       string(m.Type),
				Status:     string(m.Status),
				Confidence: m.Confidence,
				Origin:     string(m.Origin),
				FileRef:    m.FileRef,
				Text:       m.Text,
				CreatedAt:  m.CreatedAt.UTC().Format(time.RFC3339),
			}
			if m.RetireReason != nil {
				em.RetireReason = *m.RetireReason
			}
			out.Memories = append(out.Memories, em)
		}
		doc.Domains = append(doc.Domains, out)
	}
	return doc, nil
}

// Marshal renders a Document as YAML.
func Marshal(doc Document) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# ClawEh cognitive memory export.\n")
	b.WriteString("# Edit and import to restore. Ids are informational and are\n")
	b.WriteString("# re-minted on import, so this file can be loaded into any agent.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("cogmem: marshal export: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("cogmem: marshal export: %w", err)
	}
	return []byte(b.String()), nil
}

// Unmarshal parses an exported document and checks its format version.
func Unmarshal(data []byte) (Document, error) {
	var doc Document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Document{}, fmt.Errorf("cogmem: parse import: %w", err)
	}
	if doc.FormatVersion == 0 {
		return Document{}, fmt.Errorf("cogmem: not a memory export (no format_version)")
	}
	if doc.FormatVersion > FormatVersion {
		return Document{}, fmt.Errorf(
			"cogmem: export is format version %d, this build understands up to %d",
			doc.FormatVersion, FormatVersion)
	}
	return doc, nil
}

// ImportMode selects what happens to what is already in the store.
type ImportMode string

const (
	// ImportMerge adds what is missing and leaves everything else alone. A
	// domain is matched by name, a memory by its text within that domain, so
	// importing the same document twice is a no-op.
	ImportMerge ImportMode = "merge"
	// ImportReplace empties the store first, making a document a true restore
	// point: what comes back is exactly what was exported, and anything learned
	// since is gone.
	ImportReplace ImportMode = "replace"
)

// ImportResult reports what an import did.
type ImportResult struct {
	DomainsCreated  int `json:"domains_created"`
	DomainsMatched  int `json:"domains_matched"`
	MemoriesCreated int `json:"memories_created"`
	MemoriesSkipped int `json:"memories_skipped"`
}

// Import loads a document into st.
//
// Ids in the document are ignored and fresh ones minted, which is what lets a
// document be loaded into a different agent — seeding a new assistant from an
// existing one's domains. Nothing outside a single database refers to a memory
// id, so a restore under new ids is indistinguishable from the original.
func Import(ctx context.Context, st *store.Store, doc Document, mode ImportMode, agentID, sessionKey string) (ImportResult, error) {
	var res ImportResult
	if mode != ImportMerge && mode != ImportReplace {
		return res, fmt.Errorf("cogmem: unknown import mode %q", mode)
	}
	if mode == ImportReplace {
		existing, err := st.ListDomains(ctx, st.DB())
		if err != nil {
			return res, fmt.Errorf("cogmem: import: list existing: %w", err)
		}
		for _, d := range existing {
			if err := st.DeleteDomain(ctx, st.DB(), d.ID); err != nil {
				return res, fmt.Errorf("cogmem: import: clear %s: %w", d.ID, err)
			}
		}
	}
	for _, d := range doc.Domains {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			continue
		}
		target, err := st.DomainByName(ctx, st.DB(), name)
		switch {
		case err == nil:
			res.DomainsMatched++
		default:
			target, err = st.CreateDomain(ctx, st.DB(), store.CreateDomainParams{
				AgentID:    agentID,
				SessionKey: sessionKey,
				Sticky:     d.Sticky,
				Name:       name,
				Status:     domainStatus(d.Status),
				Summary:    d.Summary,
				State: store.DomainState{
					Blockers:    d.Blockers,
					NextActions: d.NextActions,
					Constraints: d.Constraints,
				},
				Triggers:        d.Triggers,
				KeywordTriggers: d.KeywordTriggers,
			})
			if err != nil {
				return res, fmt.Errorf("cogmem: import: create domain %q: %w", name, err)
			}
			res.DomainsCreated++
		}

		// Existing text in this domain, so a merge does not duplicate.
		seen := map[string]bool{}
		if mode == ImportMerge {
			cur, err := st.ListMemories(ctx, st.DB(), target.ID)
			if err != nil {
				return res, fmt.Errorf("cogmem: import: read %s: %w", target.ID, err)
			}
			for _, m := range cur {
				seen[strings.TrimSpace(m.Text)] = true
			}
		}
		for _, m := range d.Memories {
			text := strings.TrimSpace(m.Text)
			if text == "" {
				continue
			}
			if seen[text] {
				res.MemoriesSkipped++
				continue
			}
			seen[text] = true
			// Always added active, then retired if the document says so. Retiring
			// through RetireMemory is what carries the reason across and writes the
			// audit event; inserting straight into the retired status would drop
			// both, so a restored memory would lose why it was retired.
			added, err := st.AddMemory(ctx, st.DB(), store.AddMemoryParams{
				DomainID:   target.ID,
				Type:       memoryType(m.Type),
				Text:       text,
				Status:     store.StatusActive,
				Confidence: m.Confidence,
				Origin:     store.Origin(m.Origin),
				FileRef:    m.FileRef,
			})
			if err != nil {
				return res, fmt.Errorf("cogmem: import: add memory to %s: %w", target.ID, err)
			}
			if memoryStatus(m.Status) == store.StatusRetired {
				reason := m.RetireReason
				if reason == "" {
					reason = "retired before export"
				}
				if err := st.RetireMemory(ctx, st.DB(), added.ID, reason); err != nil {
					return res, fmt.Errorf("cogmem: import: retire %s: %w", added.ID, err)
				}
			}
			res.MemoriesCreated++
		}
	}
	return res, nil
}

// domainStatus maps an imported domain status onto a known one, defaulting to
// active. A document edited by hand is expected to contain typos.
func domainStatus(s string) store.Status {
	if store.Status(s) == store.StatusArchived {
		return store.StatusArchived
	}
	return store.StatusActive
}

// memoryStatus maps an imported memory status, defaulting to active.
func memoryStatus(s string) store.Status {
	if store.Status(s) == store.StatusRetired {
		return store.StatusRetired
	}
	return store.StatusActive
}

// memoryType maps an imported type, defaulting to fact. An unknown type must
// not become an event by accident: that would silently drop the memory out of
// the prompt, which is the one mistake here that is invisible.
func memoryType(s string) store.MemoryType {
	t := store.MemoryType(s)
	if store.ValidMemoryTypes(t) {
		return t
	}
	return store.TypeFact
}
