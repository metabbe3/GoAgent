package agent

import (
	"context"
	"fmt"
	"log"

	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/tools"
)

const SystemPrompt = `You are GoAgent, a general-purpose assistant specializing in log analysis, system diagnostics, and file operations.
You have access to tools for reading and writing files, inspecting directories, running safe shell commands, and analyzing logs:
- 'analyze_logs': ALWAYS prefer this tool for analyzing medium or large log files. It uses chunk-parallel processing to cluster log lines into templates, count severity levels, and detect anomalies and new patterns.
- 'read_log': Read the recent lines (tail) of a log file with optional regex filtering.
- 'learned_patterns': Inspect historical learned pattern memory across previous log analyses.
- 'read_file': Read any text file with line offset and limit.
- 'write_file': Write text content to a file.
- 'list_dir': List entries and metadata in a directory.
- 'run_command': Execute read-only diagnostic commands (ls, cat, grep, ps, etc.).

Be concise, accurate, and helpful. Format your responses with Markdown.`

// Loop manages the iterative conversation cycle between user, LLM, and tools.
type Loop struct {
	llmClient     *llm.Client
	registry      *tools.Registry
	maxIterations int
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
	}
}

// Run executes the agent cycle with a user message and returns the final assistant reply.
func (l *Loop) Run(ctx context.Context, userText string, history []llm.ChatMessage) (string, []llm.ChatMessage, error) {
	var messages []llm.ChatMessage

	// Ensure system prompt is first
	hasSystem := false
	for _, m := range history {
		if m.Role == "system" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		messages = append(messages, llm.ChatMessage{
			Role:    "system",
			Content: SystemPrompt,
		})
	}
	messages = append(messages, history...)
	messages = append(messages, llm.ChatMessage{
		Role:    "user",
		Content: userText,
	})

	toolDefs := l.registry.GetDefinitions()

	for iter := 0; iter < l.maxIterations; iter++ {
		assistantMsg, err := l.llmClient.ChatCompletion(ctx, messages, toolDefs)
		if err != nil {
			return "", messages, fmt.Errorf("agent llm error: %w", err)
		}

		messages = append(messages, *assistantMsg)

		// If no tool calls requested, we have the final answer
		if len(assistantMsg.ToolCalls) == 0 {
			return assistantMsg.Content, messages, nil
		}

		// Execute tool calls
		for _, tc := range assistantMsg.ToolCalls {
			log.Printf("[agent] executing tool %s (id: %s)", tc.Function.Name, tc.ID)
			output, err := l.registry.Execute(tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				output = fmt.Sprintf("Error executing tool: %v", err)
			}

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    output,
			})
		}
	}

	return "Reached maximum tool execution iterations without final conclusion. Please narrow your query.", messages, nil
}
