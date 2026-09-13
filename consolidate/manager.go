// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/PivotLLM/cogmem/logger"
)

// Job identifies one memory whose inbox may need consolidation. It carries
// everything a WorkerFactory needs to build a Worker; the manager is otherwise
// decoupled from the store and provider packages.
type Job struct {
	// ID labels the memory in logs and run records (an agent id, typically).
	ID string
	// Dir is the memory's directory; the store is at store.DBPath(Dir).
	Dir string
	// Workspace is where the curated files and COGMEM.md are read from.
	Workspace string
}

// key is the identity the manager de-duplicates on: two jobs for the same
// directory are the same job.
func (j Job) key() string { return j.Dir }

// WorkerFactory builds a Worker for a Job. The host supplies it: it opens the
// store at store.DBPath(job.Dir) and selects the ModelCaller. Returning an
// error skips the job (logged, not fatal).
type WorkerFactory func(Job) (*Worker, error)

// managerOptions tune the Manager's triggers and concurrency.
type managerOptions struct {
	everyNMessages int           // message-count trigger; <=0 disables
	idle           time.Duration // idle trigger; <=0 disables
	nightlyAt      string        // "HH:MM" local; "" disables nightly
	nightlyJitter  time.Duration // random delay added to the nightly fire time
	concurrency    int           // bounded worker pool size
	idlePoll       time.Duration // how often the idle ticker checks last-activity
}

// ManagerOption configures a Manager (functional-options pattern).
type ManagerOption func(*managerOptions)

// WithEveryNMessages fires consolidation after every n meaningful messages on a
// session. n<=0 disables the message-count trigger.
func WithEveryNMessages(n int) ManagerOption {
	return func(o *managerOptions) { o.everyNMessages = n }
}

// WithIdle fires consolidation for a session that has been idle at least d since
// its last OnMessage. d<=0 disables the idle trigger.
func WithIdle(d time.Duration) ManagerOption {
	return func(o *managerOptions) { o.idle = d }
}

// WithNightlyAt enqueues all known sessions at the given local "HH:MM". Empty
// disables the nightly trigger.
func WithNightlyAt(hhmm string) ManagerOption {
	return func(o *managerOptions) { o.nightlyAt = hhmm }
}

// WithNightlyJitter spreads nightly runs over a window to avoid a thundering
// herd. Zero means fire exactly at the configured time.
func WithNightlyJitter(d time.Duration) ManagerOption {
	return func(o *managerOptions) {
		if d > 0 {
			o.nightlyJitter = d
		}
	}
}

// WithConcurrency caps how many jobs run RunOnce concurrently. <=0 → 1.
func WithConcurrency(n int) ManagerOption {
	return func(o *managerOptions) {
		if n > 0 {
			o.concurrency = n
		}
	}
}

// WithIdlePollInterval overrides how often the idle ticker scans sessions.
func WithIdlePollInterval(d time.Duration) ManagerOption {
	return func(o *managerOptions) {
		if d > 0 {
			o.idlePoll = d
		}
	}
}

// sessionState tracks per-session counters/activity for trigger evaluation.
type sessionState struct {
	job          Job
	count        int       // meaningful messages since last enqueue
	lastActivity time.Time // last OnMessage time
	idleEnqueued bool      // idle already fired since last activity
}

// Manager schedules background consolidation runs. It is decoupled from the
// store/archive/provider layers via the WorkerFactory: triggers enqueue Jobs,
// and a bounded pool drains the queue by calling factory(job).RunOnce until
// RunResult.More is false.
type Manager struct {
	factory WorkerFactory
	opt     managerOptions

	mu       sync.Mutex
	sessions map[string]*sessionState // keyed by Job.key()
	inflight map[string]bool          // Job.key() currently running (de-dup)

	queue   chan queued
	sem     chan struct{}
	stop    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup

	// runFn is the function the pool invokes per job. Defaults to runJob;
	// overridable in tests to avoid building real Workers.
	runFn func(ctx context.Context, j Job, trigger string)

	// now is overridable in tests for deterministic nightly/idle scheduling.
	now func() time.Time
}

type queued struct {
	job     Job
	trigger string
}

const (
	defaultIdlePoll    = 30 * time.Second
	defaultQueueBuffer = 256
)

