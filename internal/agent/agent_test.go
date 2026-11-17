package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
	"github.com/sushantlokhande14/drydock/internal/sandbox/sandboxtest"
)

const source = "def total(items):\n    return len(items) // 10\n\n\ndef other():\n    return 1\n"

func repo() *sandboxtest.Fake {
	fake := sandboxtest.NewFake("s1", map[string]string{sandbox.WorkspaceDir + "/calc.py": source})
	fake.Handler = func(_ *sandboxtest.Fake, cmd sandbox.Command, _ string) (sandbox.Result, bool) {
		joined := strings.Join(cmd.Argv, " ")
		switch {
		case strings.Contains(joined, "pytest"):
			return sandbox.Result{Stdout: "3 passed in 0.01s"}, true
		case strings.Contains(joined, "exit 3"):
			return sandbox.Result{ExitCode: 3, Stderr: "boom"}, true
		}
		return sandbox.Result{}, false
	}
	return fake
}

type recorded struct {
	Type string
	Data map[string]any
}

func recorder() (Emit, *[]recorded) {
	var log []recorded
	return func(eventType string, data any) {
		raw, _ := json.Marshal(data)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		log = append(log, recorded{eventType, decoded})
	}, &log
}

func call(tool string, args map[string]any) model.ScriptCall {
	return model.ScriptCall{Tool: tool, Args: args}
}

func script(steps ...model.ScriptStep) *model.Scripted {
	return model.NewScripted(model.Script{Steps: steps})
}

func step(say string, calls ...model.ScriptCall) model.ScriptStep {
	return model.ScriptStep{Say: say, Calls: calls}
}

