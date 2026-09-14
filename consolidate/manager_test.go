// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/store"
)

// newTestManager builds a Manager with the runFn replaced by record, so no real
// Worker/factory is needed.
func newTestManager(t *testing.T, record func(j Job, trigger string), opts ...ManagerOption) *Manager {
	t.Helper()
	m := NewManager(func(Job) (*Worker, error) { return nil, nil }, opts...)
	m.runFn = func(_ context.Context, j Job, trigger string) { record(j, trigger) }
	return m
}

// fakeClock is an injectable Manager.now that is safe under -race. Every read
// is reported on ticks so a test can prove a loop has polled it since some
// point, rather than sleeping and hoping it did.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	ticks chan struct{}
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t, ticks: make(chan struct{}, 1024)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	t := c.t
	c.mu.Unlock()
	select {
	case c.ticks <- struct{}{}:
	default:
	}
	return t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// awaitReads discards the reads recorded so far, then blocks until n further
// reads have happened.
func (c *fakeClock) awaitReads(t *testing.T, n int) {
	t.Helper()
drain:
	for {
		select {
		case <-c.ticks:
		default:
			break drain
		}
	}
	for i := 0; i < n; i++ {
		select {
		case <-c.ticks:
		case <-time.After(waitTimeout):
			t.Fatalf("timeout: clock read %d of %d never happened", i+1, n)
		}
	}
}

// recv receives one value from ch or fails the test after waitTimeout.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timeout waiting for %s", what)
	}
	var zero T
	return zero
}

