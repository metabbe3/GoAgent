package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/logskill"
)

type readLogInput struct {
	Path        string `json:"path"`
	Tail        int    `json:"tail,omitempty"`
	FilterRegex string `json:"filter_regex,omitempty"`
	Follow      bool   `json:"follow,omitempty"` // Ignored
}

type analyzeLogsInput struct {
	Path        string `json:"path"`
	Top         int    `json:"top,omitempty"`
	FilterRegex string `json:"filter_regex,omitempty"`
	MinCount    int    `json:"min_count,omitempty"`
}

type learnedPatternsInput struct {
	File string `json:"file,omitempty"`
}

func (r *Registry) registerLogsTools() {
	// read_log
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "read_log",
			Description: "Read the most recent lines of a log file with optional regex filtering.",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the log file.",
				},
				"tail": map[string]any{
					"type":        "integer",
					"description": "Number of recent lines to retrieve (default 100).",
				},
				"filter_regex": map[string]any{
					"type":        "string",
					"description": "Optional regular expression to filter log lines.",
				},
				"follow": map[string]any{
					"type":        "boolean",
					"description": "Ignored in v0.1 (non-streaming).",
				},
			}, []string{"path"}),
		},
	}, r.handleReadLog)

	// analyze_logs
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "analyze_logs",
			Description: "Perform chunk-parallel log analysis, clustering templates, counting severity levels, and detecting anomalies/new patterns with self-learning.",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the log file to analyze.",
				},
				"top": map[string]any{
					"type":        "integer",
					"description": "Number of top frequent patterns to return (default 10).",
				},
				"filter_regex": map[string]any{
					"type":        "string",
					"description": "Optional regex to filter lines before clustering.",
				},
				"min_count": map[string]any{
					"type":        "integer",
					"description": "Threshold count below which a pattern is considered an anomaly (default 1).",
				},
			}, []string{"path"}),
		},
	}, r.handleAnalyzeLogs)

	// learned_patterns
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "learned_patterns",
			Description: "List self-learned log patterns and occurrence history across previous analysis sessions.",
			Parameters: schema(map[string]any{
				"file": map[string]any{
					"type":        "string",
					"description": "Optional file name or filter to restrict pattern history.",
				},
			}, nil),
		},
	}, r.handleLearnedPatterns)
}

func (r *Registry) handleReadLog(argsJSON string) (string, error) {
	var in readLogInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	f, err := os.Open(in.Path)
	if err != nil {
		return "", fmt.Errorf("failed to open log file %s: %w", in.Path, err)
	}
	defer f.Close()

	var filterRe *regexp.Regexp
	if in.FilterRegex != "" {
		compiled, err := regexp.Compile(in.FilterRegex)
		if err != nil {
			return "", fmt.Errorf("invalid filter regex '%s': %w", in.FilterRegex, err)
		}
		filterRe = compiled
	}

	tail := in.Tail
	if tail <= 0 {
		tail = 100
	}
	if tail > 2000 {
		tail = 2000
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// Ring buffer for tail
	ring := make([]string, tail)
	ringPos := 0
	count := 0

	for scanner.Scan() {
		line := scanner.Text()
		if filterRe != nil && !filterRe.MatchString(line) {
			continue
		}
		ring[ringPos] = line
		ringPos = (ringPos + 1) % tail
		count++
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading log file: %w", err)
	}

	if count == 0 {
		return fmt.Sprintf("(no matching lines in %s)", in.Path), nil
	}

	var sb strings.Builder
	numToOutput := count
	if numToOutput > tail {
		numToOutput = tail
	}

	start := 0
	if count > tail {
		start = ringPos
	}

	sb.WriteString(fmt.Sprintf("Showing %d lines from %s:\n", numToOutput, in.Path))
	for i := 0; i < numToOutput; i++ {
		idx := (start + i) % tail
		sb.WriteString(ring[idx])
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

func (r *Registry) handleAnalyzeLogs(argsJSON string) (string, error) {
	var in analyzeLogsInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	r.mu.RLock()
	storeDir := r.storeDir
	r.mu.RUnlock()

	opts := logskill.AnalysisOptions{
		Top:         in.Top,
		FilterRegex: in.FilterRegex,
		MinCount:    in.MinCount,
		StoreDir:    storeDir,
	}

	res, err := logskill.AnalyzeFile(in.Path, opts)
	if err != nil {
		return "", fmt.Errorf("log analysis error: %w", err)
	}

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format analysis json: %w", err)
	}

	return string(data), nil
}

func (r *Registry) handleLearnedPatterns(argsJSON string) (string, error) {
	var in learnedPatternsInput
	_ = json.Unmarshal([]byte(argsJSON), &in)

	r.mu.RLock()
	storeDir := r.storeDir
	r.mu.RUnlock()

	summaries, err := logskill.ListLearnedPatterns(storeDir, in.File)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve learned patterns: %w", err)
	}

	if len(summaries) == 0 {
		return "No learned patterns found yet. Analyze log files first to build pattern memory.", nil
	}

	data, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to encode learned patterns: %w", err)
	}

	return string(data), nil
}
