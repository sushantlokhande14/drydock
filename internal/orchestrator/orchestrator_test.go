package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
	"github.com/sushantlokhande14/drydock/internal/sandbox/sandboxtest"
)

const (
	calcPath = sandbox.WorkspaceDir + "/calc.py"
	original = "def pages(n):\n    return n // 10\n"
	fakeDiff = "diff --git a/calc.py b/calc.py\n--- a/calc.py\n+++ b/calc.py\n@@ -1,2 +1,2 @@\n def pages(n):\n-    return n // 10\n+    return (n + 9) // 10\n"
)

// harness is an orchestrator over fake sandboxes that behave like a small git checkout.
type harness struct {
	t      *testing.T
	orch   *Orchestrator
	driver *sandboxtest.Driver
	store  *events.Store

	mu            sync.Mutex
	verifyExit    int
	checkoutFails bool
	applied       []string
	gate          *gate
}

func (h *harness) handle(f *sandboxtest.Fake, cmd sandbox.Command, stdin string) (sandbox.Result, bool) {
	joined := strings.Join(cmd.Argv, " ")
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.Contains(joined, "git clone --quiet --shared"):
		if h.checkoutFails {
			return sandbox.Result{ExitCode: 128, Stderr: "fatal: not a git repository"}, true
		}
		return sandbox.Result{Stdout: "abc123def4567890\n"}, true
	case joined == "git status --porcelain":
		if content, _ := f.File(calcPath); content == original {
			return sandbox.Result{}, true
		}
		return sandbox.Result{Stdout: " M calc.py\n"}, true
	case strings.Contains(joined, "git ls-files | head"):
		return sandbox.Result{Stdout: "calc.py\ntests/test_calc.py\n"}, true
	case strings.Contains(joined, "git diff --cached --no-color"):
		if content, _ := f.File(calcPath); content == original {
			return sandbox.Result{}, true
		}
		return sandbox.Result{Stdout: fakeDiff}, true
	case strings.Contains(joined, "--numstat"):
		return sandbox.Result{Stdout: "1\t1\tcalc.py\n"}, true
	case len(cmd.Argv) >= 2 && cmd.Argv[0] == "git" && cmd.Argv[1] == "apply":
		h.applied = append(h.applied, stdin)
		return sandbox.Result{}, true
	case strings.Contains(joined, "verify-tests"):
		return sandbox.Result{ExitCode: h.verifyExit, Stdout: "verify output"}, true
	case strings.Contains(joined, "agent-tests"):
		return sandbox.Result{Stdout: "3 passed"}, true
	}
	return sandbox.Result{}, false
}

// gate is a model that waits until it is released, to hold runs in flight.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 64), release: make(chan struct{})} }

func (g *gate) Name() string { return "gate" }

func (g *gate) Complete(ctx context.Context, _ model.Request) (model.Response, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
	args, _ := json.Marshal(map[string]string{"summary": "released"})
	return model.Response{ToolCalls: []model.ToolCall{{ID: "g", Name: "submit", Arguments: args}}}, nil
}

func newHarness(t *testing.T, cfg Config, dir string) *harness {
	t.Helper()
	h := &harness{t: t}
	h.driver = &sandboxtest.Driver{New: func(index int, _ sandbox.Spec) *sandboxtest.Fake {
		fake := sandboxtest.NewFake(fmt.Sprintf("sb-%d", index), map[string]string{calcPath: original})
		fake.Handler = h.handle
		return fake
	}}
	store, err := events.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	cfg.NewModel = func(spec TaskSpec) (model.Backend, error) {
		switch spec.Model {
		case "scripted":
			return model.NewScripted(*spec.Script), nil
		case "gate":
			return h.gate, nil
		}
		return nil, fmt.Errorf("unknown model %q", spec.Model)
	}
	pool := sandbox.NewPool(h.driver, sandbox.Spec{Image: "img", Purpose: sandbox.PurposeAgent}, 0)
	cache := sandbox.NewRepoCache(h.driver, "img", "vol", time.Hour)
	h.orch = New(cfg, store, pool, cache)
	t.Cleanup(func() {
		h.orch.Shutdown(context.Background())
		store.CloseAll()
	})
	return h
}

func fixScript() *model.Script {
	return &model.Script{Steps: []model.ScriptStep{
		{Say: "reading", Calls: []model.ScriptCall{{Tool: "read_file", Args: map[string]any{"path": "calc.py"}}}},
		{Say: "fixing", Calls: []model.ScriptCall{{Tool: "replace_in_file", Args: map[string]any{"path": "calc.py", "old_text": "n // 10", "new_text": "(n + 9) // 10"}}}},
		{Calls: []model.ScriptCall{{Tool: "run_tests", Args: map[string]any{}}}},
		{Calls: []model.ScriptCall{{Tool: "submit", Args: map[string]any{"summary": "rounded up"}}}},
	}}
}

