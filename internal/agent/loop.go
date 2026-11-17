// Package agent is the autonomous loop: ask the model for the next step, run
// the tools it chose inside the sandbox, show it what happened, repeat until
// it submits or a budget runs out.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// Task is what the agent is asked to do.
type Task struct {
	Title string
	Issue string
	// RepoMap is a listing of the repository, gathered by the harness before the
	// first model call so the agent starts oriented instead of spending steps on it.
	RepoMap     string
	TestCommand string
}

// Budget bounds a run. The model decides what to do; it does not decide how long.
type Budget struct {
	MaxSteps       int
	CommandTimeout time.Duration
	// MaxToolOutput caps the characters of one tool result shown to the model.
	MaxToolOutput int
}

// DefaultBudget is generous for the small repositories this project targets.
var DefaultBudget = Budget{MaxSteps: 30, CommandTimeout: 120 * time.Second, MaxToolOutput: 8000}

// Stop reasons.
const (
	StopSubmitted = "submitted"
	StopBudget    = "step_budget_exhausted"
	StopStalled   = "model_stopped_calling_tools"
)

// Outcome says how the loop ended.
type Outcome struct {
	StopReason string
	Summary    string
	Steps      int
	ToolCalls  int
	Usage      model.Usage
}

// Emit records one event of the run. The loop reports everything it does
// through this; it holds no other output channel.
type Emit func(eventType string, data any)

// Loop runs one agent against one sandbox.
type Loop struct {
	Model   model.Backend
	Sandbox sandbox.Sandbox
	Emit    Emit
	Budget  Budget
}

const systemPrompt = `You are a software engineer fixing an issue in a repository. You work only through the tools provided.

How to work:
1. Understand: read the issue, then list and read the relevant files. Use search to find where things are defined and used.
2. Reproduce: run the tests to see the failure before changing anything.
3. Fix: make the smallest change that fixes the cause. Do not rewrite code that is not part of the problem, and do not edit tests to make them pass.
4. Verify: run the tests again. If they fail, read the output and fix the cause.
5. Finish: call submit with a short summary of what was wrong and what you changed.

Rules:
- Every step must be a tool call. Do not answer in prose alone.
- Paths are relative to the repository root.
- There is no network access. Do not try to install packages.
- If a tool returns an error, read it and correct the call; do not repeat the same call unchanged.`

