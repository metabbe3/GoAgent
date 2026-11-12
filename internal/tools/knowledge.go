package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/metabbe3/GoAgent/internal/kb"
	"github.com/metabbe3/GoAgent/internal/llm"
)

const (
	kbTitleChars   = 200
	kbContentChars = 100_000
	searchTopK     = 6
)

func (r *Registry) RegisterKnowledgeTools(ix *kb.Index) {
	r.mu.Lock()
	r.kbIndex = ix
	r.mu.Unlock()

	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "search_knowledge",
			Description: "Search the local knowledge base (RAG) for procedures, notes, and uploaded documentation. " +
				"Returns the top relevant excerpts with document titles.",
			Parameters: schema(
				map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query keywords or question"},
				},
				[]string{"query"},
			),
		},
	}, r.handleSearchKnowledge)

	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "save_knowledge",
			Description: "Save important reference text, documentation, or procedures into the team knowledge base " +
				"so it persists across conversations.",
			Parameters: schema(
				map[string]any{
					"title":   map[string]any{"type": "string", "description": "Document title"},
					"content": map[string]any{"type": "string", "description": "Text content to save"},
				},
				[]string{"title", "content"},
			),
		},
	}, r.handleSaveKnowledge)
}

func (r *Registry) handleSearchKnowledge(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	q := strings.TrimSpace(args.Query)
	if q == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	r.mu.RLock()
	ix := r.kbIndex
	r.mu.RUnlock()

	if ix == nil {
		return "", fmt.Errorf("embedding not configured")
	}

	hits, err := ix.Search(ctx, q, searchTopK)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No knowledge-base documents match the query.", nil
	}

	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "### match %d — %s (part %d)\n%s\n\n", i+1, h.Doc, h.Seq+1, h.Text)
	}
	return strings.TrimSpace(sb.String()), nil
}

func (r *Registry) handleSaveKnowledge(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	title := strings.TrimSpace(args.Title)
	content := strings.TrimSpace(args.Content)
	if title == "" || len(title) > kbTitleChars {
		return "", fmt.Errorf("title must be between 1 and %d characters", kbTitleChars)
	}
	if content == "" || len(content) > kbContentChars {
		return "", fmt.Errorf("content must be between 1 and %d characters", kbContentChars)
	}

	r.mu.RLock()
	ix := r.kbIndex
	r.mu.RUnlock()

	if ix == nil {
		return "", fmt.Errorf("embedding not configured")
	}

	if err := ix.Save(ctx, title, content); err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved %d characters under %q into knowledge base. Use search_knowledge to query it.", len(content), title), nil
}