func spec(script *model.Script) TaskSpec {
	return TaskSpec{
		Title:         "Last page is missing",
		Issue:         "25 items give 2 pages.",
		Mirror:        "/cache/x.git",
		TestCommand:   "agent-tests",
		VerifyCommand: "verify-tests",
		VerifyFiles:   map[string]string{"hidden_tests/test_hidden.py": "assert pages(25) == 3\n"},
		Model:         "scripted",
		Script:        script,
	}
}

func (h *harness) run(s TaskSpec) Summary {
	h.t.Helper()
	submitted, err := h.orch.Submit(s)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary, err := h.orch.Wait(ctx, submitted.ID)
	if err != nil {
		h.t.Fatalf("run did not finish: %v (%+v)", err, summary)
	}
	return summary
}

func (h *harness) eventTypes(id string) []string {
	log, _ := h.store.Events(id)
	types := make([]string, len(log))
	for i, event := range log {
		types[i] = event.Type
	}
	return types
}

func TestARunThatFixesTheBugIsVerifiedInASecondSandbox(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 2}, "")
	h.orch.Start()

	summary := h.run(spec(fixScript()))

	if summary.State != StateSucceeded || !summary.Verified || summary.Reason != ReasonVerified {
		t.Fatalf("summary: %+v", summary)
	}
	if summary.Step != 4 || summary.ToolCalls != 4 || summary.Phase != "verifying" || summary.Patch == nil || *summary.Patch != (PatchStats{Files: 1, Additions: 1, Deletions: 1}) {
		t.Fatalf("summary: %+v patch=%+v", summary, summary.Patch)
	}
	if summary.FinishedAt == nil || summary.Timing.Total < summary.Timing.Agent {
		t.Fatalf("timing: %+v", summary.Timing)
	}

	sandboxes := h.driver.Created()
	if len(sandboxes) != 2 {
		t.Fatalf("used %d sandboxes, want one for the agent and one for verification", len(sandboxes))
	}
	agentBox, verifyBox := sandboxes[0], sandboxes[1]
	if !agentBox.Closed() || !verifyBox.Closed() {
		t.Fatal("both sandboxes must be removed when the run ends")
	}
	// The verifier is a clean checkout: it has the original file, receives only the patch, and
	// gets the hidden tests the agent never saw.
	if content, _ := verifyBox.File(calcPath); content != original {
		t.Fatal("the verification sandbox must not share the agent's working tree")
	}
	if len(h.applied) != 1 || h.applied[0] != fakeDiff {
		t.Fatalf("the verifier should have been given exactly the patch, got %q", h.applied)
	}
	if hidden, ok := verifyBox.File(sandbox.WorkspaceDir + "/hidden_tests/test_hidden.py"); !ok || !strings.Contains(hidden, "pages(25)") {
		t.Fatal("hidden tests were not added for verification")
	}
	if _, leaked := agentBox.File(sandbox.WorkspaceDir + "/hidden_tests/test_hidden.py"); leaked {
		t.Fatal("hidden tests must never reach the agent's sandbox")
	}

	types := h.eventTypes(summary.ID)
	if types[0] != events.RunCreated || types[len(types)-1] != events.RunFinished {
		t.Fatalf("log should start with run.created and end with run.finished: %v", types)
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{events.SandboxReady, events.RepoReady, events.ModelResponse, events.ToolCall, events.PatchCreated, events.VerifyResult} {
		if !strings.Contains(joined, want) {
			t.Errorf("log is missing %s: %v", want, types)
		}
	}
	if _, patch, _ := h.orch.Get(summary.ID); patch != fakeDiff {
		t.Fatalf("stored patch: %q", patch)
	}
}

func TestAPatchThatFailsTheHiddenTestsIsAFailedRun(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	h.verifyExit = 1
	h.orch.Start()

	summary := h.run(spec(fixScript()))

	if summary.State != StateFailed || summary.Verified || summary.Reason != ReasonVerifyFailed {
		t.Fatalf("summary: %+v", summary)
	}
	if summary.Patch == nil {
		t.Fatal("the rejected patch should still be on record")
	}
}