// Event payloads. These are the shapes the control plane renders.
type (
	modelRequestEvent struct {
		Step     int `json:"step"`
		Messages int `json:"messages"`
	}
	modelResponseEvent struct {
		Step       int              `json:"step"`
		Reasoning  string           `json:"reasoning"`
		ToolCalls  []model.ToolCall `json:"toolCalls"`
		Usage      model.Usage      `json:"usage"`
		DurationMs int64            `json:"durationMs"`
	}
	toolCallEvent struct {
		ID   string          `json:"id"`
		Step int             `json:"step"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	toolOutputEvent struct {
		ID     string `json:"id"`
		Stream string `json:"stream"`
		Text   string `json:"text"`
	}
	toolResultEvent struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Output     string `json:"output"`
		IsError    bool   `json:"isError"`
		ExitCode   *int   `json:"exitCode,omitempty"`
		DurationMs int64  `json:"durationMs"`
	}
)

// Run drives the loop to completion. An error means the harness failed (the
// model was unreachable, the context was cancelled); an agent that simply did
// not solve the task returns an Outcome, not an error.
func (l *Loop) Run(ctx context.Context, task Task) (Outcome, error) {
	budget := l.Budget
	if budget.MaxSteps <= 0 {
		budget = DefaultBudget
	}
	tools := NewToolbox(l.Sandbox, task.TestCommand, budget.CommandTimeout, budget.MaxToolOutput)

	messages := []model.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: taskPrompt(task)},
	}
	outcome := Outcome{}
	nudges := 0

	for step := 1; step <= budget.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		outcome.Steps = step
		l.Emit(events.ModelRequest, modelRequestEvent{Step: step, Messages: len(messages)})

		started := time.Now()
		response, err := l.Model.Complete(ctx, model.Request{Messages: messages, Tools: tools.Specs()})
		if err != nil {
			return outcome, fmt.Errorf("agent: step %d: %w", step, err)
		}
		outcome.Usage.PromptTokens += response.Usage.PromptTokens
		outcome.Usage.CompletionTokens += response.Usage.CompletionTokens
		l.Emit(events.ModelResponse, modelResponseEvent{
			Step:       step,
			Reasoning:  response.Content,
			ToolCalls:  response.ToolCalls,
			Usage:      response.Usage,
			DurationMs: time.Since(started).Milliseconds(),
		})
		messages = append(messages, model.Message{Role: "assistant", Content: response.Content, ToolCalls: response.ToolCalls})

		if len(response.ToolCalls) == 0 {
			// Small models sometimes answer in prose. Say so twice, then give up
			// rather than spend the whole budget on a conversation.
			nudges++
			if nudges > 2 {
				outcome.StopReason = StopStalled
				return outcome, nil
			}
			messages = append(messages, model.Message{
				Role:    "user",
				Content: "You did not call a tool. Continue working with the tools, or call submit if the task is done.",
			})
			continue
		}

		for _, call := range response.ToolCalls {
			outcome.ToolCalls++
			l.Emit(events.ToolCall, toolCallEvent{ID: call.ID, Step: step, Name: call.Name, Args: call.Arguments})

			callStarted := time.Now()
			result := tools.Call(ctx, call, func(stream string, chunk []byte) {
				l.Emit(events.ToolOutput, toolOutputEvent{ID: call.ID, Stream: stream, Text: string(chunk)})
			})
			if err := ctx.Err(); err != nil {
				return outcome, err
			}
			l.Emit(events.ToolResult, toolResultEvent{
				ID:         call.ID,
				Name:       call.Name,
				Output:     result.Output,
				IsError:    result.IsError,
				ExitCode:   result.ExitCode,
				DurationMs: time.Since(callStarted).Milliseconds(),
			})
			if result.Submitted {
				outcome.StopReason = StopSubmitted
				outcome.Summary = result.Output
				return outcome, nil
			}
			content := result.Output
			if content == "" {
				content = "(no output)"
			}
			messages = append(messages, model.Message{Role: "tool", ToolCallID: call.ID, Content: content})
		}
	}
	outcome.StopReason = StopBudget
	return outcome, nil
}

func taskPrompt(task Task) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(task.Title) + "\n\n")
	b.WriteString(strings.TrimSpace(task.Issue) + "\n")
	if task.TestCommand != "" {
		b.WriteString("\nThe tests are run with: `" + task.TestCommand + "` (the run_tests tool does this).\n")
	}
	if task.RepoMap != "" {
		b.WriteString("\nFiles in the repository:\n```\n" + strings.TrimSpace(task.RepoMap) + "\n```\n")
	}
	return b.String()
}

// RepoMap lists the repository's files for the first prompt. It is the
// analysis the harness does on the agent's behalf: cheap, deterministic, and
// the same for every model.
func RepoMap(ctx context.Context, sb sandbox.Sandbox) (string, error) {
	result, err := sb.Exec(ctx, sandbox.Command{
		Argv: sandbox.Shell(`git ls-files | head -n 200; total=$(git ls-files | wc -l); if [ "$total" -gt 200 ]; then echo "... $((total-200)) more files"; fi`),
		Dir:  sandbox.WorkspaceDir,
	})
	if err != nil {
		return "", err
	}
	if !result.OK() {
		return "", fmt.Errorf("agent: listing repository: %s", strings.TrimSpace(result.Combined()))
	}
	return strings.TrimSpace(result.Stdout), nil
}
