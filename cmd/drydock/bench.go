package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sushantlokhande14/drydock/internal/engine"
	"github.com/sushantlokhande14/drydock/internal/orchestrator"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

func writeReport(path string, report any, markdown string) error {
	fmt.Print(markdown)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(path, ".json")+".md", []byte(markdown), 0o644)
}

// ------------------------------------------------------------------ startup

type startupArm struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	SamplesMs   []int64                  `json:"samplesMs"`
	Stats       orchestrator.Percentiles `json:"stats"`
	// Parts are the mean milliseconds spent getting a started container and getting the repository into it.
	SandboxMeanMs float64 `json:"sandboxMeanMs"`
	RepoMeanMs    float64 `json:"repoMeanMs"`
}

// benchStartup measures the time from "a run needs an environment" to "a
// command can run against the repository", four ways. Each way removes one
// cost, so the table shows what each technique is worth.
func benchStartup(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench startup", flag.ExitOnError)
	var opts options
	opts.register(flags)
	runs := flags.Int("runs", 20, "samples per arm")
	naiveRuns := flags.Int("naive-runs", 5, "samples for the two arms that use the network, which are slow")
	repo := flags.String("repo", "https://github.com/pallets/click", "repository to check out")
	baseImage := flags.String("base-image", "python:3.11-slim", "image with nothing preinstalled, for the naive arm")
	out := flags.String("out", "bench/startup.json", "where to write the report")
	_ = flags.Parse(args)
	opts.tasksDir = ""
	opts.pool = max(opts.pool, 4)

	sys, err := opts.boot(ctx)
	if err != nil {
		return err
	}
	defer sys.shutdown()

	fmt.Fprintf(os.Stderr, "mirroring %s ...\n", *repo)
	mirror, _, err := sys.cache.Ensure(ctx, *repo)
	if err != nil {
		return err
	}

	type sample struct{ total, sandbox, repo time.Duration }
	measure := func(name, description string, count int, one func() (sample, error)) (startupArm, error) {
		arm := startupArm{Name: name, Description: description}
		var sandboxSum, repoSum time.Duration
		for i := 0; i < count; i++ {
			if err := ctx.Err(); err != nil {
				return arm, err
			}
			result, err := one()
			if err != nil {
				return arm, fmt.Errorf("%s: %w", name, err)
			}
			arm.SamplesMs = append(arm.SamplesMs, result.total.Milliseconds())
			sandboxSum += result.sandbox
			repoSum += result.repo
			fmt.Fprintf(os.Stderr, "  %-22s %d/%d  %6d ms\n", name, i+1, count, result.total.Milliseconds())
		}
		arm.Stats = orchestrator.Summarise(arm.SamplesMs)
		arm.SandboxMeanMs = float64(sandboxSum.Milliseconds()) / float64(count)
		arm.RepoMeanMs = float64(repoSum.Milliseconds()) / float64(count)
		return arm, nil
	}

	// A container outside the hardened driver, for the arms that need the network and root.
	networked := func(image, user, script string) (sample, error) {
		started := time.Now()
		id, err := sys.engine.CreateContainer(ctx, "", engine.ContainerSpec{
			Image:      image,
			Cmd:        []string{"sleep", "infinity"},
			User:       user,
			Labels:     map[string]string{"drydock.managed": "true", "drydock.namespace": sys.driver.Namespace(), "drydock.instance": "bench"},
			HostConfig: engine.HostConfig{NetworkMode: "bridge", Init: true},
		})
		if err != nil {
			return sample{}, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = sys.engine.RemoveContainer(cleanup, id)
		}()
		if err := sys.engine.StartContainer(ctx, id); err != nil {
			return sample{}, err
		}
		ready := time.Now()
		var output strings.Builder
		code, err := sys.engine.Exec(ctx, id, engine.ExecOptions{Cmd: []string{"sh", "-c", script}, Stdout: &output, Stderr: &output})
		if err != nil {
			return sample{}, err
		}
		if code != 0 {
			return sample{}, fmt.Errorf("setup exited %d: %s", code, strings.TrimSpace(output.String()))
		}
		done := time.Now()
		return sample{total: done.Sub(started), sandbox: ready.Sub(started), repo: done.Sub(ready)}, nil
	}

	hardened := func(acquire func() (sandbox.Sandbox, error)) (sample, error) {
		started := time.Now()
		sb, err := acquire()
		if err != nil {
			return sample{}, err
		}
		defer closeLater(sb)
		ready := time.Now()
		result, err := sandbox.Checkout(ctx, sb, mirror, "")
		if err != nil {
			return sample{}, err
		}
		if !result.OK() {
			return sample{}, fmt.Errorf("checkout: %s", strings.TrimSpace(result.Combined()))
		}
		done := time.Now()
		return sample{total: done.Sub(started), sandbox: ready.Sub(started), repo: done.Sub(ready)}, nil
	}

	clone := fmt.Sprintf("git clone -q -- %q /tmp/repo && git -C /tmp/repo rev-parse HEAD >/dev/null", *repo)
	spec := sandbox.Spec{Image: opts.image, Purpose: sandbox.PurposeAgent, Limits: sandbox.DefaultLimits, CacheVolume: opts.cacheVolume}

	var arms []startupArm
	arm, err := measure("naive", "base image; install git and the test runner; clone over the network", *naiveRuns, func() (sample, error) {
		return networked(*baseImage, "0:0", "export DEBIAN_FRONTEND=noninteractive && apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends git >/dev/null 2>&1 && pip install -q --disable-pip-version-check --root-user-action=ignore pytest >/dev/null && "+clone)
	})
	if err != nil {
		return err
	}
	arms = append(arms, arm)

	arm, err = measure("prebuilt image", "tools baked into the image; clone over the network", *naiveRuns, func() (sample, error) {
		return networked(opts.image, "1000:1000", clone)
	})
	if err != nil {
		return err
	}
	arms = append(arms, arm)

	arm, err = measure("+ repository cache", "hardened sandbox; clone from the local mirror, sharing its objects", *runs, func() (sample, error) {
		return hardened(func() (sandbox.Sandbox, error) { return sys.driver.Create(ctx, spec) })
	})
	if err != nil {
		return err
	}
	arms = append(arms, arm)

	arm, err = measure("+ warm pool", "sandbox already started and waiting; clone from the local mirror", *runs, func() (sample, error) {
		// Measure a full pool, not one that is still refilling from the previous sample.
		wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := sys.pool.WaitReady(wait); err != nil {
			return sample{}, err
		}
		return hardened(func() (sandbox.Sandbox, error) {
			sb, warm, err := sys.pool.Acquire(ctx)
			if err == nil && !warm {
				closeLater(sb)
				return nil, fmt.Errorf("the pool was empty; this sample would not be a warm start")
			}
			return sb, err
		})
	})
	if err != nil {
		return err
	}
	arms = append(arms, arm)

	var b strings.Builder
	fmt.Fprintf(&b, "\nStartup: time until a command can run against a checkout of %s\n\n", *repo)
	b.WriteString("| Arm | What it does | Samples | p50 ms | p95 ms | Max ms | Container ms | Repository ms |\n|---|---|---:|---:|---:|---:|---:|---:|\n")
	for _, a := range arms {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %.0f | %.0f |\n",
			a.Name, a.Description, a.Stats.Count, a.Stats.P50, a.Stats.P95, a.Stats.Max, a.SandboxMeanMs, a.RepoMeanMs)
	}
	b.WriteString("\nContainer ms and Repository ms are means of the two parts of each sample.\n")
	return writeReport(*out, map[string]any{"repo": *repo, "image": opts.image, "arms": arms, "measuredAt": time.Now().UTC()}, b.String())
}

