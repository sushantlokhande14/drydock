package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sushantlokhande14/drydock/internal/api"
	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/orchestrator"
	"github.com/sushantlokhande14/drydock/internal/tasks"
	"github.com/sushantlokhande14/drydock/web"
)

func serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	var opts options
	opts.register(flags)
	addr := flags.String("addr", ":8080", "address to listen on")
	defaultModel := flags.String("default-model", "scripted", "model a bundled task runs with when the request does not name one")
	_ = flags.Parse(args)

	sys, err := opts.boot(ctx)
	if err != nil {
		return err
	}
	defer sys.shutdown()

	server := &http.Server{
		Addr: *addr,
		Handler: (&api.Server{
			Orchestrator: sys.orch,
			Store:        sys.store,
			Tasks:        sys.tasks,
			UI:           web.Handler(),
			DefaultModel: *defaultModel,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "drydock listening on %s (%d tasks, %d concurrent, pool of %d)\n", *addr, len(sys.tasks), opts.maxConcurrent, opts.pool)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// Stop accepting requests first; streams end when their runs are cancelled by shutdown.
	closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(closing)
	return nil
}

func findTask(all []tasks.Loaded, name string) (tasks.Loaded, error) {
	names := make([]string, 0, len(all))
	for _, task := range all {
		if task.Name == name {
			return task, nil
		}
		names = append(names, task.Name)
	}
	return tasks.Loaded{}, fmt.Errorf("no task %q (have: %s)", name, strings.Join(names, ", "))
}

// runOne runs a single bundled task and prints the agent's work as it happens.
func runOne(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	var opts options
	opts.register(flags)
	name := flags.String("task", "", "name of a bundled task")
	modelName := flags.String("model", "scripted", `"scripted" or "openai:<name>"`)
	_ = flags.Parse(args)
	opts.maxConcurrent, opts.pool = 1, 1

	sys, err := opts.boot(ctx)
	if err != nil {
		return err
	}
	defer sys.shutdown()
	task, err := findTask(sys.tasks, *name)
	if err != nil {
		return err
	}
	summary, err := sys.orch.Submit(task.Spec(*modelName))
	if err != nil {
		return err
	}
	feed, cancel, err := sys.store.Subscribe(summary.ID, 0)
	if err != nil {
		return err
	}
	defer cancel()
	for event := range feed {
		printEvent(event)
	}
	final, _, err := sys.orch.Get(summary.ID)
	if err != nil {
		return err
	}
	if final.State != orchestrator.StateSucceeded {
		return fmt.Errorf("run ended %s (%s)", final.State, final.Reason)
	}
	return nil
}

func printEvent(event events.Event) {
	var data map[string]any
	_ = json.Unmarshal(event.Data, &data)
	text := func(key string) string { value, _ := data[key].(string); return value }
	switch event.Type {
	case events.RunState:
		fmt.Printf("== %s\n", text("state"))
	case events.SandboxReady:
		fmt.Printf("   sandbox %s ready in %v ms (warm: %v)\n", text("sandbox"), data["ms"], data["warm"])
	case events.RepoReady:
		fmt.Printf("   repository at %.12s, startup %v ms\n", text("commit"), data["startupMs"])
	case events.ModelResponse:
		if reasoning := strings.TrimSpace(text("reasoning")); reasoning != "" {
			fmt.Printf("\n   %s\n", strings.ReplaceAll(reasoning, "\n", "\n   "))
		}
	case events.ToolCall:
		args, _ := json.Marshal(data["args"])
		fmt.Printf(" > %s %s\n", text("name"), clipLine(string(args), 160))
	case events.ToolResult:
		marker := "ok"
		if failed, _ := data["isError"].(bool); failed {
			marker = "error"
		}
		fmt.Printf("   [%s] %s\n", marker, clipLine(text("output"), 400))
	case events.PatchCreated:
		fmt.Printf("\n%s\n", text("diff"))
	case events.VerifyResult:
		fmt.Printf("== verification passed: %v\n", data["passed"])
	case events.RunFinished:
		fmt.Printf("== finished: %s (%s)\n", text("state"), text("reason"))
		if message := text("error"); message != "" {
			fmt.Printf("   %s\n", message)
		}
	}
}

func clipLine(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) > limit {
		text = text[:limit] + " ..."
	}
	return strings.ReplaceAll(text, "\n", "\n       ")
}

// waitAll waits for every run to finish and returns their final summaries, in the order given.
func waitAll(ctx context.Context, orch *orchestrator.Orchestrator, ids []string) ([]orchestrator.Summary, error) {
	out := make([]orchestrator.Summary, 0, len(ids))
	for _, id := range ids {
		summary, err := orch.Wait(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// submitWithRetry submits a run, backing off while the queue is full.
func submitWithRetry(ctx context.Context, orch *orchestrator.Orchestrator, spec orchestrator.TaskSpec) (orchestrator.Summary, error) {
	for {
		summary, err := orch.Submit(spec)
		if !errors.Is(err, orchestrator.ErrBusy) {
			return summary, err
		}
		select {
		case <-ctx.Done():
			return orchestrator.Summary{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