func TestSubmittingWithoutChangingAnythingFails(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	h.orch.Start()
	// The agent claims a fix three times without touching a file. The first two submissions are
	// turned away; the third stands and the run is judged on its empty diff.
	claim := model.ScriptStep{Say: "I have fixed it.", Calls: []model.ScriptCall{{Tool: "submit", Args: map[string]any{"summary": "looks fine"}}}}
	noop := &model.Script{Steps: []model.ScriptStep{claim, claim, claim}}

	summary := h.run(spec(noop))

	if summary.State != StateFailed || summary.Reason != ReasonNoChange {
		t.Fatalf("summary: %+v", summary)
	}
	if len(h.driver.Created()) != 1 {
		t.Fatal("there is nothing to verify, so no second sandbox should be started")
	}
}

func TestRunningOutOfStepsWithoutAChangeSaysSo(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	h.orch.Start()
	wander := &model.Script{}
	for i := 0; i < 10; i++ {
		wander.Steps = append(wander.Steps, model.ScriptStep{Calls: []model.ScriptCall{{Tool: "list_files", Args: map[string]any{}}}})
	}
	s := spec(wander)
	s.MaxSteps = 3

	summary := h.run(s)

	if summary.State != StateFailed || summary.Reason != ReasonBudgetNoSubmit || summary.Step != 3 {
		t.Fatalf("summary: %+v", summary)
	}
}

func TestAHarnessFailureIsReportedAndTheSandboxIsStillRemoved(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	h.checkoutFails = true
	h.orch.Start()

	summary := h.run(spec(fixScript()))

	if summary.State != StateFailed || summary.Reason != ReasonHarnessError || !strings.Contains(summary.Error, "not a git repository") {
		t.Fatalf("summary: %+v", summary)
	}
	if created := h.driver.Created(); len(created) != 1 || !created[0].Closed() {
		t.Fatal("the sandbox must be removed after a failed checkout")
	}
}