// waitUntil polls cond until it holds or waitTimeout passes. It is a bounded
// wait on a condition, not an assertion by sleeping.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// openDirStore opens a store the way a host's WorkerFactory would, at
// store.DBPath(dir).
func openDirStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(store.DBPath(dir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// countRuns counts consolidation_runs rows with the given trigger. LastRun only
// shows the newest record, which cannot prove how many runs a loop made.
func countRuns(t *testing.T, s *store.Store, trigger string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM consolidation_runs WHERE trigger=?`, trigger).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

func TestOnMessageFiresAtThreshold(t *testing.T) {
	var mu sync.Mutex
	var fired []string
	done := make(chan struct{}, 1)
	m := newTestManager(t, func(_ Job, trigger string) {
		mu.Lock()
		fired = append(fired, trigger)
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	}, WithEveryNMessages(3))
	m.Start(context.Background())
	defer m.Stop()

	job := Job{ID: "a", Dir: "/tmp/a/cogmem", Workspace: "/tmp/a"}
	m.OnMessage(job) // 1
	m.OnMessage(job) // 2
	// OnMessage decides synchronously: a fire would have reset the counter and
	// pushed to the queue, so an unreset counter proves nothing fired.
	m.mu.Lock()
	count := m.sessions[job.key()].count
	m.mu.Unlock()
	if count != 2 {
		t.Fatalf("count after 2 messages = %d, want 2 (fired before threshold)", count)
	}
	select {
	case <-done:
		t.Fatal("fired before threshold")
	default:
	}
	m.OnMessage(job) // 3 → fire

	recv(t, done, "threshold fire")
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != "message" {
		t.Fatalf("expected one message-trigger run, got %v", fired)
	}
}

func TestEnqueueRunsJob(t *testing.T) {
	type run struct {
		job     Job
		trigger string
	}
	done := make(chan run, 1)
	m := newTestManager(t, func(j Job, trigger string) { done <- run{j, trigger} })
	m.Start(context.Background())
	defer m.Stop()

	job := Job{ID: "a", Dir: "/tmp/a/cogmem", Workspace: "/tmp/a"}
	m.Enqueue(job, "manual")
	got := recv(t, done, "enqueued job")
	if got.job != job {
		t.Fatalf("job = %+v, want %+v (Dir and Workspace must reach the run)", got.job, job)
	}
	if got.trigger != "manual" {
		t.Fatalf("trigger = %q, want manual", got.trigger)
	}
}

// The default runFn builds a Worker through the factory and loops RunOnce until
// More is false. With a one-message batch cap and three messages that is three
// runs, each seeing the next message, all recorded under the enqueue trigger.
func TestRunJobDrainsInboxThroughRealWorker(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	const instructions = "Never record anything about medical matters."
	if err := os.WriteFile(filepath.Join(ws, PromptFilename), []byte(instructions), 0o600); err != nil {
		t.Fatalf("write COGMEM.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("The user is Eric."), 0o600); err != nil {
		t.Fatalf("write USER.md: %v", err)
	}
	s := openDirStore(t, dir)
	msgs := []Message{
		{Seq: 1, Role: "user", Text: "Please always run gofmt before committing."},
		{Seq: 2, Role: "assistant", Text: "Understood, I'll run gofmt first."},
		{Seq: 3, Role: "user", Text: "And run make test as well."},
	}
	seedInbox(t, s, msgs)

	model := &fakeModel{
		raw:    `{"domain_ops":[],"memory_ops":[],"conflict_ledger":[]}`,
		called: make(chan struct{}, 16),
	}
	var mu sync.Mutex
	var built []Job
	factory := func(j Job) (*Worker, error) {
		mu.Lock()
		built = append(built, j)
		mu.Unlock()
		return NewWorker(s, model, WithBatchOptions(BatchOptions{MaxMessages: 1})), nil
	}
	m := NewManager(factory)
	m.Start(context.Background())

	job := Job{ID: "a", Dir: dir, Workspace: ws}
	m.Enqueue(job, "manual")
	for i := 0; i < len(msgs); i++ {
		recv(t, model.called, "model call")
	}
	// Stop waits for the in-flight job, so everything below is settled.
	m.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(built) != 1 || built[0] != job {
		t.Fatalf("factory calls = %+v, want exactly one with %+v", built, job)
	}
	if n := model.calls(); n != len(msgs) {
		t.Fatalf("model calls = %d, want %d (one per message with MaxMessages 1)", n, len(msgs))
	}
	if left := inboxSeqs(t, s); len(left) != 0 {
		t.Fatalf("inbox after the loop = %v, want fully drained", left)
	}
	state, err := s.GetState(context.Background(), s.DB(), store.InboxStateKey)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.ConsolidatedSeq != 3 {
		t.Fatalf("consolidated_seq = %d, want 3", state.ConsolidatedSeq)
	}
	if n := countRuns(t, s, "manual"); n != 3 {
		t.Fatalf("manual run records = %d, want 3", n)
	}
	for i, req := range model.snapshot() {
		if !strings.Contains(req.System, instructions) {
			t.Errorf("request %d: system prompt lacks the workspace COGMEM.md text", i)
		}
		var in Input
		if err := json.Unmarshal([]byte(req.User), &in); err != nil {
			t.Fatalf("request %d: user payload is not an Input: %v", i, err)
		}
		if in.Curated.UserMD != "The user is Eric." {
			t.Errorf("request %d: USER.md = %q, want the workspace file", i, in.Curated.UserMD)
		}
		if len(in.NewMessages) != 1 || in.NewMessages[0] != msgs[i] {
			t.Errorf("request %d: new_messages = %+v, want exactly %+v", i, in.NewMessages, msgs[i])
		}
	}
}

func TestRunJobFactoryErrorIsLoggedAndSkipped(t *testing.T) {
	logs := installCapture(t)
	var calls atomic.Int32
	m := NewManager(func(Job) (*Worker, error) {
		calls.Add(1)
		return nil, errors.New("no model configured")
	})
	m.Start(context.Background())

	m.Enqueue(Job{ID: "a", Dir: "/tmp/a"}, "manual")
	e := logs.wait(t, "consolidation worker factory failed")
	m.Stop()

	if e.level != "warn" {
		t.Errorf("level = %q, want warn", e.level)
	}
	if e.fields["id"] != "a" || e.fields["dir"] != "/tmp/a" || e.fields["error"] != "no model configured" {
		t.Errorf("fields = %v, want id, dir and the factory's error", e.fields)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("factory calls = %d, want 1", n)
	}
	if n := logs.count("consolidation run starting"); n != 0 {
		t.Errorf("a run started after the factory failed (%d)", n)
	}
	if n := logs.count("consolidation run finished"); n != 0 {
		t.Errorf("a run finished after the factory failed (%d)", n)
	}
}

// A busy result means another owner holds the lease; the loop must stop after
// that one attempt rather than spin on it.
func TestRunJobStopsOnBusy(t *testing.T) {
	logs := installCapture(t)
	dir := t.TempDir()
	s := openDirStore(t, dir)
	seedInbox(t, s, sampleMessages())
	ok, err := s.AcquireLease(context.Background(), s.DB(), leaseName, "other", leaseTTL)
	if err != nil || !ok {
		t.Fatalf("pre-acquire lease: ok=%v err=%v", ok, err)
	}
	model := &fakeModel{raw: `{"domain_ops":[],"memory_ops":[],"conflict_ledger":[]}`}
	m := NewManager(func(Job) (*Worker, error) { return NewWorker(s, model), nil })
	m.Start(context.Background())

	m.Enqueue(Job{ID: "a", Dir: dir}, "manual")
	e := logs.wait(t, "consolidation run finished")
	if e.fields["status"] != "busy" {
		t.Fatalf("first run status = %v, want busy", e.fields["status"])
	}
	m.Stop()

	if n := logs.count("consolidation run finished"); n != 1 {
		t.Errorf("RunOnce attempts = %d, want exactly 1", n)
	}
	if n := model.calls(); n != 0 {
		t.Errorf("model calls = %d, want 0 (busy never reaches the model)", n)
	}
	if left := inboxSeqs(t, s); len(left) != 2 {
		t.Errorf("inbox = %v, want untouched", left)
	}
}

func TestConcurrencyCapRespected(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 10)

	m := NewManager(func(Job) (*Worker, error) { return nil, nil }, WithConcurrency(2))
	m.runFn = func(_ context.Context, j Job, _ string) {
		started <- j.ID
		<-release
	}
	m.Start(context.Background())
	defer m.Stop()

	// Three jobs on DISTINCT directories (per-directory de-dup would otherwise
	// collapse them).
	for _, id := range []string{"a", "b", "c"} {
		m.Enqueue(Job{ID: id, Dir: "/tmp/" + id}, "manual")
	}
	recv(t, started, "first job start")
	recv(t, started, "second job start")

	// The dispatcher marks a job in flight before it waits for a pool slot, so
	// three in-flight keys with both slots held is the third job proven blocked.
	waitUntil(t, "third job to block on the pool", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.inflight) == 3 && len(m.sem) == cap(m.sem)
	})
	select {
	case id := <-started:
		t.Fatalf("job %q started while both slots were held", id)
	default:
	}

	release <- struct{}{} // one slot frees
	recv(t, started, "third job start after a slot freed")
	close(release)
}

func TestPerStoreDedup(t *testing.T) {
	logs := installCapture(t)
	var calls int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	m := NewManager(func(Job) (*Worker, error) { return nil, nil }, WithConcurrency(4))
	m.runFn = func(_ context.Context, _ Job, _ string) {
		atomic.AddInt32(&calls, 1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	m.Start(context.Background())
	defer m.Stop()

	job := Job{ID: "same", Dir: "/tmp/same"}
	m.Enqueue(job, "manual")
	recv(t, started, "first run start")
	// Subsequent enqueues for the same archive are skipped by the dispatcher,
	// and each skip is logged.
	m.Enqueue(job, "manual")
	m.Enqueue(job, "idle")
	for i := 0; i < 2; i++ {
		e := logs.wait(t, "consolidation job skipped; run already in flight")
		if e.fields["dir"] != job.Dir {
			t.Errorf("skip %d logged dir %v, want %q", i, e.fields["dir"], job.Dir)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 in-flight run for same archive, got %d", got)
	}
	close(release)
}

// Stop must not return while a job is running: the host closes the store after
// Stop, and a run still writing to it would fail or corrupt.
func TestStopWaitsForInFlightJob(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var order []string
	m := NewManager(func(Job) (*Worker, error) { return nil, nil })
	m.runFn = func(_ context.Context, _ Job, _ string) {
		started <- struct{}{}
		<-release
		mu.Lock()
		order = append(order, "job finished")
		mu.Unlock()
	}
	m.Start(context.Background())
	m.Enqueue(Job{ID: "a", Dir: "/tmp/a"}, "manual")
	recv(t, started, "job start")

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		mu.Lock()
		order = append(order, "stop returned")
		mu.Unlock()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the job was still running")
	default:
	}
	close(release)
	recv(t, stopped, "Stop to return")

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "job finished" || order[1] != "stop returned" {
		t.Fatalf("order = %v, want [job finished, stop returned]", order)
	}
}

// Start adds the idle and nightly loops to the wait group; each must Done on
// stop or Stop hangs forever.
func TestStopWithIdleAndNightlyLoops(t *testing.T) {
	m := newTestManager(t, func(Job, string) {}, WithIdle(time.Hour), WithNightlyAt("03:00"))
	m.Start(context.Background())
	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	recv(t, stopped, "Stop with idle and nightly loops running")
}

func TestIdleLoop(t *testing.T) {
	type fire struct {
		trigger string
		dir     string
	}
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.Local)
	clk := newFakeClock(base)
	fired := make(chan fire, 16)
	m := newTestManager(t, func(j Job, trigger string) { fired <- fire{trigger, j.Dir} },
		WithIdle(time.Minute), WithIdlePollInterval(time.Millisecond))
	m.now = clk.now

	active := Job{ID: "a", Dir: "/tmp/a"}
	quiet := Job{ID: "b", Dir: "/tmp/b"}
	m.OnMessage(active)        // count 1 at base
	m.Enqueue(quiet, "manual") // known to the manager, count stays 0
	m.Start(context.Background())
	if got := recv(t, fired, "manual run"); got != (fire{"manual", quiet.Dir}) {
		t.Fatalf("first run = %+v, want the manual enqueue", got)
	}

	// Not idle yet: the clock has not moved. Several polls happen and none
	// fires; then the clock passes the idle threshold and exactly one does.
	clk.awaitReads(t, 3)
	select {
	case got := <-fired:
		t.Fatalf("fired %+v before the session was idle", got)
	default:
	}
	clk.set(base.Add(2 * time.Minute))
	if got := recv(t, fired, "idle fire"); got != (fire{"idle", active.Dir}) {
		t.Fatalf("idle fire = %+v, want idle for %q", got, active.Dir)
	}
	m.mu.Lock()
	st := m.sessions[active.key()]
	count, idleEnqueued := st.count, st.idleEnqueued
	m.mu.Unlock()
	if count != 0 || !idleEnqueued {
		t.Fatalf("after idle fire: count=%d idleEnqueued=%v, want 0/true", count, idleEnqueued)
	}

	// Further polls see idleEnqueued and do not fire again. The quiet session
	// is idle by the same clock but has count 0, so it never fires.
	clk.awaitReads(t, 3)
	select {
	case got := <-fired:
		t.Fatalf("fired %+v again without new activity", got)
	default:
	}

	// New activity re-arms the idle trigger.
	m.OnMessage(active)
	clk.set(base.Add(5 * time.Minute))
	if got := recv(t, fired, "second idle fire"); got != (fire{"idle", active.Dir}) {
		t.Fatalf("second idle fire = %+v, want idle for %q", got, active.Dir)
	}
	m.Stop()
	select {
	case got := <-fired:
		t.Fatalf("unexpected extra fire %+v", got)
	default:
	}
}

func TestNightlyLoopEnqueuesEverySession(t *testing.T) {
	logs := installCapture(t)
	base := time.Date(2026, 6, 14, 2, 59, 59, 990_000_000, time.Local)
	clk := newFakeClock(base)
	fired := make(chan Job, 16)
	m := newTestManager(t, func(j Job, trigger string) {
		if trigger == "nightly" {
			fired <- j
		}
	}, WithNightlyAt("03:00"))
	m.now = clk.now
	// The loop re-arms right after enqueuing. Move the clock past 03:00 inside
	// that enqueue so the next timer is tomorrow's, not another 10ms.
	logs.onLog = func(e logEntry) {
		if e.message == "consolidation job enqueued" && e.fields["trigger"] == "nightly" {
			clk.set(base.Add(2 * time.Second))
		}
	}

	a := Job{ID: "a", Dir: "/tmp/a", Workspace: "/ws/a"}
	b := Job{ID: "b", Dir: "/tmp/b", Workspace: "/ws/b"}
	m.OnMessage(a) // known sessions; no message threshold, so nothing fires
	m.OnMessage(b)
	m.Start(context.Background())

	got := map[string]Job{}
	for i := 0; i < 2; i++ {
		j := recv(t, fired, "nightly fire")
		if _, dup := got[j.Dir]; dup {
			t.Fatalf("session %q fired nightly twice", j.Dir)
		}
		got[j.Dir] = j
	}
	m.Stop()
	if got[a.Dir] != a || got[b.Dir] != b {
		t.Fatalf("nightly jobs = %v, want both %+v and %+v", got, a, b)
	}
	select {
	case j := <-fired:
		t.Fatalf("extra nightly fire for %+v", j)
	default:
	}
}

func TestParseHHMM(t *testing.T) {
	hh, mm := parseHHMM("02:30")
	if hh != 2 || mm != 30 {
		t.Fatalf("parseHHMM(02:30)=%d:%d", hh, mm)
	}
	hh, mm = parseHHMM("garbage")
	if hh != 3 || mm != 0 {
		t.Fatalf("parseHHMM(garbage) want 3:00 got %d:%d", hh, mm)
	}
}

func TestNextNightly(t *testing.T) {
	m := NewManager(nil, WithNightlyAt("03:15"))

	// After today's slot: tomorrow at 03:15 exactly.
	from := time.Date(2026, 6, 14, 10, 0, 0, 0, time.Local)
	got := m.nextNightly(from)
	want := time.Date(2026, 6, 15, 3, 15, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("nextNightly(%v) = %v, want %v", from, got, want)
	}

	// Before today's slot: later today.
	from = time.Date(2026, 6, 14, 1, 0, 0, 0, time.Local)
	got = m.nextNightly(from)
	want = time.Date(2026, 6, 14, 3, 15, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("nextNightly(%v) = %v, want same-day %v", from, got, want)
	}

	// Exactly at the slot counts as passed.
	from = want
	got = m.nextNightly(from)
	if !got.Equal(want.AddDate(0, 0, 1)) {
		t.Fatalf("nextNightly(at slot) = %v, want next day", got)
	}
}

func TestNextNightlyJitterStaysInWindow(t *testing.T) {
	const jitter = time.Hour
	m := NewManager(nil, WithNightlyAt("03:00"), WithNightlyJitter(jitter))
	from := time.Date(2026, 6, 14, 10, 0, 0, 0, time.Local)
	base := time.Date(2026, 6, 15, 3, 0, 0, 0, time.Local)
	for i := 0; i < 50; i++ {
		got := m.nextNightly(from)
		if got.Before(base) || !got.Before(base.Add(jitter)) {
			t.Fatalf("nextNightly = %v, want within [%v, %v)", got, base, base.Add(jitter))
		}
	}
}
