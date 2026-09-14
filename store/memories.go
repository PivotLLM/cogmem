// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AddMemoryParams are the inputs for AddMemory.
type AddMemoryParams struct {
	DomainID           string
	Type               MemoryType
	Text               string
	Status             Status // active or retired; defaults to active
	Confidence         float64
	Origin             Origin
	SourceSession      *string
	SourceSeqStart     *int64
	SourceSeqEnd       *int64
	SupersedesMemoryID *string
	FileRef            string
}

// AddMemory inserts a memory, assigns a short id, and bumps stable_rev when the
// memory affects always-on content (an active prompt-bearing memory in a sticky
// domain).
func (s *Store) AddMemory(ctx context.Context, q DBTX, p AddMemoryParams) (Memory, error) {
	if p.Status == "" {
		p.Status = StatusActive
	}
	sticky, err := s.domainSticky(ctx, q, p.DomainID)
	if err != nil {
		return Memory{}, err
	}
	id, err := freshID(ctx, q, memoryIDPrefix, "memories")
	if err != nil {
		return Memory{}, err
	}
	ts := now()
	_, err = q.ExecContext(ctx, `
		INSERT INTO memories(id, domain_id, type, text, status, confidence,
		                  origin, source_session, source_seq_start, source_seq_end,
		                  supersedes_memory_id, file_ref, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, p.DomainID, string(p.Type), p.Text, string(p.Status), p.Confidence,
		string(normalizeOrigin(p.Origin)), p.SourceSession, p.SourceSeqStart,
		p.SourceSeqEnd, p.SupersedesMemoryID, strings.TrimSpace(p.FileRef), ts, ts)
	if err != nil {
		return Memory{}, fmt.Errorf("cogmem: add hook: %w", err)
	}
	if affectsStable(sticky, p.Status, p.Type) {
		if err := bumpStableRev(ctx, q); err != nil {
			return Memory{}, err
		}
	}
	// Writing a memory counts as using the domain (recency / staleness signal).
	_ = s.Touch(ctx, q, p.DomainID)
	return s.GetMemory(ctx, q, id)
}

// RetireMemory marks a hook retired with a reason. It stays in the audit trail.
func (s *Store) RetireMemory(ctx context.Context, q DBTX, id, reason string) error {
	h, err := s.GetMemory(ctx, q, id)
	if err != nil {
		return err
	}
	sticky, err := s.domainSticky(ctx, q, h.DomainID)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx,
		`UPDATE memories SET status=?, retire_reason=?, updated_at=? WHERE id=?`,
		string(StatusRetired), reason, now(), id)
	if err != nil {
		return err
	}
	_ = s.Touch(ctx, q, h.DomainID)
	if affectsStable(sticky, h.Status, h.Type) {
		return bumpStableRev(ctx, q)
	}
	return nil
}

// DeleteMemory hard-deletes a memory row (no audit trail kept), bumping
// stable_rev when it was active content in a sticky domain. Returns ErrNotFound
// if the memory does not exist.
func (s *Store) DeleteMemory(ctx context.Context, q DBTX, id string) error {
	h, err := s.GetMemory(ctx, q, id)
	if err != nil {
		return err
	}
	sticky, err := s.domainSticky(ctx, q, h.DomainID)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, id); err != nil {
		return fmt.Errorf("cogmem: delete memory: %w", err)
	}
	if affectsStable(sticky, h.Status, h.Type) {
		return bumpStableRev(ctx, q)
	}
	return nil
}

// SupersedeMemory retires oldID and adds a replacement hook linked back to it.
// An attached file carries forward unless the replacement names its own: a
// consolidation pass that rewords a memory must not silently detach the document
// the memory exists to point at.
func (s *Store) SupersedeMemory(ctx context.Context, q DBTX, oldID string, p AddMemoryParams) (Memory, error) {
	if strings.TrimSpace(p.FileRef) == "" {
		if old, err := s.GetMemory(ctx, q, oldID); err == nil {
			p.FileRef = old.FileRef
		}
	}
	if err := s.RetireMemory(ctx, q, oldID, "superseded"); err != nil {
		return Memory{}, err
	}
	p.SupersedesMemoryID = &oldID
	return s.AddMemory(ctx, q, p)
}

// SetMemoryFileRef attaches a markdown file to an existing memory, or detaches
// the current one when ref is empty. The path is stored verbatim (trimmed); the
// caller validates it against the agent's read permissions.
//
// This is the one in-place edit cogmem allows on a memory. Text is immutable by
// design (retire + recreate keeps the audit trail honest), but a document
// pointer has to be repointable: the file it names can move, and detaching it
// should not cost the memory its id and history.
func (s *Store) SetMemoryFileRef(ctx context.Context, q DBTX, id, ref string) (Memory, error) {
	h, err := s.GetMemory(ctx, q, id)
	if err != nil {
		return Memory{}, err
	}
	sticky, err := s.domainSticky(ctx, q, h.DomainID)
	if err != nil {
		return Memory{}, err
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE memories SET file_ref=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(ref), now(), id); err != nil {
		return Memory{}, fmt.Errorf("cogmem: set memory file ref: %w", err)
	}
	_ = s.Touch(ctx, q, h.DomainID)
	if affectsStable(sticky, h.Status, h.Type) {
		if err := bumpStableRev(ctx, q); err != nil {
			return Memory{}, err
		}
	}
	return s.GetMemory(ctx, q, id)
}

// SetMemoryType changes a memory's type. This is an operator action from the
// WebUI: the model chooses a type when it writes, and gets it wrong often
// enough — a trip log filed as a fact, a self-directed note filed as a rule —
// that correcting it by hand has to be possible without losing the memory's id
// and history.
//
// Retyping to or from TypeEvent moves a memory in or out of the prompt, so the
// stable block is rebuilt on any change.
func (s *Store) SetMemoryType(ctx context.Context, q DBTX, id string, t MemoryType) (Memory, error) {
	if !ValidMemoryTypes(t) {
		return Memory{}, fmt.Errorf("cogmem: invalid memory type %q", t)
	}
	h, err := s.GetMemory(ctx, q, id)
	if err != nil {
		return Memory{}, err
	}
	if h.Type == t {
		return h, nil
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE memories SET type=?, updated_at=? WHERE id=?`,
		string(t), now(), id); err != nil {
		return Memory{}, fmt.Errorf("cogmem: set memory type: %w", err)
	}
	_ = s.Touch(ctx, q, h.DomainID)
	if err := bumpStableRev(ctx, q); err != nil {
		return Memory{}, err
	}
	return s.GetMemory(ctx, q, id)
}

