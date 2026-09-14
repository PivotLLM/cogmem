// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openPair opens two independent Store instances on one file, the way a tool
// handler and a host session (or two processes) do.
func openPair(t *testing.T) (a, b *Store, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "shared.cogmem.db")
	var err error
	if a, err = Open(path); err != nil {
		t.Fatalf("open a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if b, err = Open(path); err != nil {
		t.Fatalf("open b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return a, b, path
}

// Two stores on one file see each other's writes, and the second open neither
// re-seeds General nor takes a snapshot (nothing to migrate).
func TestTwoStoresOnOneFileShareWrites(t *testing.T) {
	a, b, path := openPair(t)
	ctx := context.Background()

	d, err := a.CreateDomain(ctx, a.DB(), CreateDomainParams{Name: "Shared", Summary: "from a"})
	if err != nil {
		t.Fatalf("create via a: %v", err)
	}
	got, err := b.GetDomain(ctx, b.DB(), d.ID, false)
	if err != nil || got.Summary != "from a" {
		t.Fatalf("b cannot see a's domain: %+v err=%v", got, err)
	}
	m, err := b.AddMemory(ctx, b.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "from b", Status: StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add via b: %v", err)
	}
	if gm, err := a.GetMemory(ctx, a.DB(), m.ID); err != nil || gm.Text != "from b" {
		t.Fatalf("a cannot see b's memory: %+v err=%v", gm, err)
	}
	// Stable rev is shared state too: both handles read the same counter.
	ra, _ := a.StableRev(ctx)
	rb, _ := b.StableRev(ctx)
	if ra != rb {
		t.Fatalf("stable_rev differs between handles: %d vs %d", ra, rb)
	}

	doms, _ := a.ListDomains(ctx, a.DB())
	if len(doms) != 2 {
		t.Fatalf("domains = %d, want 2 (General + Shared): the second open re-seeded", len(doms))
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pre-v") {
			t.Errorf("a second open of a current database took a snapshot: %s", e.Name())
		}
	}
}

// Open passes its pragmas through the DSN, so every connection database/sql
// opens under load carries busy_timeout and foreign_keys — not only the first.
// Two connections held at once force a second physical connection.
func TestEveryPooledConnectionCarriesPragmas(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	first, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("conn 1: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := s.DB().Conn(ctx) // forces a second physical connection
	if err != nil {
		t.Fatalf("conn 2: %v", err)
	}
	defer func() { _ = second.Close() }()

	var bt1, bt2, fk1, fk2 int64
	if err := first.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&bt1); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&bt2); err != nil {
		t.Fatal(err)
	}
	if err := first.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk1); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk2); err != nil {
		t.Fatal(err)
	}
	if bt1 != 5000 || fk1 != 1 {
		t.Errorf("first connection busy_timeout=%d foreign_keys=%d, want 5000/1", bt1, fk1)
	}
	if bt2 != 5000 || fk2 != 1 {
		t.Errorf("second connection busy_timeout=%d foreign_keys=%d, want 5000/1", bt2, fk2)
	}
}

// Concurrent writers through two handles and many goroutines all succeed:
// SQLite serialises them behind busy_timeout rather than failing with BUSY.
// The pools are left at their defaults, so the contention is both between the
// two handles on the file and between pooled connections within each handle.
func TestConcurrentWritersDoNotError(t *testing.T) {
	a, b, _ := openPair(t)
	ctx := context.Background()
	d, err := a.CreateDomain(ctx, a.DB(), CreateDomainParams{Name: "Hot"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		st := a
		if w%2 == 1 {
			st = b
		}
		wg.Add(1)
		go func(w int, st *Store) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				seq := int64(w*perWorker + i + 1)
				if err := st.AppendInbox(ctx, st.DB(), seq, "user", fmt.Sprintf("w%d-%d", w, i)); err != nil {
					errs <- fmt.Errorf("worker %d inbox %d: %w", w, seq, err)
					continue
				}
				if _, err := st.AddMemory(ctx, st.DB(), AddMemoryParams{
					DomainID: d.ID, Type: TypeFact, Text: fmt.Sprintf("fact %d/%d", w, i),
					Status: StatusActive, Confidence: 0.5,
				}); err != nil {
					errs <- fmt.Errorf("worker %d memory %d: %w", w, i, err)
				}
			}
		}(w, st)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n, _ := a.InboxCount(ctx, a.DB()); n != workers*perWorker {
		t.Errorf("inbox count = %d, want %d", n, workers*perWorker)
	}
	mems, _ := b.ListMemories(ctx, b.DB(), d.ID)
	if len(mems) != workers*perWorker {
		t.Errorf("memories = %d, want %d", len(mems), workers*perWorker)
	}
	ids := map[string]bool{}
	for _, m := range mems {
		if ids[m.ID] {
			t.Errorf("duplicate id %s allocated under contention", m.ID)
		}
		ids[m.ID] = true
	}
}

