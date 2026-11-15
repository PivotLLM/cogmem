// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeKeywordsDedupes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{" , ,", ""},
		{"Morning Routine, morning routine ,MORNING ROUTINE", "morning routine"},
		{"b, a, b, c, a", "b,a,c"},
	} {
		if got := normalizeKeywords(tc.in); got != tc.want {
			t.Errorf("normalizeKeywords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Routing matches the union of the agent's and consolidation's lists, deduped,
// agent first; the blocked list never routes.
func TestKeywordPhrasesUnion(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Domain
		want []string
	}{
		{"none", Domain{}, nil},
		{"agent only", Domain{KeywordTriggers: "invoice run"}, []string{"invoice run"}},
		{"auto only", Domain{AutoKeywords: "vendor bills"}, []string{"vendor bills"}},
		{
			"both, overlap deduped",
			Domain{KeywordTriggers: "invoice run,ledger", AutoKeywords: "Ledger,vendor bills"},
			[]string{"invoice run", "ledger", "vendor bills"},
		},
		{"blocked excluded", Domain{AutoKeywords: "vendor bills", BlockedKeywords: "ledger close"}, []string{"vendor bills"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.KeywordPhrases(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("KeywordPhrases = %q, want %q", got, tc.want)
			}
		})
	}
}

// MatchKeywords returns every distinct phrase found, once, in list order.
func TestMatchKeywords(t *testing.T) {
	d := Domain{KeywordTriggers: "git,invoice run", AutoKeywords: "invoice run,vendor bills,po"}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"", nil},
		{"nothing relevant", nil},
		{"git git git", []string{"git"}},
		{"Vendor bills before the INVOICE RUN, then git push", []string{"git", "invoice run", "vendor bills"}},
		{"legitimate depot", nil}, // word boundaries: not "git", not "po"
		{"the po.", []string{"po"}},
	} {
		if got := d.MatchKeywords(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("MatchKeywords(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// Rule (a): a phrase is too common when every word in it is a stoplist word.
func TestCommonPhrase(t *testing.T) {
	for _, tc := range []struct {
		phrase string
		common bool
	}{
		{"today", true},
		{"good", true},
		{"good morning", true},
		{"thank you", true},
		{"see you tomorrow", true},
		{"don't", true},
		{"don’t", true},
		{"'okay'", true},
		{"!!!", true}, // no words at all
		{"morning routine", false},
		{"invoice", false},
		{"weekly report", false},
		{"the kestrel", false},
		{"c++", false},
		{"2024 budget", false},
		{"monday standup", false},
	} {
		if got := commonPhrase(tc.phrase); got != tc.common {
			t.Errorf("commonPhrase(%q) = %v, want %v", tc.phrase, got, tc.common)
		}
	}
}

// Rule (b): a phrase already used by maxDomainsPerKeyword or more OTHER active
// domains is refused; the checked domain itself and archived domains do not
// count, and consolidation's lists count like the agent's.
func TestCheckKeywordsSharedUse(t *testing.T) {
	const phrase = "release train"
	for _, tc := range []struct {
		name    string
		others  []CreateDomainParams
		archive int // how many of others to archive
		self    bool
		reject  string
	}{
		{"two others allowed", []CreateDomainParams{{KeywordTriggers: phrase}, {AutoKeywords: phrase}}, 0, false, ""},
		{
			"three others refused",
			[]CreateDomainParams{{KeywordTriggers: phrase}, {AutoKeywords: phrase}, {KeywordTriggers: phrase}},
			0, false,
			"already used by 3 domains",
		},
		{"archived do not count", []CreateDomainParams{{KeywordTriggers: phrase}, {AutoKeywords: phrase}, {KeywordTriggers: phrase}}, 1, false, ""},
		{"own use does not count", []CreateDomainParams{{KeywordTriggers: phrase}, {AutoKeywords: phrase}}, 0, true, ""},
		{"blocked does not count", []CreateDomainParams{{KeywordTriggers: phrase}, {AutoKeywords: phrase}, {BlockedKeywords: phrase}}, 0, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			for i, p := range tc.others {
				p.Name = "Other " + string(rune('A'+i))
				d, err := s.CreateDomain(ctx, s.DB(), p)
				if err != nil {
					t.Fatal(err)
				}
				if i < tc.archive {
					if err := s.ArchiveDomain(ctx, s.DB(), d.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			self := CreateDomainParams{Name: "Self"}
			if tc.self {
				self.KeywordTriggers = phrase
			}
			d, err := s.CreateDomain(ctx, s.DB(), self)
			if err != nil {
				t.Fatal(err)
			}
			accepted, rejected, err := s.CheckKeywords(ctx, s.DB(), d.ID, []string{"Release Train", "kestrel"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.reject == "" {
				if len(rejected) != 0 || !reflect.DeepEqual(accepted, []string{phrase, "kestrel"}) {
					t.Fatalf("accepted %q rejected %+v, want both accepted", accepted, rejected)
				}
				return
			}
			want := []KeywordRejection{{Phrase: phrase, Reason: tc.reject}}
			if !reflect.DeepEqual(rejected, want) || !reflect.DeepEqual(accepted, []string{"kestrel"}) {
				t.Fatalf("accepted %q rejected %+v, want %+v", accepted, rejected, want)
			}
		})
	}
}

// CheckKeywords normalizes and dedupes its input and reports each refusal.
func TestCheckKeywordsNormalizesAndReports(t *testing.T) {
	s := openTest(t)
	accepted, rejected, err := s.CheckKeywords(context.Background(), s.DB(), "",
		[]string{"  Invoice Run ", "invoice run", "Today", "good morning, morning routine", ""})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(accepted, []string{"invoice run", "morning routine"}) {
		t.Errorf("accepted = %q", accepted)
	}
	want := []KeywordRejection{{"today", "too common"}, {"good morning", "too common"}}
	if !reflect.DeepEqual(rejected, want) {
		t.Errorf("rejected = %+v, want %+v", rejected, want)
	}
}

// setKeywords applies KeywordPatch with UpdateDomain in one transaction, the
// way the domain_update tool does.
func setKeywords(t *testing.T, s *Store, id string, phrases []string) ([]KeywordRejection, error) {
	t.Helper()
	ctx := context.Background()
	var rejected []KeywordRejection
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		p, rej, perr := s.KeywordPatch(ctx, tx, id, phrases)
		if perr != nil {
			return perr
		}
		rejected = rej
		return s.UpdateDomain(ctx, tx, id, p)
	})
	return rejected, err
}

// KeywordPatch replaces the agent's single view of the list and sorts each
// phrase into the right owner.
func TestKeywordPatch(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		agent, auto, blocked string // before
		set                  []string
		wantAgent, wantAuto  string
		wantBlocked          string
		wantRejected         []string
	}{
		{
			name:  "keeps auto, adds agent, blocks dropped auto",
			agent: "invoice run", auto: "accounts payable,vendor bills",
			set:       []string{"Invoice Run", "accounts payable", "purchase order"},
			wantAgent: "invoice run,purchase order", wantAuto: "accounts payable", wantBlocked: "vendor bills",
		},
		{
			name: "clearing blocks every auto phrase and keeps old blocks",
			auto: "vendor bills,ledger", blocked: "ledger close",
			set:         nil,
			wantBlocked: "ledger close,vendor bills,ledger",
		},
		{
			name:      "re-adding a blocked phrase unblocks it as the agent's",
			blocked:   "vendor bills,ledger close",
			set:       []string{"vendor bills"},
			wantAgent: "vendor bills", wantBlocked: "ledger close",
		},
		{
			name:      "normalizes and dedupes",
			set:       []string{"  Purchase Order ", "purchase order", "PURCHASE ORDER,po number", ""},
			wantAgent: "purchase order,po number",
		},
		{
			name:  "a phrase in both lists stays consolidation's",
			agent: "invoice run", auto: "invoice run",
			set:      []string{"invoice run"},
			wantAuto: "invoice run",
		},
		{
			name:  "common phrases are refused, including kept legacy ones",
			agent: "today,invoice run", auto: "good morning",
			set:          []string{"today", "invoice run", "good morning", "thanks"},
			wantAgent:    "invoice run",
			wantRejected: []string{"today", "good morning", "thanks"},
		},
		{
			name:         "re-adding a blocked phrase that is generic is refused and it stays blocked",
			blocked:      "today",
			set:          []string{"today"},
			wantBlocked:  "today",
			wantRejected: []string{"today"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
				Name: "Billing", KeywordTriggers: tc.agent, AutoKeywords: tc.auto, BlockedKeywords: tc.blocked,
			})
			if err != nil {
				t.Fatal(err)
			}
			rejected, err := setKeywords(t, s, d.ID, tc.set)
			if err != nil {
				t.Fatalf("set keywords: %v", err)
			}
			got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
			if got.KeywordTriggers != tc.wantAgent || got.AutoKeywords != tc.wantAuto || got.BlockedKeywords != tc.wantBlocked {
				t.Fatalf("agent=%q auto=%q blocked=%q, want %q %q %q",
					got.KeywordTriggers, got.AutoKeywords, got.BlockedKeywords, tc.wantAgent, tc.wantAuto, tc.wantBlocked)
			}
			var phrases []string //nolint:prealloc // nil when nothing was rejected, to compare with a nil want
			for _, r := range rejected {
				phrases = append(phrases, r.Phrase)
			}
			if !reflect.DeepEqual(phrases, tc.wantRejected) {
				t.Fatalf("rejected = %+v, want %q", rejected, tc.wantRejected)
			}
		})
	}
}

// A phrase the domain already routes on is not refused for being shared, but
// adding a widely shared phrase to another domain is.
func TestKeywordPatchSharedUseOnlyChecksNewPhrases(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const phrase = "release train"
	keeper, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Keeper", KeywordTriggers: phrase})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"A", "B", "C"} {
		if _, cerr := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: n, AutoKeywords: phrase}); cerr != nil {
			t.Fatal(cerr)
		}
	}
	newcomer, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Newcomer"})
	if err != nil {
		t.Fatal(err)
	}

	rejected, err := setKeywords(t, s, keeper.ID, []string{phrase, "kestrel"})
	if err != nil || len(rejected) != 0 {
		t.Fatalf("keeping an existing phrase: rejected %+v err %v", rejected, err)
	}
	rejected, err = setKeywords(t, s, newcomer.ID, []string{phrase})
	if err != nil {
		t.Fatal(err)
	}
	if want := []KeywordRejection{{phrase, "already used by 4 domains"}}; !reflect.DeepEqual(rejected, want) {
		t.Fatalf("rejected = %+v, want %+v", rejected, want)
	}
	if got, _ := s.GetDomain(ctx, s.DB(), newcomer.ID, false); got.KeywordTriggers != "" {
		t.Fatalf("refused phrase was stored: %q", got.KeywordTriggers)
	}
}

