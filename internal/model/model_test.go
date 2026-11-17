package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScriptedReplaysStepsInOrderThenSubmits(t *testing.T) {
	backend := NewScripted(Script{Steps: []ScriptStep{
		{Say: "look", Calls: []ScriptCall{{Tool: "read_file", Args: map[string]any{"path": "a.py"}}}},
		{Say: "fix", Calls: []ScriptCall{
			{Tool: "replace_in_file", Args: map[string]any{"path": "a.py", "old_text": "x", "new_text": "y"}},
			{Tool: "run_tests", Args: map[string]any{}},
		}},
	}})

	first, _ := backend.Complete(context.Background(), Request{})
	second, _ := backend.Complete(context.Background(), Request{})
	third, _ := backend.Complete(context.Background(), Request{})

	if first.Content != "look" || len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "read_file" {
		t.Fatalf("first step: %+v", first)
	}
	if string(first.ToolCalls[0].Arguments) != `{"path":"a.py"}` {
		t.Fatalf("arguments: %s", first.ToolCalls[0].Arguments)
	}
	if len(second.ToolCalls) != 2 || second.ToolCalls[0].ID == second.ToolCalls[1].ID {
		t.Fatalf("second step needs two calls with distinct ids: %+v", second.ToolCalls)
	}
	// Past the end of the recording it stops rather than inventing work.
	if len(third.ToolCalls) != 1 || third.ToolCalls[0].Name != "submit" {
		t.Fatalf("after the script: %+v", third)
	}
}

func TestOpenAISendsToolsAndHistoryInTheWireFormat(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"reading",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.py\"}"}}]}}],
			"usage":{"prompt_tokens":120,"completion_tokens":9}}`))
	}))
	defer server.Close()

	backend := &OpenAI{BaseURL: server.URL + "/v1/", Model: "test-model", APIKey: "secret"}
	response, err := backend.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_0", Name: "list_files", Arguments: json.RawMessage(`{"path":"src"}`)}}},
			{Role: "tool", ToolCallID: "call_0", Content: "a.py"},
		},
		Tools: []Tool{{Name: "read_file", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if response.Content != "reading" || response.Usage.PromptTokens != 120 || response.Usage.CompletionTokens != 9 {
		t.Fatalf("response: %+v", response)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "call_1" || string(response.ToolCalls[0].Arguments) != `{"path":"a.py"}` {
		t.Fatalf("tool calls: %+v", response.ToolCalls)
	}

	if received["model"] != "test-model" || received["stream"] != false {
		t.Fatalf("request: %v", received)
	}
	messages := received["messages"].([]any)
	assistant := messages[1].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	// Arguments go out as a JSON document inside a string, as the API specifies.
	if call["type"] != "function" || call["function"].(map[string]any)["arguments"] != `{"path":"src"}` {
		t.Fatalf("history tool call: %v", call)
	}
	if messages[2].(map[string]any)["tool_call_id"] != "call_0" {
		t.Fatalf("tool result: %v", messages[2])
	}
	tool := received["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["function"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools: %v", tool)
	}
}

func TestOpenAIAcceptsArgumentsAsAnObjectAndFillsMissingIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Some servers send the arguments as an object and leave the id empty.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",
			"tool_calls":[{"type":"function","function":{"name":"a","arguments":{"path":"x"}}},
			              {"type":"function","function":{"name":"b","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	response, err := (&OpenAI{BaseURL: server.URL, Model: "m"}).Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.ToolCalls[0].Arguments) != `{"path":"x"}` || string(response.ToolCalls[1].Arguments) != `{}` {
		t.Fatalf("arguments: %s / %s", response.ToolCalls[0].Arguments, response.ToolCalls[1].Arguments)
	}
	if response.ToolCalls[0].ID == "" || response.ToolCalls[0].ID == response.ToolCalls[1].ID {
		t.Fatalf("ids must be present and distinct: %+v", response.ToolCalls)
	}
}

func TestOpenAISurfacesServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	_, err := (&OpenAI{BaseURL: server.URL, Model: "missing"}).Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("expected the server's error, got %v", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"choices":[]}`)) }))
	defer empty.Close()
	if _, err := (&OpenAI{BaseURL: empty.URL, Model: "m"}).Complete(context.Background(), Request{}); err == nil {
		t.Fatal("a response with no choices should be an error")
	}
}
