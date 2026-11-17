package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// toolResult is what goes back to the model after a tool runs.
type toolResult struct {
	Output   string
	IsError  bool
	ExitCode *int
	// Submitted is set by the submit tool, which ends the loop.
	Submitted bool
}

type tool struct {
	spec model.Tool
	run  func(ctx context.Context, t *Toolbox, raw json.RawMessage, onOutput func(stream string, chunk []byte)) toolResult
}

// Toolbox is the set of actions an agent can take. Every one of them runs
// inside the sandbox; nothing here touches the orchestrator's own machine.
type Toolbox struct {
	Sandbox        sandbox.Sandbox
	TestCommand    string
	CommandTimeout time.Duration
	MaxOutput      int
	tools          map[string]tool
	order          []string
	// submitRefusals counts how often submit was turned away for an unchanged repository.
	submitRefusals int
}

const maxSubmitRefusals = 2

// NewToolbox returns the standard tools bound to one sandbox.
func NewToolbox(sb sandbox.Sandbox, testCommand string, commandTimeout time.Duration, maxOutput int) *Toolbox {
	if commandTimeout <= 0 {
		commandTimeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 8000
	}
	t := &Toolbox{Sandbox: sb, TestCommand: testCommand, CommandTimeout: commandTimeout, MaxOutput: maxOutput, tools: map[string]tool{}}
	for _, definition := range standardTools() {
		t.tools[definition.spec.Name] = definition
		t.order = append(t.order, definition.spec.Name)
	}
	return t
}

// Specs lists the tools for the model, in a stable order.
func (t *Toolbox) Specs() []model.Tool {
	specs := make([]model.Tool, 0, len(t.order))
	for _, name := range t.order {
		specs = append(specs, t.tools[name].spec)
	}
	return specs
}

// Call runs one tool call. An unknown tool or malformed arguments come back
// as an error result for the model to read, not as a Go error: a model that
// makes a mistake should be told, and the run should continue.
func (t *Toolbox) Call(ctx context.Context, call model.ToolCall, onOutput func(stream string, chunk []byte)) toolResult {
	definition, ok := t.tools[call.Name]
	if !ok {
		return toolResult{IsError: true, Output: fmt.Sprintf("Unknown tool %q. Available tools: %s.", call.Name, strings.Join(t.order, ", "))}
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}
	return definition.run(ctx, t, call.Arguments, onOutput)
}