// NewManager constructs a Manager. Triggers are inert until Start is called.
func NewManager(factory WorkerFactory, opts ...ManagerOption) *Manager {
	o := managerOptions{
		concurrency: 1,
		idlePoll:    defaultIdlePoll,
	}
	for _, fn := range opts {
		fn(&o)
	}
	if o.concurrency <= 0 {
		o.concurrency = 1
	}
	if o.idlePoll <= 0 {
		o.idlePoll = defaultIdlePoll
	}
	m := &Manager{
		factory:  factory,
		opt:      o,
		sessions: make(map[string]*sessionState),
		inflight: make(map[string]bool),
		queue:    make(chan queued, defaultQueueBuffer),
		sem:      make(chan struct{}, o.concurrency),
		stop:     make(chan struct{}),
		now:      time.Now,
	}
	m.runFn = m.runJob
	return m
}

// Start launches the dispatcher and trigger loops. Safe to call once.
func (m *Manager) Start(ctx context.Context) {
	m.wg.Add(1)
	go m.dispatchLoop(ctx)

	if m.opt.idle > 0 {
		m.wg.Add(1)
		go m.idleLoop(ctx)
	}
	if m.opt.nightlyAt != "" {
		m.wg.Add(1)
		go m.nightlyLoop(ctx)
	}
}

// Stop signals all loops to exit and waits for in-flight jobs to finish.
func (m *Manager) Stop() {
	m.stopped.Do(func() { close(m.stop) })
	m.wg.Wait()
}

// OnMessage records a meaningful message for a session and enqueues a
// message-count run when the threshold is reached. It is non-blocking and safe
// to call from the hot path. The host decides which sessions are eligible (it
// does not call this for ephemeral sub-agent sessions, whose memory is a
// throwaway snapshot).
func (m *Manager) OnMessage(job Job) {
	if job.Dir == "" {
		return
	}
	key := job.key()
	now := m.now()
	m.mu.Lock()
	st := m.sessions[key]
	if st == nil {
		st = &sessionState{job: job}
		m.sessions[key] = st
	}
	st.job = job
	st.count++
	st.lastActivity = now
	st.idleEnqueued = false
	fire := m.opt.everyNMessages > 0 && st.count >= m.opt.everyNMessages
	if fire {
		st.count = 0
	}
	m.mu.Unlock()

	if fire {
		m.Enqueue(job, "message")
	}
}

// Enqueue schedules a consolidation run for job with the given trigger label.
// Non-blocking: if the queue is full the job is dropped with a WARN (a later
// trigger will re-enqueue).
func (m *Manager) Enqueue(job Job, trigger string) {
	if job.Dir == "" {
		return
	}
	key := job.key()
	// Ensure the session is known so nightly/idle can find it later.
	m.mu.Lock()
	if _, ok := m.sessions[key]; !ok {
		m.sessions[key] = &sessionState{job: job, lastActivity: m.now()}
	} else {
		m.sessions[key].job = job
	}
	m.mu.Unlock()

	select {
	case m.queue <- queued{job: job, trigger: trigger}:
		logger.DebugCF("cogmem", "consolidation job enqueued", map[string]any{
			"id":      job.ID,
			"dir":     job.Dir,
			"trigger": trigger,
		})
	default:
		logger.WarnCF("cogmem", "consolidation queue full; dropping job", map[string]any{
			"id":      job.ID,
			"dir":     job.Dir,
			"trigger": trigger,
		})
	}
}

// dispatchLoop drains the queue, respecting the concurrency cap and per-archive
// de-dup so two runs never touch the same archive concurrently.
func (m *Manager) dispatchLoop(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case <-ctx.Done():
			return
		case q := <-m.queue:
			m.mu.Lock()
			if m.inflight[q.job.key()] {
				m.mu.Unlock()
				logger.DebugCF("cogmem", "consolidation job skipped; run already in flight", map[string]any{
					"id":      q.job.ID,
					"dir":     q.job.Dir,
					"trigger": q.trigger,
				})
				continue // already running; the in-flight run drains More itself
			}
			m.inflight[q.job.key()] = true
			m.mu.Unlock()

			// Acquire a pool slot; release inflight if we're shutting down.
			select {
			case m.sem <- struct{}{}:
			case <-m.stop:
				m.clearInflight(q.job.key())
				return
			case <-ctx.Done():
				m.clearInflight(q.job.key())
				return
			}

			m.wg.Add(1)
			go func(q queued) {
				defer m.wg.Done()
				defer func() { <-m.sem }()
				defer m.clearInflight(q.job.key())
				m.runFn(ctx, q.job, q.trigger)
			}(q)
		}
	}
}

