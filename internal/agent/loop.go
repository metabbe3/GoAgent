package agent

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metabbe3/GoAgent/internal/guardrail"
	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/tools"
)

const (
	MaxToolResultBytes = 8000
	MaxContextChars    = 60000
	MaxConcurrentTools = 4
)

type ctxKey string

const (
	TaskIDKey    ctxKey = "task_id"
	MissionIDKey ctxKey = "mission_id"
)

// FromContext extracts task_id and mission_id from context.
func FromContext(ctx context.Context) (taskID, missionID string) {
	if ctx == nil {
		return "", ""
	}
	if v, ok := ctx.Value(TaskIDKey).(string); ok {
		taskID = v
	}
	if v, ok := ctx.Value(MissionIDKey).(string); ok {
		missionID = v
	}
	return taskID, missionID
}

// LogPrefix returns a prefix string like "[task <id>] " if task_id or mission_id is in ctx.
func LogPrefix(ctx context.Context) string {
	taskID, missionID := FromContext(ctx)
	if taskID != "" {
		return fmt.Sprintf("[task %s] ", taskID)
	}
	if missionID != "" {
		return fmt.Sprintf("[mission %s] ", missionID)
	}
	return ""
}

// Loop manages the iterative conversation cycle between user, LLM, and tools.
type Loop struct {
	llmClient     *llm.Client
	registry      *tools.Registry
	maxIterations int
	role          string
	SystemExtras  func() string
}

// NewLoop creates a new agent loop runner.
func NewLoop(llmClient *llm.Client, registry *tools.Registry, maxIterations int) *Loop {
	if maxIterations <= 0 {
		maxIterations = 8
	}
	return &Loop{
		llmClient:     llmClient,
		registry:      registry,
		maxIterations: maxIterations,
		role:          "solo",
	}
}

// SetRole sets the mesh role ("solo", "leader", "worker").
func (l *Loop) SetRole(role string) {
	l.role = role
}

// Role returns the configured mesh role.
func (l *Loop) Role() string {
	return l.role
}

// Registry returns the underlying tool registry.
func (l *Loop) Registry() *tools.Registry {
	return l.registry
}

type toolResult struct {
	index      int
	toolCallID string
	name       string
	content    string
}

// Run executes the agent cycle with a user message and returns the final assistant reply.
func (l *Loop) Run(ctx context.Context, userText string, history []llm.ChatMessage) (string, []llm.ChatMessage, error) {
	return l.runWithEscalation(ctx, userText, history)
}

func (l *Loop) runWithEscalation(ctx context.Context, userText string, history []llm.ChatMessage) (string, []llm.ChatMessage, error) {
	reply, msgs, err := l.run(ctx, userText, history, false)
	if err == nil || l.llmClient == nil || l.llmClient.HeavyModel() == "" || ctx.Err() != nil {
		return reply, msgs, err
	}

	log.Printf("[agent] %sescalating to heavy model %q after primary run: %v", LogPrefix(ctx), l.llmClient.HeavyModel(), err)
	// Escalation retry: shallow copy with half the iteration budget, no mutation of shared state
	retry := *l
	retry.maxIterations = max(l.maxIterations/2, 2)
	reply2, msgs2, err2 := retry.run(ctx, userText, history, true)
	if err2 != nil {
		return reply, msgs, err // original outcome
	}
	return reply2, msgs2, nil
}