// RestoreMemory returns a retired memory to active, clearing its retire reason.
// The counterpart to RetireMemory, so the WebUI's status control works in both
// directions.
func (s *Store) RestoreMemory(ctx context.Context, q DBTX, id string) error {
	h, err := s.GetMemory(ctx, q, id)
	if err != nil {
		return err
	}
	if h.Status == StatusActive {
		return nil
	}
	sticky, err := s.domainSticky(ctx, q, h.DomainID)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE memories SET status=?, retire_reason=NULL, updated_at=? WHERE id=?`,
		string(StatusActive), now(), id); err != nil {
		return err
	}
	_ = s.Touch(ctx, q, h.DomainID)
	if affectsStable(sticky, StatusActive, h.Type) {
		return bumpStableRev(ctx, q)
	}
	return nil
}

// GetMemory loads one hook by id.
func (s *Store) GetMemory(ctx context.Context, q DBTX, id string) (Memory, error) {
	row := q.QueryRowContext(ctx, memorySelect+` WHERE id=?`, id)
	h, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	return h, err
}

// ListMemories returns a domain's memories with any of the given statuses (all
// if none), ordered by id. Includes every type — callers that render the prompt
// filter events out; callers that show the operator everything do not.
func (s *Store) ListMemories(ctx context.Context, q DBTX, domainID string, statuses ...Status) ([]Memory, error) {
	query := memorySelect + ` WHERE domain_id=?`
	args := []any{domainID}
	if len(statuses) > 0 {
		query += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, string(st))
		}
	}
	query += ` ORDER BY id`
	return s.queryMemories(ctx, q, query, args...)
}

// ListPromptMemories returns a domain's active memories that belong in the
// prompt — every type except TypeEvent. This is the composer's read path.
//
// Oldest first. Ids are random, so ordering by id was effectively arbitrary and
// not even stable between stores; chronological order makes the rendered block
// stable and puts the newest statement on a topic last, which is where a reader
// looks for the current one.
func (s *Store) ListPromptMemories(ctx context.Context, q DBTX, domainID string) ([]Memory, error) {
	return s.queryMemories(ctx, q,
		memorySelect+` WHERE domain_id=? AND status=? AND type<>? ORDER BY created_at, id`,
		domainID, string(StatusActive), string(TypeEvent))
}

// CountEvents returns how many active event memories a domain holds. Events are
// never loaded into the prompt, so the domain block reports the count instead —
// they stay out of the way without disappearing.
func (s *Store) CountEvents(ctx context.Context, q DBTX, domainID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE domain_id=? AND status=? AND type=?`,
		domainID, string(StatusActive), string(TypeEvent)).Scan(&n)
	return n, err
}