func TestKeywordPatchUnknownDomain(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.KeywordPatch(context.Background(), s.DB(), "dNOPE", []string{"kestrel"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A v8 database (no auto_keywords / blocked_keywords) opens: the columns are
// added empty, stored keywords are left exactly as they were, the version is
// recorded, a pre-v8 snapshot is taken, and opening again changes nothing.
func TestMigrationV9AddsKeywordColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Legacy", KeywordTriggers: "today,release train"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE domains DROP COLUMN auto_keywords`,
		`ALTER TABLE domains DROP COLUMN blocked_keywords`,
		`DELETE FROM schema_migrations`,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(8, 1)`,
	} {
		if _, xerr := raw.Exec(q); xerr != nil {
			t.Fatalf("%s: %v", q, xerr)
		}
	}
	_ = raw.Close()

	for i := range 2 {
		s, err = Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		cols, err := s.columnSet(ctx, "domains")
		if err != nil {
			t.Fatal(err)
		}
		if !cols["auto_keywords"] || !cols["blocked_keywords"] {
			t.Fatalf("open %d: columns = %v", i, cols)
		}
		got, err := s.GetDomain(ctx, s.DB(), d.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.KeywordTriggers != "today,release train" || got.AutoKeywords != "" || got.BlockedKeywords != "" {
			t.Fatalf("open %d: domain = %+v", i, got)
		}
		if v, _ := s.recordedVersion(ctx); v != schemaVersion || schemaVersion != 9 {
			t.Fatalf("open %d: recorded version %d, want 9", i, v)
		}
		_ = s.Close()
	}
	if _, err := os.Stat(path + ".pre-v8.db"); err != nil {
		t.Errorf("pre-v8 snapshot missing: %v", err)
	}
}

// A patch that sets only consolidation's keyword list does not mark the domain
// active; any other change does.
func TestUpdateDomainAutoKeywordsOnlyDoesNotTouch(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	db := st.DB()
	d, err := st.CreateDomain(ctx, db, CreateDomainParams{Name: "Billing"})
	if err != nil {
		t.Fatal(err)
	}
	const cold = 1000
	setCold := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE domains SET last_active_at=? WHERE id=?`, cold, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	lastActive := func() int64 {
		t.Helper()
		got, err := st.GetDomain(ctx, db, d.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastActiveAt
	}
	auto, summary := "vendor bills", "Invoices and payments"

	setCold()
	if err := st.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{AutoKeywords: &auto}); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(); got != cold {
		t.Errorf("auto-keywords-only update: last_active_at = %d, want unchanged %d", got, cold)
	}

	if err := st.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{AutoKeywords: &auto, Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(); got == cold {
		t.Error("update with a summary change: last_active_at not advanced")
	}

	setCold()
	if err := st.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{KeywordTriggers: &auto}); err != nil {
		t.Fatal(err)
	}
	if got := lastActive(); got == cold {
		t.Error("agent keyword update: last_active_at not advanced")
	}
}