func (m *Manager) clearInflight(key string) {
	m.mu.Lock()
	delete(m.inflight, key)
	m.mu.Unlock()
}

// runJob builds a Worker for the job and runs RunOnce in a loop while there is
// more to consolidate. Errors are logged and end the loop for this job.
func (m *Manager) runJob(ctx context.Context, j Job, trigger string) {
	w, err := m.factory(j)
	if err != nil {
		logger.WarnCF("cogmem", "consolidation worker factory failed", map[string]any{
			"id":    j.ID,
			"dir":   j.Dir,
			"error": err.Error(),
		})
		return
	}
	logger.DebugCF("cogmem", "consolidation run starting", map[string]any{
		"id":      j.ID,
		"dir":     j.Dir,
		"trigger": trigger,
	})
	for {
		select {
		case <-m.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		res, err := w.RunOnce(ctx, RunParams{
			ID:        j.ID,
			Dir:       j.Dir,
			Workspace: j.Workspace,
			Trigger:   trigger,
		})
		if err != nil {
			logger.WarnCF("cogmem", "consolidation run failed", map[string]any{
				"id":      j.ID,
				"dir":     j.Dir,
				"trigger": trigger,
				"status":  res.Status,
				"error":   err.Error(),
			})
			return
		}
		// Log every outcome — including idle/busy, which previously left no trace
		// in the log or the run table, making no-op runs impossible to diagnose.
		logger.InfoCF("cogmem", "consolidation run finished", map[string]any{
			"id":        j.ID,
			"dir":       j.Dir,
			"trigger":   trigger,
			"status":    res.Status,
			"applied":   res.Applied,
			"seq_start": res.SeqStart,
			"seq_end":   res.SeqEnd,
			"more":      res.More,
		})
		if res.Status == "busy" {
			return // another owner holds the lease; let it drain.
		}
		if !res.More {
			return
		}
	}
}

// idleLoop scans sessions on a ticker and enqueues those idle beyond the
// configured threshold. Each session fires at most once per idle period (until
// the next OnMessage resets idleEnqueued).
func (m *Manager) idleLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.opt.idlePoll)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			now := m.now()
			var due []Job
			m.mu.Lock()
			for _, st := range m.sessions {
				if st.idleEnqueued || st.count == 0 {
					continue
				}
				if now.Sub(st.lastActivity) >= m.opt.idle {
					st.idleEnqueued = true
					st.count = 0
					due = append(due, st.job)
				}
			}
			m.mu.Unlock()
			for _, j := range due {
				m.Enqueue(j, "idle")
			}
		}
	}
}

// nightlyLoop fires once per day at the configured local time (plus jitter),
// enqueuing every known session.
func (m *Manager) nightlyLoop(ctx context.Context) {
	defer m.wg.Done()
	for {
		next := m.nextNightly(m.now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-m.stop:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.mu.Lock()
			jobs := make([]Job, 0, len(m.sessions))
			for _, st := range m.sessions {
				jobs = append(jobs, st.job)
			}
			m.mu.Unlock()
			for _, j := range jobs {
				m.Enqueue(j, "nightly")
			}
		}
	}
}

// nextNightly returns the next time the nightly trigger should fire after from,
// honoring nightlyAt ("HH:MM" local) and adding random jitter within
// nightlyJitter.
func (m *Manager) nextNightly(from time.Time) time.Time {
	hh, mm := parseHHMM(m.opt.nightlyAt)
	loc := from.Location()
	candidate := time.Date(from.Year(), from.Month(), from.Day(), hh, mm, 0, 0, loc)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	if m.opt.nightlyJitter > 0 {
		candidate = candidate.Add(time.Duration(rand.Int63n(int64(m.opt.nightlyJitter))))
	}
	return candidate
}

// parseHHMM parses "HH:MM" into hour/minute, defaulting to 03:00 on malformed
// input.
func parseHHMM(s string) (hh, mm int) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 3, 0
	}
	return t.Hour(), t.Minute()
}
