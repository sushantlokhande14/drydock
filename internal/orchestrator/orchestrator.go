// Package orchestrator schedules runs and carries each one through its life:
// provision a sandbox, check the repository out, run the agent, collect the
// patch, and verify it in a second, clean sandbox.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sushantlokhande14/drydock/internal/agent"
	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// Config is the orchestrator's fixed configuration.
type Config struct {
	// MaxConcurrent is how many runs execute at once. Each holds one sandbox, and
	// briefly a second one during verification.
	MaxConcurrent int
	// QueueSize is how many runs may wait. Beyond it, Submit refuses instead of
	// accepting work it cannot start: the caller learns immediately and can back off.
	QueueSize int
	Image     string
	Limits    sandbox.Limits
	Budget    agent.Budget
	// NewModel builds the backend for one run.
	NewModel func(spec TaskSpec) (model.Backend, error)
}

// maxSandboxAttempts bounds how often a run replaces a sandbox that turned out to be gone.
const maxSandboxAttempts = 3

// ErrBusy is returned by Submit when the queue is full.
var ErrBusy = errors.New("orchestrator: queue is full")

// ErrUnknownRun is returned for an id that does not exist.
var ErrUnknownRun = errors.New("orchestrator: unknown run")

// Orchestrator owns the queue, the workers and every run's state.
type Orchestrator struct {
	cfg   Config
	store *events.Store
	pool  *sandbox.Pool
	cache *sandbox.RepoCache

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	queue chan *run
	// slots holds one token per queued run. A token is taken before a run is
	// announced, so a run that has been accepted can always be enqueued.
	slots chan struct{}

	mu   sync.RWMutex
	runs map[string]*run

	active   atomic.Int64
	peak     atomic.Int64
	startups latencies
}

type run struct {
	id       string
	spec     TaskSpec
	enqueued time.Time
	done     chan struct{}

	mu      sync.Mutex
	summary Summary
	patch   string
	cancel  context.CancelFunc
}

// New returns an orchestrator. Call Start to begin executing runs.
func New(cfg Config, store *events.Store, pool *sandbox.Pool, cache *sandbox.RepoCache) *Orchestrator {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4 * cfg.MaxConcurrent
	}
	if cfg.Budget.MaxSteps <= 0 {
		cfg.Budget = agent.DefaultBudget
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Orchestrator{
		cfg:    cfg,
		store:  store,
		pool:   pool,
		cache:  cache,
		ctx:    ctx,
		cancel: cancel,
		queue:  make(chan *run, cfg.QueueSize),
		slots:  make(chan struct{}, cfg.QueueSize),
		runs:   map[string]*run{},
	}
}

// Restore rebuilds the state of earlier runs from the event logs on disk.
func (o *Orchestrator) Restore() (int, error) {
	loaded, err := o.store.Load()
	if err != nil {
		return 0, err
	}
	for _, id := range o.store.Runs() {
		log, err := o.store.Events(id)
		if err != nil {
			continue
		}
		restored := &run{id: id, done: make(chan struct{}), summary: Fold(log)}
		close(restored.done)
		for _, event := range log {
			if event.Type == events.PatchCreated {
				var data patchCreatedEvent
				if json.Unmarshal(event.Data, &data) == nil {
					restored.patch = data.Diff
				}
			}
		}
		o.mu.Lock()
		o.runs[id] = restored
		o.mu.Unlock()
	}
	return loaded, nil
}

// Start launches the workers.
func (o *Orchestrator) Start() {
	for i := 0; i < o.cfg.MaxConcurrent; i++ {
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			for {
				select {
				case <-o.ctx.Done():
					return
				case next := <-o.queue:
					<-o.slots
					o.execute(next)
				}
			}
		}()
	}
}

// Shutdown cancels every run, waits for the workers to finish cleaning up,
// and releases the pool and the cache. Runs still queued are marked cancelled.
func (o *Orchestrator) Shutdown(ctx context.Context) {
	o.cancel()
	o.wg.Wait()
	for {
		select {
		case queued := <-o.queue:
			o.finish(queued, time.Now(), Timing{}, StateCancelled, ReasonCancelled, "", "", false)
		default:
			o.pool.Close(ctx)
			o.cache.Close(ctx)
			return
		}
	}
}

