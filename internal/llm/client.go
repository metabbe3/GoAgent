package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/config"
)

// FunctionCall represents the function called by the assistant.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents a tool call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

// ChatMessage represents a single message in the chat conversation.
type ChatMessage struct {
	Role       string     `json:"role"`                   // "system", "user", "assistant", "tool"
	Content    string     `json:"content,omitempty"`      // text content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // if assistant requests tool calls
	ToolCallID string     `json:"tool_call_id,omitempty"` // if role is "tool"
	Name       string     `json:"name,omitempty"`         // optional function name for tool
}

// ToolDefinition represents the OpenAI tool definition schema.
type ToolDefinition struct {
	Type     string          `json:"type"` // "function"
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef represents the metadata and parameters of a tool function.
type ToolFunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"` // JSON Schema map or struct
}

// ChatRequest represents the payload sent to /chat/completions.
type ChatRequest struct {
	Model    string           `json:"model"`
	Messages []ChatMessage    `json:"messages"`
	Tools    []ToolDefinition `json:"tools,omitempty"`
}

// ChatChoice represents an individual choice in the completion response.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatResponse represents the response payload from /chat/completions.
type ChatResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Error   *APIError    `json:"error,omitempty"`
}

// APIError represents an error returned by the OpenAI API.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// Client is an OpenAI-compatible HTTP client for chat completions.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient initializes a new LLM client.
func NewClient(cfg *config.Config) *Client {
	baseURL := cfg.LLMBaseURL
	if baseURL == "" {
		baseURL = config.DefaultLLMBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	model := cfg.LLMModel
	if model == "" {
		model = config.DefaultLLMModel
	}

	return &Client{
		baseURL: baseURL,
		apiKey:  cfg.LLMAPIKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// ChatCompletion sends a request to the LLM provider and returns the assistant's reply.
func (c *Client) ChatCompletion(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (*ChatMessage, error) {
	reqBody := ChatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr ChatResponse
		if err := json.Unmarshal(bodyBytes, &apiErr); err == nil && apiErr.Error != nil {
			return nil, fmt.Errorf("llm api error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("llm api error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices in response")
	}

	return &chatResp.Choices[0].Message, nil
}