// A lease taken through one handle blocks the other until it is released.
func TestLeaseContentionBetweenStores(t *testing.T) {
	a, b, _ := openPair(t)
	ctx := context.Background()
	const name = "consolidate"

	if ok, err := a.AcquireLease(ctx, a.DB(), name, "proc-a", time.Minute); err != nil || !ok {
		t.Fatalf("a acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := b.AcquireLease(ctx, b.DB(), name, "proc-b", time.Minute); err != nil || ok {
		t.Fatalf("b acquired a lease a holds: ok=%v err=%v", ok, err)
	}
	if err := a.ReleaseLease(ctx, a.DB(), name, "proc-a"); err != nil {
		t.Fatalf("a release: %v", err)
	}
	if ok, err := b.AcquireLease(ctx, b.DB(), name, "proc-b", time.Minute); err != nil || !ok {
		t.Fatalf("b acquire after release: ok=%v err=%v", ok, err)
	}
	if ok, err := a.AcquireLease(ctx, a.DB(), name, "proc-a", time.Minute); err != nil || ok {
		t.Fatalf("a re-acquired a lease b holds: ok=%v err=%v", ok, err)
	}
}

// Snapshot while the source is open and its recent writes are still in the
// WAL (never checkpointed into the main file): the copy must carry them, and
// must not carry anything written after it was taken.
func TestSnapshotUnderLiveWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "live.cogmem.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Live", Summary: "in the wal"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	add(t, s, d.ID, TypeFact, "written before the snapshot")
	for seq := int64(1); seq <= 3; seq++ {
		if err := s.AppendInbox(ctx, s.DB(), seq, "user", "m"); err != nil {
			t.Fatalf("inbox: %v", err)
		}
	}
	// The writes are committed to the WAL, which has not been checkpointed.
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected a non-empty WAL beside the open source (err=%v)", err)
	}

	dst := filepath.Join(dir, "copy.cogmem.db")
	if err := Snapshot(ctx, src, dst); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Still open, still writing: this must not reach the copy.
	add(t, s, d.ID, TypeFact, "written after the snapshot")

	s2, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = s2.Close() }()
	gd, err := s2.GetDomain(ctx, s2.DB(), d.ID, true)
	if err != nil {
		t.Fatalf("snapshot lacks the domain: %v", err)
	}
	if gd.Summary != "in the wal" || len(gd.Memories) != 1 || gd.Memories[0].Text != "written before the snapshot" {
		t.Fatalf("snapshot domain = %+v memories=%+v", gd, gd.Memories)
	}
	if n, _ := s2.InboxCount(ctx, s2.DB()); n != 3 {
		t.Fatalf("snapshot inbox count = %d, want 3", n)
	}
	// The source kept going independently.
	if live, _ := s.ListMemories(ctx, s.DB(), d.ID); len(live) != 2 {
		t.Fatalf("source memories = %d, want 2", len(live))
	}
}

// Snapshot refuses to overwrite nothing: a pre-existing destination is
// removed first, so a repeated snapshot to the same path succeeds.
func TestSnapshotReplacesExistingDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.cogmem.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	dst := filepath.Join(dir, "copy.cogmem.db")
	if err := os.WriteFile(dst, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := Snapshot(ctx, src, dst); err != nil {
		t.Fatalf("snapshot over a stale file: %v", err)
	}
	s2, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, err := s2.GeneralDomain(ctx, s2.DB()); err != nil {
		t.Fatalf("snapshot content: %v", err)
	}
}

// Snapshot of a path that is not a database fails rather than producing an
// empty copy.
func TestSnapshotMissingSourceFails(t *testing.T) {
	dir := t.TempDir()
	err := Snapshot(context.Background(), filepath.Join(dir, "absent.db"), filepath.Join(dir, "copy.db"))
	if err == nil {
		t.Fatal("snapshot of a missing source succeeded")
	}
	if !strings.HasPrefix(err.Error(), "cogmem snapshot: ") {
		t.Fatalf("err = %v, want the snapshot prefix", err)
	}
}
