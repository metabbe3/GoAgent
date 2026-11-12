package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/metabbe3/GoAgent/internal/config"
)

func TestDecodeResponseFixtureWithToolCalls(t *testing.T) {
	fixture := `{
		"id": "chatcmpl-test-123",
		"object": "chat.completion",
		"created": 1677652288,
		"model": "gpt-4o-mini",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_abc123",
					"type": "function",
					"function": {
						"name": "analyze_logs",
						"arguments": "{\"path\":\"/var/log/app.log\",\"top\":5}"
					}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`

	var resp ChatResponse
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("failed to unmarshal fixture: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}

	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got '%s'", choice.FinishReason)
	}

	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(choice.Message.ToolCalls))
	}

	tc := choice.Message.ToolCalls[0]
	if tc.ID != "call_abc123" {
		t.Errorf("expected id 'call_abc123', got '%s'", tc.ID)
	}
	if tc.Function.Name != "analyze_logs" {
		t.Errorf("expected function name 'analyze_logs', got '%s'", tc.Function.Name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("failed to parse arguments JSON: %v", err)
	}
	if args["path"] != "/var/log/app.log" {
		t.Errorf("expected path '/var/log/app.log', got '%v'", args["path"])
	}
}

func TestChatCompletionMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got '%s'", r.Header.Get("Authorization"))
		}

		resp := ChatResponse{
			ID: "chatcmpl-mock",
			Choices: []ChatChoice{
				{
					Index: 0,
					Message: ChatMessage{
						Role:    "assistant",
						Content: "Hello from mock LLM!",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMAPIKey:  "test-key",
		LLMModel:   "test-model",
	}

	client := NewClient(cfg)
	msg, err := client.ChatCompletion(context.Background(), []ChatMessage{
		{Role: "user", Content: "Hi"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Content != "Hello from mock LLM!" {
		t.Errorf("unexpected message content: %s", msg.Content)
	}
}

func TestChatCompletionHeavy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "heavy-model-pro" {
			t.Errorf("expected model heavy-model-pro, got %s", req.Model)
		}

		resp := ChatResponse{
			ID: "chatcmpl-heavy",
			Choices: []ChatChoice{
				{
					Index: 0,
					Message: ChatMessage{
						Role:    "assistant",
						Content: "Solved on heavy model!",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL:    server.URL,
		LLMAPIKey:     "test-key",
		LLMModel:      "small-model",
		LLMHeavyModel: "heavy-model-pro",
	}

	client := NewClient(cfg)
	msg, err := client.ChatCompletionHeavy(context.Background(), []ChatMessage{
		{Role: "user", Content: "Hard problem"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content != "Solved on heavy model!" {
		t.Errorf("unexpected heavy content: %s", msg.Content)
	}
}

func TestEmbeddings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{0.1, 0.2, 0.3}},
				{"index": 1, "embedding": []float32{0.4, 0.5, 0.6}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL:    server.URL,
		LLMEmbedModel: "text-embedding-3-small",
	}
	client := NewClient(cfg)
	vecs, err := client.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("unexpected vecs output: %v", vecs)
	}
}

func TestPingSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.MaxTokens != 1 {
			t.Errorf("expected max_tokens 1, got %d", req.MaxTokens)
		}
		resp := ChatResponse{
			ID: "chatcmpl-ping",
			Choices: []ChatChoice{
				{
					Index: 0,
					Message: ChatMessage{
						Role:    "assistant",
						Content: "pong",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMModel:   "test-model",
	}
	client := NewClient(cfg)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("expected ping success, got error: %v", err)
	}
}

func TestPingTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hangs
		time.Sleep(2 * time.Second)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMModel:   "test-model",
	}
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.Ping(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected ping timeout error, got nil")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("ping did not return promptly on timeout, took %v", elapsed)
	}
}