func closeLater(sb sandbox.Sandbox) {
	if sb == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Close(ctx)
	}()
}

// --------------------------------------------------------------------- load

type loadReport struct {
	Agents            int                        `json:"agents"`
	MaxConcurrent     int                        `json:"maxConcurrent"`
	Pool              int                        `json:"pool"`
	ThinkTimeMs       int64                      `json:"scriptedThinkTimeMs"`
	Succeeded         int                        `json:"succeeded"`
	Failed            int                        `json:"failed"`
	Reasons           map[string]int             `json:"reasons"`
	WallSeconds       float64                    `json:"wallSeconds"`
	RunsPerMinute     float64                    `json:"runsPerMinute"`
	PeakActiveRuns    int64                      `json:"peakActiveRuns"`
	PeakLiveSandboxes int64                      `json:"peakLiveSandboxes"`
	Startup           orchestrator.LatencyReport `json:"startupMs"`
	Agent             orchestrator.Percentiles   `json:"agentMs"`
	Verify            orchestrator.Percentiles   `json:"verifyMs"`
	Total             orchestrator.Percentiles   `json:"totalMs"`
	Events            int64                      `json:"eventsWritten"`
	MeasuredAt        time.Time                  `json:"measuredAt"`
}

// benchLoad submits many scripted runs at once and reports what the system
// sustained. The model is replayed, so this measures the harness (scheduling,
// sandboxes, event log, verification), not model quality or model latency.
func benchLoad(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench load", flag.ExitOnError)
	var opts options
	opts.register(flags)
	agents := flags.Int("agents", 128, "runs to submit")
	out := flags.String("out", "bench/load.json", "where to write the report")
	_ = flags.Parse(args)
	opts.queueSize = *agents

	sys, err := opts.boot(ctx)
	if err != nil {
		return err
	}
	defer sys.shutdown()
	if len(sys.tasks) == 0 {
		return fmt.Errorf("no tasks found in %s", opts.tasksDir)
	}
	wait, cancel := context.WithTimeout(ctx, 3*time.Minute)
	err = sys.pool.WaitReady(wait)
	cancel()
	if err != nil {
		return fmt.Errorf("waiting for the pool to fill: %w", err)
	}

	// Count sandboxes by asking Docker, not the orchestrator: the claim is about containers that exist.
	var peakSandboxes atomic.Int64
	sampling, stopSampling := context.WithCancel(ctx)
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampling.Done():
				return
			case <-ticker.C:
				containers, err := sys.engine.ListContainers(sampling, "drydock.purpose=agent")
				if err != nil {
					continue
				}
				running := int64(0)
				for _, container := range containers {
					if container.State == "running" {
						running++
					}
				}
				for {
					peak := peakSandboxes.Load()
					if running <= peak || peakSandboxes.CompareAndSwap(peak, running) {
						break
					}
				}
			}
		}
	}()

	fmt.Fprintf(os.Stderr, "submitting %d runs (%d at once, pool of %d) ...\n", *agents, opts.maxConcurrent, opts.pool)
	started := time.Now()
	ids := make([]string, 0, *agents)
	for i := 0; i < *agents; i++ {
		summary, err := submitWithRetry(ctx, sys.orch, sys.tasks[i%len(sys.tasks)].Spec("scripted"))
		if err != nil {
			stopSampling()
			return err
		}
		ids = append(ids, summary.ID)
	}
	summaries, err := waitAll(ctx, sys.orch, ids)
	wall := time.Since(started)
	stopSampling()
	sampler.Wait()
	if err != nil {
		return err
	}

	report := loadReport{
		Agents:            *agents,
		MaxConcurrent:     opts.maxConcurrent,
		Pool:              opts.pool,
		ThinkTimeMs:       opts.thinkTime.Milliseconds(),
		Reasons:           map[string]int{},
		WallSeconds:       wall.Seconds(),
		RunsPerMinute:     float64(*agents) / wall.Minutes(),
		PeakActiveRuns:    sys.orch.Stats().PeakActive,
		PeakLiveSandboxes: peakSandboxes.Load(),
		Startup:           sys.orch.Stats().Startup,
		MeasuredAt:        time.Now().UTC(),
	}
	var agentMs, verifyMs, totalMs []int64
	for _, summary := range summaries {
		if summary.State == orchestrator.StateSucceeded {
			report.Succeeded++
		} else {
			report.Failed++
		}
		report.Reasons[summary.Reason]++
		report.Events += summary.Events
		agentMs = append(agentMs, summary.Timing.Agent)
		verifyMs = append(verifyMs, summary.Timing.Verify)
		totalMs = append(totalMs, summary.Timing.Total)
	}
	report.Agent = orchestrator.Summarise(agentMs)
	report.Verify = orchestrator.Summarise(verifyMs)
	report.Total = orchestrator.Summarise(totalMs)

	reasons := make([]string, 0, len(report.Reasons))
	for reason, count := range report.Reasons {
		reasons = append(reasons, fmt.Sprintf("%s: %d", reason, count))
	}
	sort.Strings(reasons)

	var b strings.Builder
	fmt.Fprintf(&b, "\nLoad: %d scripted agents, up to %d at once, pool of %d, %d ms scripted think time per turn\n\n", report.Agents, report.MaxConcurrent, report.Pool, report.ThinkTimeMs)
	b.WriteString("| Measure | Value |\n|---|---:|\n")
	fmt.Fprintf(&b, "| Runs that produced a verified patch | %d of %d |\n", report.Succeeded, report.Agents)
	fmt.Fprintf(&b, "| Peak runs executing at once | %d |\n", report.PeakActiveRuns)
	fmt.Fprintf(&b, "| Peak sandbox containers running at once | %d |\n", report.PeakLiveSandboxes)
	fmt.Fprintf(&b, "| Wall time | %.1f s |\n", report.WallSeconds)
	fmt.Fprintf(&b, "| Throughput | %.0f runs/min |\n", report.RunsPerMinute)
	fmt.Fprintf(&b, "| Events written | %d |\n", report.Events)
	fmt.Fprintf(&b, "| Startup, all runs (p50 / p95 / max) | %d / %d / %d ms |\n", report.Startup.All.P50, report.Startup.All.P95, report.Startup.All.Max)
	fmt.Fprintf(&b, "| Startup, warm from the pool (%d runs) | %d / %d / %d ms |\n", report.Startup.Warm.Count, report.Startup.Warm.P50, report.Startup.Warm.P95, report.Startup.Warm.Max)
	fmt.Fprintf(&b, "| Startup, created on demand (%d runs) | %d / %d / %d ms |\n", report.Startup.Cold.Count, report.Startup.Cold.P50, report.Startup.Cold.P95, report.Startup.Cold.Max)
	fmt.Fprintf(&b, "| Agent loop (p50 / p95) | %d / %d ms |\n", report.Agent.P50, report.Agent.P95)
	fmt.Fprintf(&b, "| Verification in a second sandbox (p50 / p95) | %d / %d ms |\n", report.Verify.P50, report.Verify.P95)
	fmt.Fprintf(&b, "| Whole run (p50 / p95) | %d / %d ms |\n", report.Total.P50, report.Total.P95)
	fmt.Fprintf(&b, "\nOutcomes: %s\n", strings.Join(reasons, ", "))
	return writeReport(*out, report, b.String())
}