func newRunID() string {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	// Millisecond timestamp first, so ids sort by creation time.
	return fmt.Sprintf("%011x%s", time.Now().UnixMilli(), hex.EncodeToString(suffix))
}

// Submit accepts a run and queues it. It returns ErrBusy when the queue is full.
func (o *Orchestrator) Submit(spec TaskSpec) (Summary, error) {
	if strings.TrimSpace(spec.Title) == "" {
		return Summary{}, errors.New("title is required")
	}
	if (spec.RepoURL == "") == (spec.Mirror == "") {
		return Summary{}, errors.New("exactly one of repoUrl and mirror is required")
	}
	if spec.RepoURL != "" {
		if err := sandbox.ValidateURL(spec.RepoURL); err != nil {
			return Summary{}, err
		}
	}
	if spec.Mirror != "" && !strings.HasPrefix(spec.Mirror, "/cache/") {
		return Summary{}, errors.New("mirror must be a path under /cache")
	}
	if spec.Model == "" {
		return Summary{}, errors.New("model is required")
	}

	select {
	case o.slots <- struct{}{}:
	default:
		return Summary{}, ErrBusy
	}

	created := &run{id: newRunID(), spec: spec, enqueued: time.Now(), done: make(chan struct{})}
	o.mu.Lock()
	o.runs[created.id] = created
	o.mu.Unlock()

	repo := spec.RepoURL
	if repo == "" {
		repo = spec.Mirror
	}
	o.emit(created, events.RunCreated, runCreatedEvent{Title: spec.Title, Issue: spec.Issue, Repo: repo, Ref: spec.Ref, Model: spec.Model})
	o.queue <- created
	return created.snapshot(), nil
}

func (r *run) snapshot() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.summary
}

// emit appends an event and folds it into the run's summary, in that order:
// the summary never shows something the log does not contain.
func (o *Orchestrator) emit(r *run, eventType string, data any) {
	event, err := o.store.Append(r.id, eventType, data)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.summary.Apply(event)
	if eventType == events.PatchCreated {
		if patch, ok := data.(patchCreatedEvent); ok {
			r.patch = patch.Diff
		}
	}
	r.mu.Unlock()
}

