package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
