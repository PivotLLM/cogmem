// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestManager builds a Manager with the runFn replaced by record, so no real
// Worker/factory is needed.
func newTestManager(t *testing.T, record func(j Job, trigger string), opts ...ManagerOption) *Manager {
	t.Helper()
	m := NewManager(func(Job) (*Worker, error) { return nil, nil }, opts...)
	m.runFn = func(_ context.Context, j Job, trigger string) { record(j, trigger) }
	return m
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

	job := Job{AgentID: "a", SessionKey: "s", Workspace: "/tmp"}
	m.OnMessage(job) // 1
	m.OnMessage(job) // 2
	select {
	case <-done:
		t.Fatal("fired before threshold")
	case <-time.After(50 * time.Millisecond):
	}
	m.OnMessage(job) // 3 → fire

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for threshold fire")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != "message" {
		t.Fatalf("expected one message-trigger run, got %v", fired)
	}
}

func TestEnqueueRunsJob(t *testing.T) {
	done := make(chan Job, 1)
	m := newTestManager(t, func(j Job, _ string) { done <- j })
	m.Start(context.Background())
	defer m.Stop()

	job := Job{AgentID: "a", SessionKey: "s", Workspace: "/tmp"}
	m.Enqueue(job, "manual")
	select {
	case got := <-done:
		if got.SessionKey != "s" {
			t.Fatalf("got job %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for enqueued job")
	}
}

func TestConcurrencyCapRespected(t *testing.T) {
	var active, maxActive int32
	release := make(chan struct{})
	started := make(chan struct{}, 10)

	m := NewManager(func(Job) (*Worker, error) { return nil, nil }, WithConcurrency(2))
	m.runFn = func(_ context.Context, _ Job, _ string) {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if n <= old || atomic.CompareAndSwapInt32(&maxActive, old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
	}
	m.Start(context.Background())
	defer m.Stop()

	// Enqueue 5 jobs on DISTINCT sessions (per-store de-dup would otherwise
	// collapse same-session jobs).
	for i := 0; i < 5; i++ {
		m.Enqueue(Job{SessionKey: string(rune('a' + i)), Workspace: "/tmp"}, "manual")
	}

	// Wait until at least 2 are running.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for jobs to start")
		}
	}
	// Give a moment for any (incorrect) extra job to start.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&maxActive); got > 2 {
		t.Fatalf("concurrency cap exceeded: maxActive=%d want<=2", got)
	}
	close(release)
}

func TestPerStoreDedup(t *testing.T) {
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

	job := Job{SessionKey: "same", Workspace: "/tmp"}
	m.Enqueue(job, "manual")
	<-started // first run is now in-flight
	// Subsequent enqueues for the same archive must not start a second run.
	m.Enqueue(job, "manual")
	m.Enqueue(job, "manual")
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 in-flight run for same archive, got %d", got)
	}
	close(release)
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
	m := NewManager(nil, WithNightlyAt("03:00"))
	from := time.Date(2026, 6, 14, 10, 0, 0, 0, time.Local)
	got := m.nextNightly(from)
	if got.Hour() != 3 || !got.After(from) {
		t.Fatalf("nextNightly=%v want next-day 03:00 after %v", got, from)
	}
	if got.Day() != 15 {
		t.Fatalf("nextNightly day=%d want 15", got.Day())
	}
}