func (o *Orchestrator) execute(r *run) {
	ctx, cancel := context.WithCancel(o.ctx)
	defer cancel()
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()

	current := o.active.Add(1)
	defer o.active.Add(-1)
	for {
		peak := o.peak.Load()
		if current <= peak || o.peak.CompareAndSwap(peak, current) {
			break
		}
	}

	started := time.Now()
	timing := Timing{Queue: started.Sub(r.enqueued).Milliseconds()}
	fail := func(reason string, err error) {
		state := StateFailed
		message := ""
		if err != nil {
			message = err.Error()
		}
		if ctx.Err() != nil {
			state, reason, message = StateCancelled, ReasonCancelled, ""
		}
		o.finish(r, started, timing, state, reason, message, "", false)
	}

	o.emit(r, events.RunState, runStateEvent{State: StateProvisioning, Phase: "provisioning"})

	// The mirror is fetched outside the startup measurement: it happens once per
	// repository, not once per run, and is reported on its own.
	mirror, fetched := r.spec.Mirror, false
	if r.spec.RepoURL != "" {
		var err error
		if mirror, fetched, err = o.cache.Ensure(ctx, r.spec.RepoURL); err != nil {
			fail(ReasonHarnessError, err)
			return
		}
	}

	// A sandbox that waited in the pool may have been removed from under it. That shows up on first
	// use, so getting the sandbox and checking the repository out are retried together, a few times.
	var (
		sb            sandbox.Sandbox
		warm          bool
		checkout      sandbox.Result
		checkoutStart time.Time
	)
	for attempt := 1; ; attempt++ {
		provisionStart := time.Now()
		var err error
		sb, warm, err = o.pool.Acquire(ctx)
		if err != nil {
			fail(ReasonHarnessError, err)
			return
		}
		timing.Sandbox = time.Since(provisionStart).Milliseconds()
		o.emit(r, events.SandboxReady, sandboxReadyEvent{Sandbox: sb.ID(), Warm: warm, Millis: timing.Sandbox, QueueMs: timing.Queue})

		checkoutStart = time.Now()
		checkout, err = sandbox.Checkout(ctx, sb, mirror, r.spec.Ref)
		if err == nil && !checkout.OK() {
			err = fmt.Errorf("checkout failed: %s", strings.TrimSpace(checkout.Combined()))
		}
		if err == nil {
			break
		}
		closeSandbox(sb)
		if errors.Is(err, sandbox.ErrGone) && attempt < maxSandboxAttempts {
			continue
		}
		fail(ReasonHarnessError, err)
		return
	}
	defer closeSandbox(sb)
	commit := strings.TrimSpace(checkout.Stdout)
	timing.Checkout = time.Since(checkoutStart).Milliseconds()
	timing.Startup = timing.Sandbox + timing.Checkout
	o.startups.record(timing.Startup, warm)

	repoMap, err := agent.RepoMap(ctx, sb)
	if err != nil {
		fail(ReasonHarnessError, err)
		return
	}
	o.emit(r, events.RepoReady, repoReadyEvent{
		Commit:        commit,
		CheckoutMs:    timing.Checkout,
		StartupMs:     timing.Startup,
		MirrorFetched: fetched,
		Files:         strings.Count(repoMap, "\n") + 1,
	})

	backend, err := o.cfg.NewModel(r.spec)
	if err != nil {
		fail(ReasonHarnessError, err)
		return
	}
	budget := o.cfg.Budget
	if r.spec.MaxSteps > 0 {
		budget.MaxSteps = r.spec.MaxSteps
	}
	o.emit(r, events.RunState, runStateEvent{State: StateRunning, Phase: "analysis"})
	agentStart := time.Now()
	loop := agent.Loop{
		Model:   backend,
		Sandbox: sb,
		Emit:    func(eventType string, data any) { o.emit(r, eventType, data) },
		Budget:  budget,
	}
	outcome, err := loop.Run(ctx, agent.Task{
		Title:       r.spec.Title,
		Issue:       r.spec.Issue,
		RepoMap:     repoMap,
		TestCommand: r.spec.TestCommand,
	})
	timing.Agent = time.Since(agentStart).Milliseconds()
	if err != nil {
		fail(ReasonHarnessError, err)
		return
	}

	diff, stats, err := collectPatch(ctx, sb)
	if err != nil {
		fail(ReasonHarnessError, err)
		return
	}
	if strings.TrimSpace(diff) == "" {
		reason := ReasonNoChange
		if outcome.StopReason != agent.StopSubmitted {
			reason = outcome.StopReason
		}
		o.finish(r, started, timing, StateFailed, reason, "", outcome.Summary, false)
		return
	}
	o.emit(r, events.PatchCreated, patchCreatedEvent{Diff: diff, Stats: stats})

	// A patch is judged by what it does on a clean checkout, not by what the agent
	// saw in a workspace it had been editing and running commands in.
	o.emit(r, events.RunState, runStateEvent{State: StateVerifying, Phase: "verifying"})
	verifyStart := time.Now()
	passed, output, verifier, err := o.verify(ctx, r.spec, mirror, commit, diff)
	timing.Verify = time.Since(verifyStart).Milliseconds()
	if err != nil {
		fail(ReasonHarnessError, err)
		return
	}
	o.emit(r, events.VerifyResult, verifyResultEvent{Passed: passed, Output: output, Millis: timing.Verify, Sandbox: verifier})

	if passed {
		o.finish(r, started, timing, StateSucceeded, ReasonVerified, "", outcome.Summary, true)
		return
	}
	o.finish(r, started, timing, StateFailed, ReasonVerifyFailed, "", outcome.Summary, false)
}

func closeSandbox(sb sandbox.Sandbox) {
	// The run's own context may already be cancelled; cleanup must happen anyway.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = sb.Close(ctx)
}