func TestSubmitValidatesTheRequest(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	valid := spec(fixScript())
	cases := map[string]func(*TaskSpec){
		"no title":             func(s *TaskSpec) { s.Title = " " },
		"no repository":        func(s *TaskSpec) { s.Mirror = "" },
		"two repositories":     func(s *TaskSpec) { s.RepoURL = "https://github.com/a/b" },
		"no model":             func(s *TaskSpec) { s.Model = "" },
		"mirror outside cache": func(s *TaskSpec) { s.Mirror = "/etc/passwd" },
		"bad url":              func(s *TaskSpec) { s.Mirror = ""; s.RepoURL = "ext::sh -c id" },
	}
	for name, mutate := range cases {
		bad := valid
		mutate(&bad)
		if _, err := h.orch.Submit(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(h.orch.List()) != 0 {
		t.Fatal("a rejected request must not create a run")
	}
}

func TestTheQueueRefusesWorkItCannotHold(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1, QueueSize: 2}, "")
	h.gate = newGate()
	h.orch.Start()
	gated := spec(nil)
	gated.Model = "gate"

	first, err := h.orch.Submit(gated)
	if err != nil {
		t.Fatal(err)
	}
	<-h.gate.entered // the first run holds the only worker
	for i := 0; i < 2; i++ {
		if _, err := h.orch.Submit(gated); err != nil {
			t.Fatalf("queued run %d was refused: %v", i, err)
		}
	}
	if _, err := h.orch.Submit(gated); !errors.Is(err, ErrBusy) {
		t.Fatalf("a run beyond the queue should be refused with ErrBusy, got %v", err)
	}
	if stats := h.orch.Stats(); stats.Active != 1 || stats.Queued != 2 || stats.Total != 3 {
		t.Fatalf("stats: %+v", stats)
	}

	close(h.gate.release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.orch.Wait(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyNeverExceedsTheLimit(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 3, QueueSize: 16}, "")
	h.gate = newGate()
	h.orch.Start()
	gated := spec(nil)
	gated.Model = "gate"

	var ids []string
	for i := 0; i < 9; i++ {
		summary, err := h.orch.Submit(gated)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, summary.ID)
	}
	for i := 0; i < 3; i++ {
		<-h.gate.entered
	}
	time.Sleep(50 * time.Millisecond)
	if stats := h.orch.Stats(); stats.Active != 3 || stats.Queued != 6 {
		t.Fatalf("with every worker busy: %+v", stats)
	}
	close(h.gate.release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := h.orch.Wait(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if stats := h.orch.Stats(); stats.PeakActive != 3 || stats.Active != 0 || stats.Total != 9 {
		t.Fatalf("after all runs: %+v", stats)
	}
}

func TestCancellingARunStopsItAndRemovesItsSandbox(t *testing.T) {
	h := newHarness(t, Config{MaxConcurrent: 1}, "")
	h.gate = newGate()
	h.orch.Start()
	gated := spec(nil)
	gated.Model = "gate"
	submitted, _ := h.orch.Submit(gated)
	<-h.gate.entered

	if err := h.orch.Cancel(submitted.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary, err := h.orch.Wait(ctx, submitted.ID)
	if err != nil {
		t.Fatal(err)
	}

	if summary.State != StateCancelled || summary.Reason != ReasonCancelled || summary.Error != "" {
		t.Fatalf("summary: %+v", summary)
	}
	if created := h.driver.Created(); len(created) != 1 || !created[0].Closed() {
		t.Fatal("a cancelled run must not leave its sandbox behind")
	}
	if err := h.orch.Cancel("nope"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("cancelling an unknown run: %v", err)
	}
}

func TestRunsAreRebuiltFromTheirLogsAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first := newHarness(t, Config{MaxConcurrent: 1}, dir)
	first.orch.Start()
	finished := first.run(spec(fixScript()))
	first.orch.Shutdown(context.Background())
	first.store.CloseAll()

	// A second run whose log stops mid-flight, as if the process had been killed.
	interrupted := filepath.Join(dir, "interrupted.jsonl")
	lines := `{"seq":1,"run":"interrupted","time":"2026-01-01T00:00:00Z","type":"run.created","data":{"title":"Half done","repo":"/cache/x.git","model":"scripted"}}
{"seq":2,"run":"interrupted","time":"2026-01-01T00:00:01Z","type":"run.state","data":{"state":"running","phase":"analysis"}}
{"seq":3,"run":"interrupted","time":"2026-01-01T00:00:02Z","type":"tool.call","data":{"id":"c","step":1,"name":"replace_in_file","args":{}}}
`
	if err := os.WriteFile(interrupted, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	second := newHarness(t, Config{MaxConcurrent: 1}, dir)
	restored, err := second.orch.Restore()
	if err != nil || restored != 2 {
		t.Fatalf("restored %d runs, err %v", restored, err)
	}

	again, patch, err := second.orch.Get(finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != StateSucceeded || !again.Verified || again.Title != finished.Title || again.ToolCalls != finished.ToolCalls || patch != fakeDiff {
		t.Fatalf("restored summary differs from the live one:\n live %+v\n disk %+v", finished, again)
	}

	half, _, err := second.orch.Get("interrupted")
	if err != nil {
		t.Fatal(err)
	}
	// It is not shown as still running: nothing is running it.
	if half.State != StateFailed || half.Reason != ReasonInterrupted || half.Phase != "editing" || half.FinishedAt == nil {
		t.Fatalf("interrupted run: %+v", half)
	}
}

func TestSummaryFollowsTheWorkThroughItsPhases(t *testing.T) {
	var summary Summary
	apply := func(eventType string, data any) {
		raw, _ := json.Marshal(data)
		summary.Apply(events.Event{Seq: summary.Events + 1, Run: "r", Time: time.Unix(int64(summary.Events), 0), Type: eventType, Data: raw})
	}
	phase := func(tool string) string {
		apply(events.ToolCall, map[string]string{"name": tool})
		return summary.Phase
	}

	apply(events.RunCreated, runCreatedEvent{Title: "t", Repo: "r", Model: "m"})
	if summary.State != StateQueued || summary.Title != "t" {
		t.Fatalf("after creation: %+v", summary)
	}
	apply(events.RunState, runStateEvent{State: StateRunning, Phase: "analysis"})
	if got := []string{phase("list_files"), phase("read_file"), phase("replace_in_file"), phase("read_file"), phase("run_tests"), phase("submit")}; strings.Join(got, ",") != "analysis,analysis,editing,editing,testing,delivering" {
		t.Fatalf("phases: %v", got)
	}
	// Reading after an edit is still editing: the phase does not move backwards.
	if summary.ToolCalls != 6 || summary.LastTool != "submit" {
		t.Fatalf("counts: %+v", summary)
	}
	apply("some.future.event", map[string]int{"x": 1})
	if summary.Events != 9 || summary.Phase != "delivering" {
		t.Fatalf("an unknown event type should be counted and otherwise ignored: %+v", summary)
	}
}

func TestSummarise(t *testing.T) {
	if got := Summarise(nil); got != (Percentiles{}) {
		t.Fatalf("empty input: %+v", got)
	}
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(100 - i) // 100..1, unsorted on purpose
	}
	got := Summarise(samples)
	if got.Count != 100 || got.P50 != 50 || got.P95 != 95 || got.Max != 100 || got.Mean != 50.5 {
		t.Fatalf("1..100: %+v", got)
	}
	if one := Summarise([]int64{7}); one.P50 != 7 || one.P95 != 7 || one.Max != 7 {
		t.Fatalf("single sample: %+v", one)
	}
	if samples[0] != 100 {
		t.Fatal("Summarise must not reorder its input")
	}
}