func TestLoopEditsTestsAndSubmits(t *testing.T) {
	fake := repo()
	emit, log := recorder()
	loop := Loop{
		Model: script(
			step("reading", call("read_file", map[string]any{"path": "calc.py"})),
			step("fixing", call("replace_in_file", map[string]any{"path": "calc.py", "old_text": "len(items) // 10", "new_text": "(len(items) + 9) // 10"})),
			step("", call("run_tests", nil)),
			step("", call("submit", map[string]any{"summary": "rounded up"})),
		),
		Sandbox: fake,
		Emit:    emit,
	}

	outcome, err := loop.Run(context.Background(), Task{Title: "t", Issue: "i", TestCommand: "python -m pytest -q"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != StopSubmitted || outcome.Summary != "rounded up" || outcome.Steps != 4 || outcome.ToolCalls != 4 {
		t.Fatalf("outcome: %+v", outcome)
	}
	content, _ := fake.File(sandbox.WorkspaceDir + "/calc.py")
	if !strings.Contains(content, "(len(items) + 9) // 10") || !strings.Contains(content, "def other()") {
		t.Fatalf("file after the edit:\n%s", content)
	}

	// Every step is on the record: a request, a response, then a call and a result per tool.
	var types []string
	for _, entry := range *log {
		types = append(types, entry.Type)
	}
	want := []string{
		events.ModelRequest, events.ModelResponse, events.ToolCall, events.ToolResult,
		events.ModelRequest, events.ModelResponse, events.ToolCall, events.ToolResult,
		events.ModelRequest, events.ModelResponse, events.ToolCall, events.ToolOutput, events.ToolResult,
		events.ModelRequest, events.ModelResponse, events.ToolCall, events.ToolResult,
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", types, want)
	}
	if (*log)[1].Data["reasoning"] != "reading" {
		t.Fatalf("the model's reasoning was not recorded: %v", (*log)[1].Data)
	}
}

func TestLoopStopsWhenTheStepBudgetRunsOut(t *testing.T) {
	steps := make([]model.ScriptStep, 10)
	for i := range steps {
		steps[i] = step("", call("list_files", nil))
	}
	emit, _ := recorder()
	loop := Loop{Model: script(steps...), Sandbox: repo(), Emit: emit, Budget: Budget{MaxSteps: 3}}

	outcome, err := loop.Run(context.Background(), Task{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != StopBudget || outcome.Steps != 3 {
		t.Fatalf("outcome: %+v", outcome)
	}
}

func TestLoopNudgesAModelThatAnswersInProseThenGivesUp(t *testing.T) {
	emit, _ := recorder()
	loop := Loop{Model: script(step("I think the bug is in calc.py."), step("It should round up."), step("Done."), step("Really done.")), Sandbox: repo(), Emit: emit}

	outcome, err := loop.Run(context.Background(), Task{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != StopStalled || outcome.Steps != 3 || outcome.ToolCalls != 0 {
		t.Fatalf("outcome: %+v", outcome)
	}
}

type failingModel struct{}

func (failingModel) Name() string { return "down" }
func (failingModel) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, errors.New("connection refused")
}

func TestAModelFailureIsAHarnessErrorNotAnAgentOutcome(t *testing.T) {
	emit, _ := recorder()
	_, err := (&Loop{Model: failingModel{}, Sandbox: repo(), Emit: emit}).Run(context.Background(), Task{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected the model error, got %v", err)
	}
}

func TestACancelledRunStopsBeforeTheNextStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	emit, log := recorder()
	_, err := (&Loop{Model: script(step("", call("list_files", nil))), Sandbox: repo(), Emit: emit}).Run(ctx, Task{Title: "t"})
	if !errors.Is(err, context.Canceled) || len(*log) != 0 {
		t.Fatalf("err=%v events=%d", err, len(*log))
	}
}

// ---------------------------------------------------------------------- tools

func runTool(t *testing.T, fake *sandboxtest.Fake, name string, args map[string]any) toolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewToolbox(fake, "python -m pytest -q", 0, 0).Call(context.Background(), model.ToolCall{ID: "c", Name: name, Arguments: raw}, nil)
}

func TestReplaceInFileRequiresExactlyOneMatch(t *testing.T) {
	fake := repo()
	path := sandbox.WorkspaceDir + "/calc.py"

	missing := runTool(t, fake, "replace_in_file", map[string]any{"path": "calc.py", "old_text": "no such text", "new_text": "x"})
	if !missing.IsError || !strings.Contains(missing.Output, "was not found") {
		t.Fatalf("missing text: %+v", missing)
	}
	ambiguous := runTool(t, fake, "replace_in_file", map[string]any{"path": "calc.py", "old_text": "return", "new_text": "yield"})
	if !ambiguous.IsError || !strings.Contains(ambiguous.Output, "occurs 2 times") {
		t.Fatalf("ambiguous text: %+v", ambiguous)
	}
	if content, _ := fake.File(path); content != source {
		t.Fatal("a refused edit must not change the file")
	}

	done := runTool(t, fake, "replace_in_file", map[string]any{"path": "calc.py", "old_text": "    return 1", "new_text": "    return 2"})
	if done.IsError {
		t.Fatalf("unique text: %+v", done)
	}
	if content, _ := fake.File(path); !strings.HasSuffix(content, "def other():\n    return 2\n") {
		t.Fatalf("file after the edit:\n%s", content)
	}
}

func TestToolErrorsAreMessagesForTheModel(t *testing.T) {
	fake := repo()
	unknown := runTool(t, fake, "delete_everything", nil)
	if !unknown.IsError || !strings.Contains(unknown.Output, "Unknown tool") || !strings.Contains(unknown.Output, "read_file") {
		t.Fatalf("unknown tool: %+v", unknown)
	}
	malformed := NewToolbox(fake, "", 0, 0).Call(context.Background(), model.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path": 5`)}, nil)
	if !malformed.IsError || !strings.Contains(malformed.Output, "not valid JSON") {
		t.Fatalf("malformed arguments: %+v", malformed)
	}
	empty := runTool(t, fake, "replace_in_file", map[string]any{"path": "calc.py", "old_text": "", "new_text": "x"})
	if !empty.IsError || !strings.Contains(empty.Output, "write_file") {
		t.Fatalf("empty old_text: %+v", empty)
	}
}

func TestPathsCannotLeaveTheRepositoryOrTouchGit(t *testing.T) {
	fake := repo()
	for _, path := range []string{"/etc/passwd", ".git/config", "src/../.git/HEAD"} {
		result := runTool(t, fake, "write_file", map[string]any{"path": path, "content": "x"})
		if !result.IsError {
			t.Errorf("writing %q was allowed", path)
		}
	}
	escaped := runTool(t, fake, "write_file", map[string]any{"path": "../../outside.txt", "content": "x"})
	if escaped.IsError {
		t.Fatalf("a relative path is rooted in the repository, not rejected: %+v", escaped)
	}
	if _, ok := fake.File(sandbox.WorkspaceDir + "/outside.txt"); !ok {
		t.Fatal("../../outside.txt should have been written inside the repository")
	}
}

func TestCommandResultsSpellOutTheExitStatus(t *testing.T) {
	fake := repo()
	passed := runTool(t, fake, "run_tests", nil)
	if passed.IsError || !strings.Contains(passed.Output, "3 passed") || !strings.HasSuffix(passed.Output, "[exit code 0]") {
		t.Fatalf("passing tests: %+v", passed)
	}
	failed := runTool(t, fake, "run_command", map[string]any{"command": "exit 3"})
	if !failed.IsError || *failed.ExitCode != 3 || !strings.Contains(failed.Output, "boom") || !strings.HasSuffix(failed.Output, "[exit code 3]") {
		t.Fatalf("failing command: %+v", failed)
	}
	if blank := runTool(t, fake, "run_command", map[string]any{"command": "  "}); !blank.IsError {
		t.Fatal("an empty command should be refused")
	}
}

func TestLongOutputKeepsItsHeadAndTail(t *testing.T) {
	text := "FIRST FAILURE\n" + strings.Repeat("noise\n", 5000) + "SUMMARY: 1 failed"
	clipped := clip(text, 400)
	if len(clipped) > 500 || !strings.HasPrefix(clipped, "FIRST FAILURE") || !strings.HasSuffix(clipped, "SUMMARY: 1 failed") || !strings.Contains(clipped, "characters omitted") {
		t.Fatalf("clipped to %d characters: %q", len(clipped), clipped)
	}
	if clip("short", 400) != "short" {
		t.Fatal("short output must be left alone")
	}
}

func TestSubmitEndsTheRunAndToolSpecsAreValidSchemas(t *testing.T) {
	result := runTool(t, repo(), "submit", map[string]any{"summary": "done"})
	if !result.Submitted || result.Output != "done" {
		t.Fatalf("submit: %+v", result)
	}
	for _, spec := range NewToolbox(repo(), "", 0, 0).Specs() {
		var schema map[string]any
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("tool %s has an invalid schema: %v %s", spec.Name, err, spec.Parameters)
		}
		if spec.Description == "" {
			t.Errorf("tool %s has no description", spec.Name)
		}
	}
}