func (o *Orchestrator) finish(r *run, started time.Time, timing Timing, state State, reason, errText, summary string, verified bool) {
	timing.Total = time.Since(started).Milliseconds()
	o.emit(r, events.RunFinished, runFinishedEvent{
		State:    state,
		Reason:   reason,
		Error:    errText,
		Summary:  summary,
		Verified: verified,
		Timing:   timing,
	})
	o.store.Close(r.id)
	select {
	case <-r.done:
	default:
		close(r.done)
	}
}

// collectPatch stages everything the agent changed, including new files, and
// returns it as a unified diff against the commit the run started from.
func collectPatch(ctx context.Context, sb sandbox.Sandbox) (string, PatchStats, error) {
	diff, err := sb.Exec(ctx, sandbox.Command{
		Argv:      sandbox.Shell(`git add -A . && git diff --cached --no-color --no-ext-diff HEAD`),
		Dir:       sandbox.WorkspaceDir,
		MaxOutput: 1 << 20,
	})
	if err != nil {
		return "", PatchStats{}, err
	}
	if !diff.OK() {
		return "", PatchStats{}, fmt.Errorf("collecting patch: %s", strings.TrimSpace(diff.Combined()))
	}
	if diff.Truncated {
		return "", PatchStats{}, errors.New("collecting patch: the change is larger than 1 MiB")
	}
	numstat, err := sb.Exec(ctx, sandbox.Command{
		Argv: sandbox.Shell(`git diff --cached --numstat HEAD`),
		Dir:  sandbox.WorkspaceDir,
	})
	if err != nil {
		return "", PatchStats{}, err
	}
	var stats PatchStats
	for _, line := range strings.Split(strings.TrimSpace(numstat.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		stats.Files++
		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		stats.Additions += added
		stats.Deletions += deleted
	}
	return diff.Stdout, stats, nil
}

// verify applies the patch to a clean checkout in a new sandbox, adds the
// hidden files, and runs the verification command.
func (o *Orchestrator) verify(ctx context.Context, spec TaskSpec, mirror, commit, diff string) (passed bool, output, sandboxID string, err error) {
	sb, _, err := o.pool.Acquire(ctx)
	if err != nil {
		return false, "", "", err
	}
	defer closeSandbox(sb)
	sandboxID = sb.ID()

	checkout, err := sandbox.Checkout(ctx, sb, mirror, commit)
	if err != nil {
		return false, "", sandboxID, err
	}
	if !checkout.OK() {
		return false, "", sandboxID, fmt.Errorf("verification checkout failed: %s", strings.TrimSpace(checkout.Combined()))
	}

	applied, err := sb.Exec(ctx, sandbox.Command{
		Argv:  []string{"git", "apply", "--whitespace=nowarn", "-"},
		Dir:   sandbox.WorkspaceDir,
		Stdin: strings.NewReader(diff),
	})
	if err != nil {
		return false, "", sandboxID, err
	}
	if !applied.OK() {
		return false, "The patch does not apply to a clean checkout:\n" + strings.TrimSpace(applied.Combined()), sandboxID, nil
	}

	paths := make([]string, 0, len(spec.VerifyFiles))
	for path := range spec.VerifyFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		target, err := sandbox.RepoPath(path)
		if err != nil {
			return false, "", sandboxID, fmt.Errorf("verify file: %w", err)
		}
		if err := sandbox.WriteFile(ctx, sb, target, []byte(spec.VerifyFiles[path])); err != nil {
			return false, "", sandboxID, err
		}
	}

	command := spec.VerifyCommand
	if command == "" {
		command = spec.TestCommand
	}
	if command == "" {
		return false, "", sandboxID, errors.New("no verification command: set verifyCommand or testCommand")
	}
	result, err := sb.Exec(ctx, sandbox.Command{
		Argv:    sandbox.Shell(command),
		Dir:     sandbox.WorkspaceDir,
		Timeout: o.cfg.Budget.CommandTimeout,
	})
	if err != nil {
		return false, "", sandboxID, err
	}
	return result.OK(), strings.TrimSpace(result.Combined()), sandboxID, nil
}