// --------------------------------------------------------------------- eval

type evalRow struct {
	Task      string  `json:"task"`
	State     string  `json:"state"`
	Reason    string  `json:"reason"`
	Steps     int     `json:"steps"`
	ToolCalls int     `json:"toolCalls"`
	Seconds   float64 `json:"seconds"`
	Error     string  `json:"error,omitempty"`
}

// evaluate runs every bundled task with a real model and reports how many
// ended with a patch that passes the hidden tests on a clean checkout.
func evaluate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("eval", flag.ExitOnError)
	var opts options
	opts.register(flags)
	modelName := flags.String("model", "ollama:qwen2.5:7b", `"ollama:<name>" or "openai:<name>"`)
	repeat := flags.Int("repeat", 1, "attempts per task")
	out := flags.String("out", "bench/eval.json", "where to write the report")
	_ = flags.Parse(args)

	sys, err := opts.boot(ctx)
	if err != nil {
		return err
	}
	defer sys.shutdown()

	var ids []string
	var names []string
	for attempt := 0; attempt < *repeat; attempt++ {
		for _, task := range sys.tasks {
			summary, err := submitWithRetry(ctx, sys.orch, task.Spec(*modelName))
			if err != nil {
				return err
			}
			ids = append(ids, summary.ID)
			names = append(names, task.Name)
		}
	}
	fmt.Fprintf(os.Stderr, "running %d attempts with %s ...\n", len(ids), *modelName)

	rows := make([]evalRow, 0, len(ids))
	resolved := 0
	for i, id := range ids {
		summary, err := sys.orch.Wait(ctx, id)
		if err != nil {
			return err
		}
		if summary.State == orchestrator.StateSucceeded {
			resolved++
		}
		rows = append(rows, evalRow{
			Task:      names[i],
			State:     string(summary.State),
			Reason:    summary.Reason,
			Steps:     summary.Step,
			ToolCalls: summary.ToolCalls,
			Seconds:   float64(summary.Timing.Total) / 1000,
			Error:     summary.Error,
		})
		fmt.Fprintf(os.Stderr, "  %-28s %-10s %s\n", names[i], summary.State, summary.Reason)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nEvaluation: %s resolved %d of %d attempts (%d tasks x %d)\n\n", *modelName, resolved, len(rows), len(sys.tasks), *repeat)
	b.WriteString("| Task | Result | Why | Model turns | Tool calls | Seconds |\n|---|---|---|---:|---:|---:|\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %.0f |\n", row.Task, row.State, row.Reason, row.Steps, row.ToolCalls, row.Seconds)
	}
	return writeReport(*out, map[string]any{
		"model": *modelName, "resolved": resolved, "attempts": len(rows), "rows": rows, "measuredAt": time.Now().UTC(),
	}, b.String())
}
