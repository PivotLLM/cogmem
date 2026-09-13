// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotFound is returned when an addressed domain or hook does not exist.
var ErrNotFound = errors.New("cogmem: not found")

// ErrDuplicateName is returned by CreateDomain/UpdateDomain when a domain name
// collides with an existing active domain (case-insensitive, trimmed).
var ErrDuplicateName = errors.New("cogmem: a domain with that name already exists")

// stickyValue maps a sticky bool to the value stored in the legacy "type"
// column ("1" = sticky, "0" = not). The magnitude is reserved for future sorting.
func stickyValue(sticky bool) string {
	if sticky {
		return "1"
	}
	return "0"
}

// nameTaken reports whether an active domain (other than excludeID) already uses
// name (case-insensitive, trimmed).
func (s *Store) nameTaken(ctx context.Context, q DBTX, name, excludeID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM domains WHERE status=? AND lower(trim(name))=lower(trim(?)) AND id<>?`,
		string(StatusActive), name, excludeID).Scan(&n)
	return n > 0, err
}

// CreateDomainParams are the inputs for CreateDomain.
type CreateDomainParams struct {
	AgentID         string
	SessionKey      string
	Sticky          bool // injected into every prompt when true (use sparingly)
	Name            string
	Status          Status // active or review
	Summary         string
	State           DomainState
	Triggers        string // comma-delimited tool-name substrings (optional)
	KeywordTriggers string // comma-delimited message-text phrases (optional)
}

// CreateDomain inserts a new domain, assigns a short id, and bumps stable_rev
// (a new domain always affects either the index or the pending digest). Returns
// ErrDuplicateName if an active domain already uses the name.
func (s *Store) CreateDomain(ctx context.Context, q DBTX, p CreateDomainParams) (Domain, error) {
	if taken, err := s.nameTaken(ctx, q, p.Name, ""); err != nil {
		return Domain{}, err
	} else if taken {
		return Domain{}, ErrDuplicateName
	}
	id, err := freshID(ctx, q, domainIDPrefix, "domains")
	if err != nil {
		return Domain{}, err
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	stateJSON, err := json.Marshal(p.State)
	if err != nil {
		return Domain{}, err
	}
	ts := now()
	// All timestamps are unix seconds. Creation counts as the first activity.
	_, err = q.ExecContext(ctx, `
		INSERT INTO domains(id, agent_id, session_key, type, name, status, version,
		                    summary, state_json, schema_name, schema_version,
		                    last_active_at, triggers, keyword_triggers, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, p.AgentID, p.SessionKey, stickyValue(p.Sticky), p.Name, string(p.Status), 1,
		p.Summary, string(stateJSON), "domain", 1, ts, normalizeTriggers(p.Triggers), normalizeKeywords(p.KeywordTriggers), ts, ts)
	if err != nil {
		return Domain{}, fmt.Errorf("cogmem: create domain: %w", err)
	}
	if err := bumpStableRev(ctx, q); err != nil {
		return Domain{}, err
	}
	return s.GetDomain(ctx, q, id, false)
}

// UpdateDomainParams is a patch: every field is optional and only provided
// (non-nil) fields are changed. There is no optimistic-concurrency check —
// domain edits (rename, sticky, summary/state) don't risk overwriting data.
type UpdateDomainParams struct {
	Name            *string // rename; rejected with ErrDuplicateName on collision
	Summary         *string
	State           *DomainState
	Status          *Status
	Triggers        *string // when non-nil, replaces the comma-delimited trigger list
	KeywordTriggers *string // when non-nil, replaces the comma-delimited keyword-trigger list
	Sticky          *bool   // when non-nil, sets/clears sticky (always-in-prompt)
}

