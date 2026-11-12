package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/tools"
)

func TestAgentLoopWithToolCall(t *testing.T) {
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		var chatReq llm.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&chatReq)

		var resp llm.ChatResponse
		if step == 1 {
			// First call: model requests tool 'list_dir'
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role: "assistant",
							ToolCalls: []llm.ToolCall{
								{
									ID:   "call_step1",
									Type: "function",
									Function: llm.FunctionCall{
										Name:      "list_dir",
										Arguments: `{"path":"."}`,
									},
								},
							},
						},
						FinishReason: "tool_calls",
					},
				},
			}
		} else {
			// Second call: model responds with final answer after receiving tool output
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: "Found the files successfully.",
						},
						FinishReason: "stop",
					},
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMAPIKey:  "mock-key",
		LLMModel:   "mock-model",
	}

	llmClient := llm.NewClient(cfg)
	reg := tools.NewRegistry(cfg)
	agentLoop := NewLoop(llmClient, reg, 5)

	finalText, messages, err := agentLoop.Run(context.Background(), "list current directory", nil)
	if err != nil {
		t.Fatalf("agent loop failed: %v", err)
	}

	if finalText != "Found the files successfully." {
		t.Errorf("unexpected final text: %s", finalText)
	}

	// Verify messages: system, user, assistant (tool_call), tool (result), assistant (final)
	if len(messages) < 5 {
		t.Errorf("expected at least 5 messages in conversation history, got %d", len(messages))
	}

	hasToolResult := false
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call_step1" && strings.Contains(m.Content, "Directory listing") {
			hasToolResult = true
			break
		}
	}
	if !hasToolResult {
		t.Errorf("conversation history missing tool result")
	}
}

func TestParallelToolCallsDeterministicOrder(t *testing.T) {
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		var resp llm.ChatResponse
		if step == 1 {
			// First response: request 2 tools simultaneously
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role: "assistant",
							ToolCalls: []llm.ToolCall{
								{
									ID:   "call_first",
									Type: "function",
									Function: llm.FunctionCall{
										Name:      "list_dir",
										Arguments: `{"path":"."}`,
									},
								},
								{
									ID:   "call_second",
									Type: "function",
									Function: llm.FunctionCall{
										Name:      "run_command",
										Arguments: `{"cmd":"echo 'second tool result'"}`,
									},
								},
							},
						},
						FinishReason: "tool_calls",
					},
				},
			}
		} else {
			// Second response: final answer
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: "Completed parallel tasks.",
						},
						FinishReason: "stop",
					},
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMAPIKey:  "mock-key",
		LLMModel:   "mock-model",
		AllowShell: false,
	}

	llmClient := llm.NewClient(cfg)
	reg := tools.NewRegistry(cfg)
	agentLoop := NewLoop(llmClient, reg, 5)

	finalText, messages, err := agentLoop.Run(context.Background(), "run parallel diagnostics", nil)
	if err != nil {
		t.Fatalf("agent loop failed: %v", err)
	}

	if finalText != "Completed parallel tasks." {
		t.Errorf("unexpected final text: %s", finalText)
	}

	// Verify that tool results appear in exact deterministic order: call_first, then call_second
	var toolResults []llm.ChatMessage
	for _, m := range messages {
		if m.Role == "tool" {
			toolResults = append(toolResults, m)
		}
	}

	if len(toolResults) != 2 {
		t.Fatalf("expected 2 tool results, got %d", len(toolResults))
	}

	if toolResults[0].ToolCallID != "call_first" {
		t.Errorf("expected first tool result to be call_first, got: %s", toolResults[0].ToolCallID)
	}
	if toolResults[1].ToolCallID != "call_second" {
		t.Errorf("expected second tool result to be call_second, got: %s", toolResults[1].ToolCallID)
	}
}

func TestToolResultTruncation(t *testing.T) {
	longOutput := strings.Repeat("X", 10000)
	truncated := TruncateToolOutput(longOutput, 8000)

	if !strings.HasPrefix(truncated, strings.Repeat("X", 8000)) {
		t.Errorf("expected prefix of 8000 characters")
	}
	if !strings.Contains(truncated, "...[truncated 2000 bytes]") {
		t.Errorf("expected truncation footer, got: %s", truncated[7990:])
	}
	if len(truncated) > 8100 {
		t.Errorf("truncated string too long: %d", len(truncated))
	}
}

func TestContextBudgetPruning(t *testing.T) {
	// Total budget > 60,000 chars
	oldToolContent := strings.Repeat("LOG LINE\n", 6000)    // ~54,000 chars
	newToolContent := strings.Repeat("ANOTHER LOG\n", 2000) // ~24,000 chars

	messages := []llm.ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Analyze first log"},
		{Role: "tool", ToolCallID: "call_1", Content: oldToolContent},
		{Role: "assistant", Content: "Done with first"},
		{Role: "user", Content: "Analyze second log"},
		{Role: "tool", ToolCallID: "call_2", Content: newToolContent},
	}

	PruneContextBudget(messages, 60000)

	// Oldest tool result should be omitted
	if messages[2].Content != "[tool result omitted]" {
		t.Errorf("expected oldest tool result to be omitted, got length %d", len(messages[2].Content))
	}

	// User and assistant messages must be kept intact
	if messages[0].Content != "System prompt" || messages[1].Content != "Analyze first log" || messages[3].Content != "Done with first" {
		t.Errorf("user or assistant message content was corrupted during pruning")
	}
}

