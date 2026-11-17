// Package model is the boundary between the agent loop and whatever produces
// the next step: a hosted model, a local one, or a recorded script.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message is one turn of the conversation, in the shape every chat API shares.
type Message struct {
	Role       string     `json:"role"` // system, user, assistant or tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

// ToolCall is a request from the model to run one tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Tool describes a tool the model may call. Parameters is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request is everything the model needs to produce the next turn.
type Request struct {
	Messages []Message
	Tools    []Tool
}

// Usage counts tokens, when the backend reports them.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
}

// Response is the model's next turn: text, tool calls, or both.
type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
}

// Backend produces the next turn of a conversation. One Backend value serves
// one run: implementations may keep per-run state.
type Backend interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// ---------------------------------------------------------------- OpenAI-compatible

// OpenAI talks to any server that implements the chat completions API with
// tool calling. That includes local servers such as Ollama, which is what the
// evaluation in this repository uses.
type OpenAI struct {
	BaseURL string // e.g. http://localhost:11434/v1
	Model   string
	APIKey  string
	Client  *http.Client
}

func (o *OpenAI) Name() string { return o.Model }

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body := struct {
		Model       string        `json:"model"`
		Messages    []wireMessage `json:"messages"`
		Tools       []wireTool    `json:"tools,omitempty"`
		Temperature float64       `json:"temperature"`
		Stream      bool          `json:"stream"`
	}{Model: o.Model}

	for _, message := range req.Messages {
		wire := wireMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			var wc wireToolCall
			wc.ID = call.ID
			wc.Type = "function"
			wc.Function.Name = call.Name
			// The API carries arguments as a JSON document inside a string.
			quoted, _ := json.Marshal(string(call.Arguments))
			wc.Function.Arguments = quoted
			wire.ToolCalls = append(wire.ToolCalls, wc)
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
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
		Choices []struct {
			Message wireMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("model: decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, errors.New("model: response has no choices")
	}
	message := parsed.Choices[0].Message
	out := Response{
		Content: message.Content,
		Usage:   Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens},
	}
	for index, call := range message.ToolCalls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: call.Function.Name, Arguments: normalizeArguments(call.Function.Arguments)})
	}
	return out, nil
}

// normalizeArguments accepts arguments either as a JSON string containing a
// document (what the specification says) or as the document itself (what some
// servers send) and returns the document.
func normalizeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var inner string
		if json.Unmarshal(trimmed, &inner) == nil {
			return json.RawMessage(strings.TrimSpace(inner))
		}
	}
	return trimmed
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------- scripted

// Script is a recorded sequence of assistant turns.
type Script struct {
	Steps []ScriptStep `json:"steps"`
}

// ScriptStep is one assistant turn: what it says and which tools it calls.
type ScriptStep struct {
	Say   string       `json:"say"`
	Calls []ScriptCall `json:"calls"`
}

// ScriptCall is one tool call in a scripted turn.
type ScriptCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Scripted replays a Script, one step per call, without looking at what the
// tools returned. It exists so that everything except the model can be
// exercised deterministically: the load test drives a hundred of these at
// once through real sandboxes, and the test suite uses it to pin the loop's
// behaviour. Results obtained with it say nothing about model quality.
type Scripted struct {
	script Script
	// ThinkTime, if set, is slept before each answer to imitate model latency.
	ThinkTime time.Duration

	mu   sync.Mutex
	next int
}

// NewScripted returns a backend that replays script from its first step.
func NewScripted(script Script) *Scripted { return &Scripted{script: script} }

func (s *Scripted) Name() string { return "scripted" }

func (s *Scripted) Complete(ctx context.Context, _ Request) (Response, error) {
	if s.ThinkTime > 0 {
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(s.ThinkTime):
		}
	}
	s.mu.Lock()
	index := s.next
	s.next++
	s.mu.Unlock()

	if index >= len(s.script.Steps) {
		// Past the end of the recording the only honest move is to stop.
		args, _ := json.Marshal(map[string]string{"summary": "script finished"})
		return Response{ToolCalls: []ToolCall{{ID: fmt.Sprintf("s%d", index), Name: "submit", Arguments: args}}}, nil
	}
	step := s.script.Steps[index]
	out := Response{Content: step.Say}
	for i, call := range step.Calls {
		args, err := json.Marshal(call.Args)
		if err != nil {
			return Response{}, err
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: fmt.Sprintf("s%d_%d", index, i), Name: call.Tool, Arguments: args})
	}
	return out, nil
}