// UpdateDomain applies the patch (only provided fields change) and bumps the
// version. Returns ErrNotFound for a missing domain, or ErrDuplicateName when a
// rename collides with another active domain.
func (s *Store) UpdateDomain(ctx context.Context, q DBTX, id string, p UpdateDomainParams) error {
	cur, err := s.GetDomain(ctx, q, id, false)
	if err != nil {
		return err
	}
	name := cur.Name
	if p.Name != nil {
		if taken, terr := s.nameTaken(ctx, q, *p.Name, id); terr != nil {
			return terr
		} else if taken {
			return ErrDuplicateName
		}
		name = *p.Name
	}
	summary := cur.Summary
	if p.Summary != nil {
		summary = *p.Summary
	}
	state := cur.State
	if p.State != nil {
		state = *p.State
	}
	status := cur.Status
	if p.Status != nil {
		status = *p.Status
	}
	triggers := cur.Triggers
	if p.Triggers != nil {
		triggers = normalizeTriggers(*p.Triggers)
	}
	keywordTriggers := cur.KeywordTriggers
	if p.KeywordTriggers != nil {
		keywordTriggers = normalizeKeywords(*p.KeywordTriggers)
	}
	stickyCol := stickyValue(cur.Sticky())
	if p.Sticky != nil {
		stickyCol = stickyValue(*p.Sticky)
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, `
		UPDATE domains SET name=?, summary=?, state_json=?, status=?, triggers=?, keyword_triggers=?, type=?, version=version+1, updated_at=?
		WHERE id=?`,
		name, summary, string(stateJSON), string(status), triggers, keywordTriggers, stickyCol, now(), id)
	if err != nil {
		return fmt.Errorf("cogmem: update domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_ = s.Touch(ctx, q, id)
	// Stable block changes if a sticky domain changed, or name/summary/status/
	// sticky changed (index + always-in-prompt visibility).
	if cur.Sticky() || p.Sticky != nil || p.Name != nil || p.Summary != nil || p.Status != nil {
		if err := bumpStableRev(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// DeleteDomain hard-deletes a domain and all of its memories. Returns ErrNotFound
// if the domain does not exist.
func (s *Store) DeleteDomain(ctx context.Context, q DBTX, id string) error {
	if _, err := s.GetDomain(ctx, q, id, false); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM memories WHERE domain_id=?`, id); err != nil {
		return fmt.Errorf("cogmem: delete domain memories: %w", err)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM domains WHERE id=?`, id); err != nil {
		return fmt.Errorf("cogmem: delete domain: %w", err)
	}
	return bumpStableRev(ctx, q)
}

// MigrateDomain moves every memory from the "from" domain into the "to" domain,
// then hard-deletes the "from" domain. Both must exist.
func (s *Store) MigrateDomain(ctx context.Context, q DBTX, fromID, toID string) (moved int, err error) {
	if fromID == toID {
		return 0, errors.New("cogmem: from and to domains are the same")
	}
	if _, err := s.GetDomain(ctx, q, fromID, false); err != nil {
		return 0, err
	}
	if _, err := s.GetDomain(ctx, q, toID, false); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `UPDATE memories SET domain_id=? WHERE domain_id=?`, toID, fromID)
	if err != nil {
		return 0, fmt.Errorf("cogmem: migrate memories: %w", err)
	}
	n, _ := res.RowsAffected()
	_ = s.Touch(ctx, q, toID) // destination received memories — mark it active
	if _, err := q.ExecContext(ctx, `DELETE FROM domains WHERE id=?`, fromID); err != nil {
		return 0, fmt.Errorf("cogmem: delete migrated domain: %w", err)
	}
	if err := bumpStableRev(ctx, q); err != nil {
		return 0, err
	}
	return int(n), nil
}

// ArchiveDomain marks a domain archived (out of default prompting).
func (s *Store) ArchiveDomain(ctx context.Context, q DBTX, id string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE domains SET status=?, archived_at=?, version=version+1, updated_at=?
		WHERE id=? AND status!=?`,
		string(StatusArchived), now(), now(), id, string(StatusArchived))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either missing or already archived.
		if _, gerr := s.GetDomain(ctx, q, id, false); gerr != nil {
			return gerr
		}
		return nil
	}
	return bumpStableRev(ctx, q)
}

// Touch updates last_active_at (recency signal for routed pre-load). It does NOT
// bump stable_rev — recency is not part of the cached stable block.
func (s *Store) Touch(ctx context.Context, q DBTX, id string) error {
	_, err := q.ExecContext(ctx, `UPDATE domains SET last_active_at=? WHERE id=?`, now(), id)
	return err
}

// GetDomain loads one domain by id, optionally with its hooks.
func (s *Store) GetDomain(ctx context.Context, q DBTX, id string, withMemories bool) (Domain, error) {
	row := q.QueryRowContext(ctx, domainSelect+` WHERE id=?`, id)
	d, err := scanDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Domain{}, ErrNotFound
	}
	if err != nil {
		return Domain{}, err
	}
	if withMemories {
		memories, err := s.ListMemories(ctx, q, id, StatusActive)
		if err != nil {
			return Domain{}, err
		}
		d.Memories = memories
	}
	return d, nil
}

// DomainByName returns the active domain with the given name (case-insensitive,
// trimmed). Returns ErrNotFound if none exists.
func (s *Store) DomainByName(ctx context.Context, q DBTX, name string) (Domain, error) {
	row := q.QueryRowContext(ctx,
		domainSelect+` WHERE status=? AND lower(trim(name))=lower(trim(?)) ORDER BY id LIMIT 1`,
		string(StatusActive), name)
	d, err := scanDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Domain{}, ErrNotFound
	}
	return d, err
}

// GeneralDomain returns the seeded "General" domain (created sticky on a fresh
// DB). It may not exist if the user deleted it. Returns ErrNotFound if absent.
func (s *Store) GeneralDomain(ctx context.Context, q DBTX) (Domain, error) {
	row := q.QueryRowContext(ctx,
		domainSelect+` WHERE lower(trim(name))='general' AND status=? ORDER BY id LIMIT 1`,
		string(StatusActive))
	d, err := scanDomain(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Domain{}, ErrNotFound
	}
	return d, err
}

// ListDomains returns domains with any of the given statuses (all if none),
// ordered by id for stable index rendering.
func (s *Store) ListDomains(ctx context.Context, q DBTX, statuses ...Status) ([]Domain, error) {
	query := domainSelect
	var args []any
	if len(statuses) > 0 {
		query += ` WHERE status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, string(st))
		}
	}
	query += ` ORDER BY id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const domainSelect = `
	SELECT id, agent_id, session_key, type, name, status, version, summary,
	       state_json, schema_name, schema_version, last_active_at, triggers,
	       keyword_triggers, created_at, updated_at, archived_at
	FROM domains`

type scanner interface {
	Scan(dest ...any) error
}

// canonTool canonicalizes a tool name or trigger token for matching: trims,
// lowercases (case-insensitive), strips '*', and collapses runs of '_' to a
// single '_'. Matching is always substring (contains), so '*' is purely
// decorative: "*mail*", "mail*", and "mail" all canonicalize to "mail" and
// behave identically. The underscore collapse lets single-underscore tokens
// (e.g. "fusion_google_") match the double-underscore MCP separators in real
// names (mcp__fusion__google_*) and vice-versa.
func canonTool(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "*", "")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	return s
}

// normalizeTriggers canonicalizes a comma-delimited trigger list: canonTool each
// token (trim, lowercase, collapse underscores), drop empties, rejoin with single
// commas. Stored form is what TriggerTokens splits back out.
func normalizeTriggers(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := canonTool(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// TriggerTokens returns the domain's normalized trigger substrings (canonicalized,
// no empties). A domain activates when an invoked tool name contains any token.
func (d Domain) TriggerTokens() []string {
	if d.Triggers == "" {
		return nil
	}
	return strings.Split(normalizeTriggers(d.Triggers), ",")
}

// MatchTrigger reports the first trigger token contained in toolName (both
// canonicalized: case-insensitive, underscores collapsed), and whether any
// matched. toolName need not be pre-normalized.
func (d Domain) MatchTrigger(toolName string) (string, bool) {
	cname := canonTool(toolName)
	for _, t := range d.TriggerTokens() {
		if strings.Contains(cname, t) {
			return t, true
		}
	}
	return "", false
}

// normalizeKeywords canonicalizes a comma-delimited keyword-trigger list: trims
// and lowercases each phrase, drops empties, rejoins with single commas. Unlike
// tool triggers these are matched against free-form message text, so '*' and
// underscores are NOT special — a phrase may contain spaces (e.g. "morning
// routine") and is matched whole.
func normalizeKeywords(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.ToLower(strings.TrimSpace(p)); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// KeywordPhrases returns the domain's normalized keyword-trigger phrases.
func (d Domain) KeywordPhrases() []string {
	if d.KeywordTriggers == "" {
		return nil
	}
	return strings.Split(normalizeKeywords(d.KeywordTriggers), ",")
}

// MatchKeyword reports the first keyword phrase that appears in text as a whole
// phrase on word boundaries (case-insensitive), and whether any matched. The
// boundary check means a phrase like "git" matches the word "git" but not
// "legitimate", and "morning routine" matches only that contiguous phrase.
func (d Domain) MatchKeyword(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, p := range d.KeywordPhrases() {
		if phraseMatch(lower, p) {
			return p, true
		}
	}
	return "", false
}

// phraseMatch reports whether phrase occurs in text (both lowercase) bounded by
// non-word characters (start/end of string count as boundaries).
func phraseMatch(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	for start := 0; start <= len(text)-len(phrase); {
		i := strings.Index(text[start:], phrase)
		if i < 0 {
			return false
		}
		i += start
		beforeOK := i == 0 || !isWordByte(text[i-1])
		end := i + len(phrase)
		afterOK := end == len(text) || !isWordByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		start = i + 1
	}
	return false
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func scanDomain(sc scanner) (Domain, error) {
	var (
		d                         Domain
		typ, status, stateJSON    string
		createdAt, updatedAt      int64
		lastActivePtr, archivedAt *int64
	)
	err := sc.Scan(&d.ID, &d.AgentID, &d.SessionKey, &typ, &d.Name, &status,
		&d.Version, &d.Summary, &stateJSON, &d.SchemaName, &d.SchemaVersion,
		&lastActivePtr, &d.Triggers, &d.KeywordTriggers, &createdAt, &updatedAt, &archivedAt)
	if err != nil {
		return Domain{}, err
	}
	// The legacy "type" column now holds a sticky priority (int as text); a
	// non-numeric legacy value ("project"/"workflow"/…) parses to 0 (not sticky).
	d.StickyPriority, _ = strconv.Atoi(strings.TrimSpace(typ))
	d.Status = Status(status)
	if err := json.Unmarshal([]byte(stateJSON), &d.State); err != nil {
		return Domain{}, fmt.Errorf("cogmem: bad state_json for %s: %w", d.ID, err)
	}
	d.LastActiveAt = derefOr0(lastActivePtr)
	d.CreatedAt = timeUnix(createdAt)
	d.UpdatedAt = timeUnix(updatedAt)
	d.ArchivedAt = unixPtr(archivedAt)
	return d, nil
}
