package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama talks to a local Ollama server through its native chat API.
//
// The native API is used instead of the OpenAI-compatible one for a single
// reason: it lets the request set the context window. Ollama's default is
// 4,096 tokens, which an agent conversation (instructions, tool schemas, file
// contents, test output) overflows within a few steps; the server then drops
// the oldest tokens without saying so, and the first thing lost is the
// instructions.
type Ollama struct {
	BaseURL string // e.g. http://localhost:11434
	Model   string
	// ContextTokens is the context window to request. Zero means 8192.
	ContextTokens int
	// MaxOutputTokens bounds one answer, so a model that starts repeating itself
	// cannot hold a run until the HTTP timeout. Zero means 1024.
	MaxOutputTokens int
	Client          *http.Client
}

func (o *Ollama) Name() string { return o.Model }

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name string `json:"name"`
		// The native API carries arguments as an object, not as a string.
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (o *Ollama) Complete(ctx context.Context, req Request) (Response, error) {
	contextTokens := o.ContextTokens
	if contextTokens <= 0 {
		contextTokens = 8192
	}
	maxOutput := o.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 1024
	}
	body := struct {
		Model    string          `json:"model"`
		Messages []ollamaMessage `json:"messages"`
		Tools    []wireTool      `json:"tools,omitempty"`
		Stream   bool            `json:"stream"`
		Options  map[string]any  `json:"options"`
	}{
		Model:   o.Model,
		Options: map[string]any{"num_ctx": contextTokens, "num_predict": maxOutput, "temperature": 0},
	}

	// A tool result is matched to its call by name, so remember which id belongs to which tool.
	names := map[string]string{}
	for _, message := range req.Messages {
		wire := ollamaMessage{Role: message.Role, Content: message.Content}
		for _, call := range message.ToolCalls {
			names[call.ID] = call.Name
			var wc ollamaToolCall
			wc.Function.Name = call.Name
			wc.Function.Arguments = call.Arguments
			if len(wc.Function.Arguments) == 0 {
				wc.Function.Arguments = json.RawMessage("{}")
			}
			wire.ToolCalls = append(wire.ToolCalls, wc)
		}
		if message.Role == "tool" {
			wire.ToolName = names[message.ToolCallID]
		}
		body.Messages = append(body.Messages, wire)
	}
	for _, tool := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = tool.Name
		wt.Function.Description = tool.Description
		wt.Function.Parameters = tool.Parameters
		body.Tools = append(body.Tools, wt)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/api/chat", bytes.NewReader(encoded))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("model: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("model: %s answered %d: %s", o.Model, resp.StatusCode, truncate(string(raw), 300))
	}

	var parsed struct {
		Message         ollamaMessage `json:"message"`
		PromptEvalCount int           `json:"prompt_eval_count"`
		EvalCount       int           `json:"eval_count"`
		Error           string        `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("model: decoding response: %w", err)
	}
	if parsed.Error != "" {
		return Response{}, fmt.Errorf("model: %s: %s", o.Model, parsed.Error)
	}
	out := Response{
		Content: parsed.Message.Content,
		Usage:   Usage{PromptTokens: parsed.PromptEvalCount, CompletionTokens: parsed.EvalCount},
	}
	for index, call := range parsed.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			// The native API has no call ids; the loop only needs them to be distinct within a turn
			// and stable across the conversation, which the prompt size at that point gives.
			ID:        fmt.Sprintf("call_%d_%d", parsed.PromptEvalCount, index),
			Name:      call.Function.Name,
			Arguments: normalizeArguments(call.Function.Arguments),
		})
	}
	return out, nil
}