// Cancel stops a run. Cancelling a finished run does nothing.
func (o *Orchestrator) Cancel(id string) error {
	o.mu.RLock()
	target, ok := o.runs[id]
	o.mu.RUnlock()
	if !ok {
		return ErrUnknownRun
	}
	target.mu.Lock()
	cancel := target.cancel
	target.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Get returns a run's summary and its patch, if it produced one.
func (o *Orchestrator) Get(id string) (Summary, string, error) {
	o.mu.RLock()
	target, ok := o.runs[id]
	o.mu.RUnlock()
	if !ok {
		return Summary{}, "", ErrUnknownRun
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.summary, target.patch, nil
}

// Wait blocks until the run has finished or ctx ends.
func (o *Orchestrator) Wait(ctx context.Context, id string) (Summary, error) {
	o.mu.RLock()
	target, ok := o.runs[id]
	o.mu.RUnlock()
	if !ok {
		return Summary{}, ErrUnknownRun
	}
	select {
	case <-target.done:
		return target.snapshot(), nil
	case <-ctx.Done():
		return target.snapshot(), ctx.Err()
	}
}

// List returns every run, newest first.
func (o *Orchestrator) List() []Summary {
	o.mu.RLock()
	out := make([]Summary, 0, len(o.runs))
	for _, r := range o.runs {
		out = append(out, r.snapshot())
	}
	o.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Stats is a snapshot of the scheduler for the metrics endpoint and the benchmarks.
type Stats struct {
	Active        int64             `json:"active"`
	PeakActive    int64             `json:"peakActive"`
	Queued        int               `json:"queued"`
	MaxConcurrent int               `json:"maxConcurrent"`
	QueueSize     int               `json:"queueSize"`
	Total         int               `json:"total"`
	ByState       map[State]int     `json:"byState"`
	Pool          sandbox.PoolStats `json:"pool"`
	Startup       LatencyReport     `json:"startup"`
}

// Stats returns a snapshot of the scheduler.
func (o *Orchestrator) Stats() Stats {
	stats := Stats{
		Active:        o.active.Load(),
		PeakActive:    o.peak.Load(),
		Queued:        len(o.queue),
		MaxConcurrent: o.cfg.MaxConcurrent,
		QueueSize:     o.cfg.QueueSize,
		ByState:       map[State]int{},
		Pool:          o.pool.Stats(),
		Startup:       o.startups.report(),
	}
	o.mu.RLock()
	for _, r := range o.runs {
		stats.Total++
		stats.ByState[r.snapshot().State]++
	}
	o.mu.RUnlock()
	return stats
}

// Percentiles summarises a set of latencies in milliseconds.
type Percentiles struct {
	Count int     `json:"count"`
	P50   int64   `json:"p50"`
	P95   int64   `json:"p95"`
	Max   int64   `json:"max"`
	Mean  float64 `json:"mean"`
}

// LatencyReport splits startup latency by whether the sandbox came from the pool.
type LatencyReport struct {
	All  Percentiles `json:"all"`
	Warm Percentiles `json:"warm"`
	Cold Percentiles `json:"cold"`
}

type latencies struct {
	mu   sync.Mutex
	warm []int64
	cold []int64
}

func (l *latencies) record(millis int64, warm bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if warm {
		l.warm = append(l.warm, millis)
	} else {
		l.cold = append(l.cold, millis)
	}
}

func (l *latencies) report() LatencyReport {
	l.mu.Lock()
	warm := append([]int64(nil), l.warm...)
	cold := append([]int64(nil), l.cold...)
	l.mu.Unlock()
	return LatencyReport{
		All:  Summarise(append(append([]int64(nil), warm...), cold...)),
		Warm: Summarise(warm),
		Cold: Summarise(cold),
	}
}

// Summarise computes percentiles by the nearest-rank method.
func Summarise(samples []int64) Percentiles {
	if len(samples) == 0 {
		return Percentiles{}
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := func(p float64) int64 {
		index := int(float64(len(sorted))*p+0.999999) - 1
		return sorted[min(max(index, 0), len(sorted)-1)]
	}
	var sum int64
	for _, value := range sorted {
		sum += value
	}
	return Percentiles{
		Count: len(sorted),
		P50:   rank(0.50),
		P95:   rank(0.95),
		Max:   sorted[len(sorted)-1],
		Mean:  float64(sum) / float64(len(sorted)),
	}
}