func TestToolPanicRecover(t *testing.T) {
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		var resp llm.ChatResponse
		if step == 1 {
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role: "assistant",
							ToolCalls: []llm.ToolCall{
								{
									ID:   "call_panic",
									Type: "function",
									Function: llm.FunctionCall{
										Name:      "panic_tool",
										Arguments: "{}",
									},
								},
							},
						},
						FinishReason: "tool_calls",
					},
				},
			}
		} else {
			resp = llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: "Recovered from panic.",
						},
						FinishReason: "stop",
					},
				},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMAPIKey:  "key",
		LLMModel:   "model",
	}
	llmClient := llm.NewClient(cfg)
	reg := tools.NewRegistry(cfg)

	// Register a tool that deliberately panics
	reg.Register(llm.ToolDefinition{
		Type:     "function",
		Function: llm.ToolFunctionDef{Name: "panic_tool"},
	}, func(_ context.Context, args string) (string, error) {
		panic("simulated explosion")
	})

	agentLoop := NewLoop(llmClient, reg, 5)
	reply, msgs, err := agentLoop.Run(context.Background(), "test panic", nil)
	if err != nil {
		t.Fatalf("agent should not crash on tool panic: %v", err)
	}
	if reply != "Recovered from panic." {
		t.Errorf("unexpected reply: %s", reply)
	}

	foundToolErr := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "Error: internal tool failure") {
			foundToolErr = true
			break
		}
	}
	if !foundToolErr {
		t.Errorf("tool result did not capture recovered panic error")
	}
}

func TestHourPrecisionClock(t *testing.T) {
	loop := NewLoop(nil, tools.NewRegistry(nil), 5)
	sys := loop.buildSystemPrompt()

	if !strings.Contains(sys, "Current date/time (hour precision):") {
		t.Fatalf("system prompt missing hour precision clock header: %s", sys)
	}
	if !strings.Contains(sys, "h UTC") {
		t.Fatalf("clock format should contain 'h UTC', got: %s", sys)
	}
}

func TestLoopToolContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := llm.ChatResponse{
			ID: "chatcmpl-slow",
			Choices: []llm.ChatChoice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role: "assistant",
						ToolCalls: []llm.ToolCall{
							{
								ID:   "call_slow_1",
								Type: "function",
								Function: llm.FunctionCall{
									Name:      "slow_tool",
									Arguments: "{}",
								},
							},
						},
					},
					FinishReason: "tool_calls",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMBaseURL: server.URL,
		LLMAPIKey:  "key",
		LLMModel:   "model",
	}
	llmClient := llm.NewClient(cfg)
	reg := tools.NewRegistry(cfg)

	reg.Register(llm.ToolDefinition{
		Type:     "function",
		Function: llm.ToolFunctionDef{Name: "slow_tool"},
	}, func(ctx context.Context, _ string) (string, error) {
		select {
		case <-time.After(10 * time.Second):
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	agentLoop := NewLoop(llmClient, reg, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := agentLoop.Run(ctx, "run slow", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error due to context timeout, got nil")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("loop should complete in < 3s when ctx times out, took %v", elapsed)
	}
}

func TestLogContextCorrelation(t *testing.T) {
	// 1. Empty context
	taskID, missionID := FromContext(context.Background())
	if taskID != "" || missionID != "" {
		t.Errorf("expected empty taskID and missionID, got: %q, %q", taskID, missionID)
	}
	if p := LogPrefix(context.Background()); p != "" {
		t.Errorf("expected empty log prefix, got: %q", p)
	}

	// 2. Context with task_id only
	ctx := context.WithValue(context.Background(), TaskIDKey, "task-999")
	taskID, missionID = FromContext(ctx)
	if taskID != "task-999" || missionID != "" {
		t.Errorf("expected task-999 and empty missionID, got: %q, %q", taskID, missionID)
	}
	if p := LogPrefix(ctx); p != "[task task-999] " {
		t.Errorf("unexpected prefix: %q", p)
	}

	// 3. Context with both task_id and mission_id
	ctx = context.WithValue(ctx, MissionIDKey, "mission-111")
	taskID, missionID = FromContext(ctx)
	if taskID != "task-999" || missionID != "mission-111" {
		t.Errorf("expected task-999 and mission-111, got: %q, %q", taskID, missionID)
	}
	if p := LogPrefix(ctx); p != "[task task-999] " {
		t.Errorf("unexpected prefix: %q", p)
	}

	// 4. Context with mission_id only
	ctxMissionOnly := context.WithValue(context.Background(), MissionIDKey, "mission-222")
	taskID, missionID = FromContext(ctxMissionOnly)
	if taskID != "" || missionID != "mission-222" {
		t.Errorf("expected empty taskID and mission-222, got: %q, %q", taskID, missionID)
	}
	if p := LogPrefix(ctxMissionOnly); p != "[mission mission-222] " {
		t.Errorf("unexpected prefix: %q", p)
	}
}
