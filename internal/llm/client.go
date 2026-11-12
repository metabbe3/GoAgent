package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	Model     string           `json:"model"`
	Messages  []ChatMessage    `json:"messages"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
	MaxTokens int              `json:"max_tokens,omitempty"`
}

// ChatChoice represents an individual choice in the completion response.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage captures token counts of an LLM completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add combines two token usage measurements.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
	}
}

// ChatResponse represents the response payload from /chat/completions.
type ChatResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
	Error   *APIError    `json:"error,omitempty"`
}

// APIError represents an error returned by the OpenAI API.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// Client is an OpenAI-compatible HTTP client for chat completions and embeddings.
type Client struct {
	baseURL      string
	apiKey       string
	model        string
	heavyModel   string
	embedURL     string
	embedModel   string
	embedArrayOK bool
	httpClient   *http.Client
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

	embedURL := cfg.LLMEmbedURL
	if embedURL != "" {
		embedURL = strings.TrimRight(embedURL, "/")
	}

	return &Client{
		baseURL:      baseURL,
		apiKey:       cfg.LLMAPIKey,
		model:        model,
		heavyModel:   cfg.LLMHeavyModel,
		embedURL:     embedURL,
		embedModel:   cfg.LLMEmbedModel,
		embedArrayOK: true,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Model returns the primary model name.
func (c *Client) Model() string { return c.model }

// HeavyModel returns the configured heavy escalation model, or empty if unset.
func (c *Client) HeavyModel() string { return c.heavyModel }

// EmbedModel returns the configured embedding model.
func (c *Client) EmbedModel() string { return c.embedModel }

// ChatCompletion sends a request to the LLM provider and returns the assistant's reply.
func (c *Client) ChatCompletion(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (*ChatMessage, error) {
	reqBody := ChatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
	}
	return c.doCompletion(ctx, reqBody)
}

// ChatCompletionHeavy retries the completion on the heavy (smarter) model.
func (c *Client) ChatCompletionHeavy(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (*ChatMessage, error) {
	if c.heavyModel == "" {
		return nil, fmt.Errorf("no heavy model configured")
	}
	reqBody := ChatRequest{
		Model:    c.heavyModel,
		Messages: messages,
		Tools:    tools,
	}
	return c.doCompletion(ctx, reqBody)
}

// Ping verifies connectivity to the LLM endpoint by requesting a minimal completion.
func (c *Client) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqBody := ChatRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "user", Content: "ping"},
		},
		MaxTokens: 1,
	}
	_, err := c.doCompletion(pingCtx, reqBody)
	if err != nil && (strings.Contains(err.Error(), "max_tokens") || strings.Contains(err.Error(), "400")) {
		reqBody.MaxTokens = 0
		_, err = c.doCompletion(pingCtx, reqBody)
	}
	return err
}

func (c *Client) doCompletion(ctx context.Context, reqBody ChatRequest) (*ChatMessage, error) {
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

// embeddingResult is one row of the /embeddings response.
type embeddingResult struct {
	Index int       `json:"index"`
	Embed []float32 `json:"embedding"`
}

// Embed calls POST /embeddings for a batch of inputs and returns vectors in input order.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if c.embedModel == "" && c.embedURL == "" {
		return nil, fmt.Errorf("embedding not configured")
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	if !c.embedArrayOK {
		return c.embedOneByOne(ctx, inputs)
	}
	out, err := c.embedBatch(ctx, inputs)
	if err == nil {
		return out, nil
	}
	c.embedArrayOK = false
	return c.embedOneByOne(ctx, inputs)
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	out, err := c.embedCall(ctx, inputs)
	if err != nil {
		return nil, err
	}
	if len(out) != len(inputs) {
		return nil, fmt.Errorf("embeddings: want %d vectors, got %d", len(inputs), len(out))
	}
	return out, nil
}

func (c *Client) embedOneByOne(ctx context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for _, in := range inputs {
		vec, err := c.embedCall(ctx, []string{in})
		if err != nil {
			return nil, err
		}
		out = append(out, vec[0])
	}
	return out, nil
}

func (c *Client) embedCall(ctx context.Context, inputs []string) ([][]float32, error) {
	var input any = inputs[0]
	if len(inputs) > 1 {
		input = inputs
	}
	payload, err := json.Marshal(map[string]any{
		"model": c.embedModel,
		"input": input,
	})
	if err != nil {
		return nil, err
	}

	embedEndpoint := c.baseURL + "/embeddings"
	if c.embedURL != "" {
		embedEndpoint = c.embedURL
		if !strings.HasSuffix(embedEndpoint, "/embeddings") {
			embedEndpoint += "/embeddings"
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embedEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings failed (status %d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Data  []embeddingResult `json:"data"`
		Error *APIError         `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embeddings api error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("embeddings: empty data returned")
	}

	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = d.Embed
	}
	return out, nil
}