func schema(properties string, required ...string) json.RawMessage {
	quoted := make([]string, len(required))
	for i, name := range required {
		quoted[i] = strconv.Quote(name)
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s},"required":[%s],"additionalProperties":false}`, properties, strings.Join(quoted, ",")))
}

func decode[T any](raw json.RawMessage) (T, *toolResult) {
	var args T
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, &toolResult{IsError: true, Output: "The arguments are not valid JSON for this tool: " + err.Error()}
	}
	return args, nil
}

// clip keeps the head and tail of long output, which is where a test run
// puts its first failure and its summary.
func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	head := limit * 2 / 5
	tail := limit - head
	return text[:head] + fmt.Sprintf("\n... [%d characters omitted] ...\n", len(text)-limit) + text[len(text)-tail:]
}

func standardTools() []tool {
	return []tool{
		{
			spec: model.Tool{
				Name:        "list_files",
				Description: "List files in the repository, optionally under a directory. Use this first to find your way around.",
				Parameters:  schema(`"path":{"type":"string","description":"Directory relative to the repository root. Omit for the whole repository."}`),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Path string `json:"path"`
				}](raw)
				if bad != nil {
					return *bad
				}
				where, err := sandbox.RepoPath(args.Path)
				if err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				// A pathspec, not a cd: it works for a directory and for a single file alike.
				result, err := t.Sandbox.Exec(ctx, sandbox.Command{
					Argv: []string{"bash", "-c", `git ls-files --cached --others --exclude-standard -- "$1" | head -n 400`, "bash", where},
					Dir:  sandbox.WorkspaceDir,
				})
				if err == nil && result.OK() && strings.TrimSpace(result.Stdout) == "" {
					return toolResult{IsError: true, Output: "Nothing at " + strconv.Quote(args.Path) + ". Call list_files with no path to see the whole repository."}
				}
				return fromExec(result, err, t.MaxOutput, false)
			},
		},
		{
			spec: model.Tool{
				Name:        "read_file",
				Description: "Read a file with line numbers. Reads up to 250 lines per call; pass start_line to read further.",
				Parameters: schema(
					`"path":{"type":"string"},"start_line":{"type":"integer","description":"First line to read, starting at 1."},"end_line":{"type":"integer"}`,
					"path",
				),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Path  string `json:"path"`
					Start int    `json:"start_line"`
					End   int    `json:"end_line"`
				}](raw)
				if bad != nil {
					return *bad
				}
				file, err := sandbox.RepoPath(args.Path)
				if err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				start := max(args.Start, 1)
				end := args.End
				if end < start || end-start >= 250 {
					end = start + 249
				}
				script := `awk -v s="$2" -v e="$3" 'NR>=s && NR<=e {printf "%5d  %s\n", NR, $0} END {if (NR>e) printf "... %d more lines\n", NR-e}' "$1"`
				result, err := t.Sandbox.Exec(ctx, sandbox.Command{
					Argv: []string{"bash", "-c", script, "bash", file, strconv.Itoa(start), strconv.Itoa(end)},
				})
				return fromExec(result, err, t.MaxOutput*2, false)
			},
		},
		{
			spec: model.Tool{
				Name:        "search",
				Description: "Search file contents for a regular expression. Returns matching lines with file names and line numbers.",
				Parameters: schema(
					`"pattern":{"type":"string"},"path":{"type":"string","description":"Directory or file to search. Omit for the whole repository."}`,
					"pattern",
				),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}](raw)
				if bad != nil {
					return *bad
				}
				where, err := sandbox.RepoPath(args.Path)
				if err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				// The pattern travels as an argument, never through a shell.
				result, err := t.Sandbox.Exec(ctx, sandbox.Command{
					Argv: []string{"rg", "--line-number", "--no-heading", "--smart-case", "--max-count", "30", "--regexp", args.Pattern, "--", where},
					Dir:  sandbox.WorkspaceDir,
				})
				if err == nil && result.ExitCode == 1 && result.Stderr == "" {
					return toolResult{Output: "No matches."}
				}
				out := fromExec(result, err, t.MaxOutput, false)
				out.Output = strings.ReplaceAll(out.Output, sandbox.WorkspaceDir+"/", "")
				return out
			},
		},
		{
			spec: model.Tool{
				Name: "replace_in_file",
				Description: "Replace one exact occurrence of old_text with new_text in a file. old_text must match the file exactly, " +
					"including indentation, and must occur exactly once; include surrounding lines to make it unique.",
				Parameters: schema(`"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}`, "path", "old_text", "new_text"),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Path string `json:"path"`
					Old  string `json:"old_text"`
					New  string `json:"new_text"`
				}](raw)
				if bad != nil {
					return *bad
				}
				file, err := sandbox.RepoPath(args.Path)
				if err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				if args.Old == "" {
					return toolResult{IsError: true, Output: "old_text is empty. To create a file, use write_file."}
				}
				content, truncated, err := sandbox.ReadFile(ctx, t.Sandbox, file, 2<<20)
				if err != nil {
					return toolResult{IsError: true, Output: "Cannot read " + args.Path + ": " + lastLine(err.Error())}
				}
				if truncated {
					return toolResult{IsError: true, Output: args.Path + " is too large to edit with this tool."}
				}
				switch count := strings.Count(content, args.Old); count {
				case 0:
					return toolResult{IsError: true, Output: "old_text was not found in " + args.Path + ". Read the file again and copy the text exactly, including indentation."}
				case 1:
				default:
					return toolResult{IsError: true, Output: fmt.Sprintf("old_text occurs %d times in %s. Include more surrounding lines so it matches exactly one place.", count, args.Path)}
				}
				updated := strings.Replace(content, args.Old, args.New, 1)
				if err := sandbox.WriteFile(ctx, t.Sandbox, file, []byte(updated)); err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				return toolResult{Output: "Replaced 1 occurrence in " + args.Path + "."}
			},
		},
		{
			spec: model.Tool{
				Name:        "write_file",
				Description: "Create a file, or overwrite it completely, with the given content.",
				Parameters:  schema(`"path":{"type":"string"},"content":{"type":"string"}`, "path", "content"),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}](raw)
				if bad != nil {
					return *bad
				}
				file, err := sandbox.RepoPath(args.Path)
				if err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				if file == sandbox.WorkspaceDir {
					return toolResult{IsError: true, Output: "path is a directory."}
				}
				if err := sandbox.WriteFile(ctx, t.Sandbox, file, []byte(args.Content)); err != nil {
					return toolResult{IsError: true, Output: err.Error()}
				}
				return toolResult{Output: fmt.Sprintf("Wrote %d bytes to %s.", len(args.Content), args.Path)}
			},
		},
		{
			spec: model.Tool{
				Name:        "run_command",
				Description: "Run a shell command in the repository root. There is no network access. Output is truncated if long.",
				Parameters:  schema(`"command":{"type":"string"}`, "command"),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, onOutput func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Command string `json:"command"`
				}](raw)
				if bad != nil {
					return *bad
				}
				if strings.TrimSpace(args.Command) == "" {
					return toolResult{IsError: true, Output: "command is empty."}
				}
				result, err := t.Sandbox.Exec(ctx, sandbox.Command{
					Argv:     sandbox.Shell(args.Command),
					Dir:      sandbox.WorkspaceDir,
					Timeout:  t.CommandTimeout,
					OnOutput: onOutput,
				})
				return fromExec(result, err, t.MaxOutput, true)
			},
		},
		{
			spec: model.Tool{
				Name:        "run_tests",
				Description: "Run the project's test suite. Do this after every change, and before you submit.",
				Parameters:  schema(``),
			},
			run: func(ctx context.Context, t *Toolbox, _ json.RawMessage, onOutput func(string, []byte)) toolResult {
				if t.TestCommand == "" {
					return toolResult{IsError: true, Output: "This task has no test command. Use run_command."}
				}
				result, err := t.Sandbox.Exec(ctx, sandbox.Command{
					Argv:     sandbox.Shell(t.TestCommand),
					Dir:      sandbox.WorkspaceDir,
					Timeout:  t.CommandTimeout,
					OnOutput: onOutput,
				})
				return fromExec(result, err, t.MaxOutput, true)
			},
		},
		{
			spec: model.Tool{
				Name:        "submit",
				Description: "Finish the task. Call this once the tests pass, with a short summary of what you changed and why.",
				Parameters:  schema(`"summary":{"type":"string"}`, "summary"),
			},
			run: func(ctx context.Context, t *Toolbox, raw json.RawMessage, _ func(string, []byte)) toolResult {
				args, bad := decode[struct {
					Summary string `json:"summary"`
				}](raw)
				if bad != nil {
					return *bad
				}
				// A model can describe a fix, believe it has made it, and submit an untouched
				// repository. Say so, twice at most; after that the submission stands and the
				// run is judged on its empty diff like any other.
				if t.submitRefusals < maxSubmitRefusals {
					status, err := t.Sandbox.Exec(ctx, sandbox.Command{Argv: []string{"git", "status", "--porcelain"}, Dir: sandbox.WorkspaceDir})
					if err == nil && status.OK() && strings.TrimSpace(status.Stdout) == "" {
						t.submitRefusals++
						return toolResult{IsError: true, Output: "Not submitted: no file in the repository has changed. Describing a change does not make it. " +
							"Edit the code with replace_in_file or write_file, run the tests, then call submit."}
					}
				}
				return toolResult{Output: args.Summary, Submitted: true}
			},
		},
	}
}

// fromExec turns a command result into what the model reads. With showStatus
// the exit code is spelled out, because "it printed nothing" and "it failed"
// must not look alike.
func fromExec(result sandbox.Result, err error, limit int, showStatus bool) toolResult {
	if err != nil {
		return toolResult{IsError: true, Output: "The command could not be run: " + err.Error()}
	}
	output := clip(strings.TrimRight(result.Combined(), "\n"), limit)
	code := result.ExitCode
	switch {
	case result.TimedOut:
		output += fmt.Sprintf("\n[timed out after %s and was killed]", result.Duration.Round(time.Second))
	case showStatus:
		output += fmt.Sprintf("\n[exit code %d]", code)
	case code != 0 && output == "":
		output = fmt.Sprintf("[exit code %d]", code)
	}
	return toolResult{Output: strings.TrimLeft(output, "\n"), IsError: !result.OK(), ExitCode: &code}
}

func lastLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		return text[i+1:]
	}
	return text
}
