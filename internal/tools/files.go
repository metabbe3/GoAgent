package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/metabbe3/GoAgent/internal/llm"
)

type readFileInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type listDirInput struct {
	Path string `json:"path"`
}

func (r *Registry) registerFilesTools() {
	// read_file
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "read_file",
			Description: "Read contents of a file with optional line offset and limit.",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to read.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Starting line number (1-based, default 1).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to read (default 200).",
				},
			}, []string{"path"}),
		},
	}, r.handleReadFile)

	// write_file
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "write_file",
			Description: "Write full content to a file, creating parent directories if necessary.",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Target file path.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Text content to write.",
				},
			}, []string{"path", "content"}),
		},
	}, r.handleWriteFile)

	// list_dir
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "list_dir",
			Description: "List entries in a directory with file metadata (size, mod time, is_dir).",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path (default current directory).",
				},
			}, nil),
		},
	}, r.handleListDir)
}

func (r *Registry) handleReadFile(argsJSON string) (string, error) {
	var in readFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	f, err := os.Open(in.Path)
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", in.Path, err)
	}
	defer f.Close()

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var sb strings.Builder
	lineNum := 0
	readCount := 0

	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if readCount >= limit {
			break
		}
		sb.WriteString(fmt.Sprintf("%4d | %s\n", lineNum, scanner.Text()))
		readCount++
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	if readCount == 0 {
		return fmt.Sprintf("(file %s is empty or offset %d exceeds line count)", in.Path, offset), nil
	}

	return sb.String(), nil
}

func (r *Registry) handleWriteFile(argsJSON string) (string, error) {
	var in writeFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	dir := filepath.Dir(in.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	if err := os.WriteFile(in.Path, []byte(in.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file %s: %w", in.Path, err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Content), in.Path), nil
}

func (r *Registry) handleListDir(argsJSON string) (string, error) {
	var in listDirInput
	_ = json.Unmarshal([]byte(argsJSON), &in)

	path := in.Path
	if path == "" {
		path = "."
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("failed to read directory %s: %w", path, err)
	}

	if len(entries) == 0 {
		return fmt.Sprintf("Directory %s is empty", path), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Directory listing for %s (%d entries):\n", path, len(entries)))

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		typeChar := "-"
		if entry.IsDir() {
			typeChar = "d"
		}
		sb.WriteString(fmt.Sprintf("%s %10d  %s  %s\n",
			typeChar,
			info.Size(),
			info.ModTime().Format("2006-01-02 15:04:05"),
			entry.Name(),
		))
	}

	return sb.String(), nil
}
