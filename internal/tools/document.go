package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/metabbe3/GoAgent/internal/guardrail"
	"github.com/metabbe3/GoAgent/internal/ingest"
	"github.com/metabbe3/GoAgent/internal/llm"
)

func (r *Registry) registerDocumentTools() {
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "read_document",
			Description: "Read and extract plain text from local documents and files (.txt, .md, .csv, .json, .log, .docx, .xlsx, .pdf). " +
				"Protected files (.env, /etc) are blocked by guardrails.",
			Parameters: schema(
				map[string]any{
					"path": map[string]any{"type": "string", "description": "Absolute or relative file path to the document"},
				},
				[]string{"path"},
			),
		},
	}, r.handleReadDocument)
}

func (r *Registry) handleReadDocument(_ context.Context, argsJSON string) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	filePath := strings.TrimSpace(args.Path)
	if filePath == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	cleanPath, err := guardrail.ValidatePath(filePath)
	if err != nil {
		return "", fmt.Errorf("access denied by path guard: %w", err)
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	text, err := ingest.ExtractText(cleanPath, data)
	if err != nil {
		return "", fmt.Errorf("document extraction failed: %w", err)
	}

	redacted, _ := guardrail.Redact(text)
	framed := guardrail.FrameDocumentContent("file "+filepath.Base(cleanPath), redacted)
	return ingest.Truncate(framed), nil
}
