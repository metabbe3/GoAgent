package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/metabbe3/GoAgent/internal/guardrail"
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
	Path            string `json:"path"`
	Top             int    `json:"top,omitempty"`
	FilterRegex     string `json:"filter_regex,omitempty"`
	MinCount        int    `json:"min_count,omitempty"`
	CompareBaseline bool   `json:"compare_baseline,omitempty"`
	Window          string `json:"window,omitempty"`
}

type learnedPatternsInput struct {
	File string `json:"file,omitempty"`
}

type logTimelineInput struct {
	Path         string `json:"path"`
	PatternRegex string `json:"pattern_regex,omitempty"`
	AroundLine   int    `json:"around_line,omitempty"`
	Context      int    `json:"context,omitempty"`
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
			Description: "Perform chunk-parallel log analysis, clustering templates, counting severity levels, detecting bursts, correlations, and anomalies with self-learning.",
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
				"compare_baseline": map[string]any{
					"type":        "boolean",
					"description": "Compare current run with 30-day historical baseline to flag frequency spikes (>2x) or drops (<0.5x).",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Optional time window before the latest timestamp in the file (e.g. '1h', '24h', or empty for full file).",
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

	// log_timeline
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "log_timeline",
			Description: "Inspect context lines around a specific line number or first regex match in a log file (max 50 lines total).",
			Parameters: schema(map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the log file.",
				},
				"pattern_regex": map[string]any{
					"type":        "string",
					"description": "Regex pattern to locate the center of the timeline window.",
				},
				"around_line": map[string]any{
					"type":        "integer",
					"description": "1-based line number to center the context around (used if pattern_regex is omitted).",
				},
				"context": map[string]any{
					"type":        "integer",
					"description": "Number of lines before and after to display (default 5, max 25).",
				},
			}, []string{"path"}),
		},
	}, r.handleLogTimeline)
}

func (r *Registry) handleReadLog(_ context.Context, argsJSON string) (string, error) {
	var in readLogInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	validPath, err := guardrail.ValidatePath(in.Path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(validPath)
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

func (r *Registry) handleAnalyzeLogs(_ context.Context, argsJSON string) (string, error) {
	var in analyzeLogsInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	validPath, err := guardrail.ValidatePath(in.Path)
	if err != nil {
		return "", err
	}

	r.mu.RLock()
	storeDir := r.storeDir
	r.mu.RUnlock()

	opts := logskill.AnalysisOptions{
		Top:             in.Top,
		FilterRegex:     in.FilterRegex,
		MinCount:        in.MinCount,
		StoreDir:        storeDir,
		CompareBaseline: in.CompareBaseline,
		Window:          in.Window,
	}

	res, err := logskill.AnalyzeFile(validPath, opts)
	if err != nil {
		return "", fmt.Errorf("log analysis error: %w", err)
	}

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format analysis json: %w", err)
	}

	return string(data), nil
}

func (r *Registry) handleLearnedPatterns(_ context.Context, argsJSON string) (string, error) {
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

func (r *Registry) handleLogTimeline(_ context.Context, argsJSON string) (string, error) {
	var in logTimelineInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	validPath, err := guardrail.ValidatePath(in.Path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(validPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", in.Path, err)
	}
	defer f.Close()

	ctxCount := in.Context
	if ctxCount <= 0 {
		ctxCount = 5
	}
	if ctxCount > 25 {
		ctxCount = 25
	}

	// First pass or locate target line
	var targetLine int
	var filterRe *regexp.Regexp
	if in.PatternRegex != "" {
		compiled, err := regexp.Compile(in.PatternRegex)
		if err != nil {
			return "", fmt.Errorf("invalid pattern regex '%s': %w", in.PatternRegex, err)
		}
		filterRe = compiled
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var allLines []string
	lineNum := 0
	foundMatch := false

	for scanner.Scan() {
		lineNum++
		text := scanner.Text()
		allLines = append(allLines, text)

		if !foundMatch && filterRe != nil && filterRe.MatchString(text) {
			targetLine = lineNum
			foundMatch = true
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading log file: %w", err)
	}

	if filterRe != nil && !foundMatch {
		return fmt.Sprintf("pattern '%s' not found in %s", in.PatternRegex, in.Path), nil
	}

	if targetLine == 0 {
		if in.AroundLine > 0 {
			targetLine = in.AroundLine
		} else {
			targetLine = 1
		}
	}

	start := targetLine - ctxCount
	if start < 1 {
		start = 1
	}
	end := targetLine + ctxCount
	if end > len(allLines) {
		end = len(allLines)
	}

	// Cap total output lines to 50
	if end-start+1 > 50 {
		end = start + 49
		if end > len(allLines) {
			end = len(allLines)
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Timeline around line %d for %s (lines %d-%d of %d):\n",
		targetLine, in.Path, start, end, len(allLines)))

	for i := start; i <= end; i++ {
		prefix := "  "
		if i == targetLine {
			prefix = "> "
		}
		sb.WriteString(fmt.Sprintf("%s%4d | %s\n", prefix, i, allLines[i-1]))
	}

	redacted, _ := guardrail.Redact(sb.String())
	return redacted, nil
}