// SearchMemories does a case-insensitive LIKE scan over active memory text. No
// FTS5 (per design); fine at cogmem scale.
//
// includeEvents controls whether event memories are searchable. They are
// excluded by default so an ordinary lookup is not buried under trip logs, and
// included on request — search is the only way to reach an event at all, so
// this flag is the whole retrieval path for them.
func (s *Store) SearchMemories(ctx context.Context, q DBTX, term string, limit int, includeEvents bool) ([]Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	like := "%" + strings.ToLower(term) + "%"
	query := memorySelect + ` WHERE status=? AND lower(text) LIKE ?`
	args := []any{string(StatusActive), like}
	if !includeEvents {
		query += ` AND type<>?`
		args = append(args, string(TypeEvent))
	}
	query += ` ORDER BY confidence DESC, id LIMIT ?`
	args = append(args, limit)
	return s.queryMemories(ctx, q, query, args...)
}

func (s *Store) queryMemories(ctx context.Context, q DBTX, query string, args ...any) ([]Memory, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		h, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// domainSticky reports whether the domain is sticky (legacy "type" column parsed
// as int > 0).
func (s *Store) domainSticky(ctx context.Context, q DBTX, domainID string) (bool, error) {
	var t string
	err := q.QueryRowContext(ctx, `SELECT type FROM domains WHERE id=?`, domainID).Scan(&t)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	n, _ := strconv.Atoi(strings.TrimSpace(t))
	return n > 0, err
}

// affectsStable reports whether a memory is part of the cached stable block, so
// a write to it must rebuild that block. Only an active, prompt-bearing memory
// in a sticky domain is: an event never reaches the prompt, and a retired
// memory has left it.
func affectsStable(sticky bool, st Status, t MemoryType) bool {
	return sticky && st == StatusActive && t.Prompt()
}

const memorySelect = `
	SELECT id, domain_id, type, text, status, confidence, origin,
	       source_session, source_seq_start, source_seq_end, supersedes_memory_id,
	       retire_reason, file_ref, created_at, updated_at
	FROM memories`

func scanMemory(sc scanner) (Memory, error) {
	var (
		h                    Memory
		kind, status, origin string
		createdAt, updatedAt int64
	)
	err := sc.Scan(&h.ID, &h.DomainID, &kind, &h.Text, &status, &h.Confidence,
		&origin, &h.SourceSession, &h.SourceSeqStart, &h.SourceSeqEnd,
		&h.SupersedesMemoryID, &h.RetireReason, &h.FileRef, &createdAt, &updatedAt)
	if err != nil {
		return Memory{}, err
	}
	h.Type = MemoryType(kind)
	h.Status = Status(status)
	h.Origin = normalizeOrigin(Origin(origin))
	h.CreatedAt = timeUnix(createdAt)
	h.UpdatedAt = timeUnix(updatedAt)
	return h, nil
}

// MatchActiveMemories returns every active memory whose text contains term
// (case-insensitive), events included, optionally restricted to one domain.
// Unlike SearchMemories there is no cap: this is the read for an operation
// that must reach every match, such as forgetting.
func (s *Store) MatchActiveMemories(ctx context.Context, q DBTX, term, domainID string) ([]Memory, error) {
	query := memorySelect + ` WHERE status=? AND lower(text) LIKE ?`
	args := []any{string(StatusActive), "%" + strings.ToLower(term) + "%"}
	if domainID != "" {
		query += ` AND domain_id=?`
		args = append(args, domainID)
	}
	query += ` ORDER BY id`
	return s.queryMemories(ctx, q, query, args...)
}