func (l *Loop) run(ctx context.Context, userText string, history []llm.ChatMessage, heavy bool) (string, []llm.ChatMessage, error) {
	var messages []llm.ChatMessage

	// Assemble system prompt with clock, role, and dynamic extras
	messages = append(messages, llm.ChatMessage{
		Role:    "system",
		Content: l.buildSystemPrompt(),
	})

	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		messages = append(messages, m)
	}

	messages = append(messages, llm.ChatMessage{
		Role:    "user",
		Content: userText,
	})

	toolDefs := l.registry.GetDefinitions()

	for iter := 0; iter < l.maxIterations; iter++ {
		PruneContextBudget(messages, MaxContextChars)
		log.Printf("[agent] %siteration %d/%d", LogPrefix(ctx), iter+1, l.maxIterations)

		var assistantMsg *llm.ChatMessage
		var err error

		if heavy {
			assistantMsg, err = l.llmClient.ChatCompletionHeavy(ctx, messages, toolDefs)
		} else {
			assistantMsg, err = l.llmClient.ChatCompletion(ctx, messages, toolDefs)
		}

		if err != nil {
			log.Printf("[agent] %sllm error on iteration %d: %v", LogPrefix(ctx), iter+1, err)
			return "", messages, fmt.Errorf("agent llm error: %w", err)
		}

		messages = append(messages, *assistantMsg)

		// If no tool calls requested, we have the final answer
		if len(assistantMsg.ToolCalls) == 0 {
			return assistantMsg.Content, messages, nil
		}

		// Parallel execution of tool calls (max 4 concurrent), preserving deterministic order
		toolCalls := assistantMsg.ToolCalls
		results := make([]toolResult, len(toolCalls))
		var resultsMu sync.Mutex
		sem := make(chan struct{}, MaxConcurrentTools)
		var wg sync.WaitGroup

		for i, tc := range toolCalls {
			wg.Add(1)
			go func(idx int, call llm.ToolCall) {
				defer wg.Done()

				// Per-goroutine panic recovery prevents child panic from crashing the process
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[agent] %sPANIC in tool %s: %v\n%s", LogPrefix(ctx), call.Function.Name, r, debug.Stack())
						resultsMu.Lock()
						results[idx] = toolResult{
							index:      idx,
							toolCallID: call.ID,
							name:       call.Function.Name,
							content:    fmt.Sprintf("Error: internal tool failure (%s): %v", call.Function.Name, r),
						}
						resultsMu.Unlock()
					}
				}()

				sem <- struct{}{}
				defer func() { <-sem }()

				log.Printf("[agent] %sexecuting tool %s (id: %s) [worker]", LogPrefix(ctx), call.Function.Name, call.ID)
				output, err := l.registry.Execute(ctx, call.Function.Name, call.Function.Arguments)
				if err != nil {
					output = fmt.Sprintf("Error executing tool: %v", err)
				}

				truncated := TruncateToolOutput(output, l.registry.ResultCap(call.Function.Name))
				redacted, _ := guardrail.Redact(truncated)

				resultsMu.Lock()
				results[idx] = toolResult{
					index:      idx,
					toolCallID: call.ID,
					name:       call.Function.Name,
					content:    redacted,
				}
				resultsMu.Unlock()
			}(i, tc)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			// remaining goroutines will finish on their own; results not yet gathered are considered timeout failures
		}

		for idx, tc := range toolCalls {
			resultsMu.Lock()
			res := results[idx]
			resultsMu.Unlock()
			if res.toolCallID == "" {
				res = toolResult{
					index:      idx,
					toolCallID: tc.ID,
					name:       tc.Function.Name,
					content:    fmt.Sprintf("Error executing %s: context cancelled or timed out: %v", tc.Function.Name, ctx.Err()),
				}
			}
			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				ToolCallID: res.toolCallID,
				Name:       res.name,
				Content:    res.content,
			})
		}

		if ctx.Err() != nil {
			return "", messages, ctx.Err()
		}
	}

	return "", messages, fmt.Errorf("agent hit the %d-iteration tool budget without a final answer — narrow the query", l.maxIterations)
}

func (l *Loop) buildSystemPrompt() string {
	sys := SystemPrompt

	switch l.role {
	case "leader":
		sys += "\n\n" + LeaderPrompt
	case "worker":
		sys += "\n\n" + WorkerPrompt
	}

	if l.SystemExtras != nil {
		if extra := l.SystemExtras(); extra != "" {
			sys += "\n\n## Additional Knowledge\n" + extra
		}
	}

	// Hour precision clock avoids defeating LLM prompt caching on each turn
	sys += "\n\nCurrent date/time (hour precision): " + time.Now().UTC().Format("Mon 2006-01-02 15h MST")
	return sys
}

// TruncateToolOutput caps output to maxBytes with a truncation notice on a rune boundary.
func TruncateToolOutput(output string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = MaxToolResultBytes
	}
	if len(output) > maxBytes {
		truncated := len(output) - maxBytes
		for maxBytes > 0 && !utf8.RuneStart(output[maxBytes]) {
			maxBytes--
		}
		return output[:maxBytes] + fmt.Sprintf("\n...[truncated %d bytes]", truncated)
	}
	return output
}

// PruneContextBudget omits oldest tool results when total message characters exceed maxChars.
func PruneContextBudget(messages []llm.ChatMessage, maxChars int) {
	if maxChars <= 0 {
		maxChars = MaxContextChars
	}
	total := 0
	for _, m := range messages {
		total += len(m.Content)
	}
	for total > maxChars {
		found := false
		for i := range messages {
			if messages[i].Role == "tool" && messages[i].Content != "[tool result omitted]" {
				total -= len(messages[i].Content)
				messages[i].Content = "[tool result omitted]"
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
