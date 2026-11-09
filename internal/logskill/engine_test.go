package logskill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizationTable(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Date and Time",
			input:    "2024-03-29 14:22:10 INFO server started",
			expected: "{DATE} {TIME} INFO server started",
		},
		{
			name:     "ISO timestamp",
			input:    "2024-03-29T14:22:10.500Z ERROR database down",
			expected: "{DATE} ERROR database down",
		},
		{
			name:     "IPv4 address",
			input:    "Connection from 192.168.1.100 accepted",
			expected: "Connection from {IP} accepted",
		},
		{
			name:     "UUID",
			input:    "Request id 123e4567-e89b-12d3-a456-426614174000 completed",
			expected: "Request id {UUID} completed",
		},
		{
			name:     "Long Hex",
			input:    "Commit hash 7b4f3a1c89de or token 0xdeadbeef",
			expected: "Commit hash {HEX} or token {HEX}",
		},
		{
			name:     "Numbers (formatted, float, int, durations)",
			input:    "Processed 1.234,56 items in 12ms (average 3.2s per 50 units)",
			expected: "Processed {N} items in {N} (average {N} per {N} units)",
		},
		{
			name:     "Quoted string length > 4",
			input:    `User message: "connection timeout error" status="ok"`,
			expected: `User message: {STR} status="ok"`,
		},
		{
			name:     "Path with line number",
			input:    "Panic at /foo/bar.js:123 in handler",
			expected: "Panic at {PATH} in handler",
		},
		{
			name:     "Complex log line",
			input:    `2026-03-29 10:15:30 [ERROR] Client 10.0.0.1 failed request 550e8400-e29b-41d4-a716-446655440000 in 150ms: "bad gateway error" at /usr/local/api/handler.go:88`,
			expected: `{DATE} {TIME} [ERROR] Client {IP} failed request {UUID} in {N}: {STR} at {PATH}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeLine(tc.input)
			if got != tc.expected {
				t.Errorf("\ninput:    %s\nexpected: %s\ngot:      %s", tc.input, tc.expected, got)
			}
		})
	}
}

func TestLevelDetection(t *testing.T) {
	tests := []struct {
		line     string
		expected string
	}{
		{"FATAL: system crash", "FATAL"},
		{"panic: out of memory", "FATAL"},
		{"2024-01-01 [ERROR] connection failed", "ERROR"},
		{"level=error msg=failed", "ERROR"},
		{`{"level":"warn","msg":"high cpu"}`, "WARN"},
		{"[WARNING] disk space low", "WARN"},
		{"2024-01-01 12:00:00 INFO starting service", "INFO"},
		{"DEBUG: cache hit key=foo", "DEBUG"},
		{"TRACE: entered function foo", "DEBUG"},
		{"Just a normal text line", "OTHER"},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			lvl := DetectLevel(tc.line)
			if lvl != tc.expected {
				t.Errorf("line: %s, expected: %s, got: %s", tc.line, tc.expected, lvl)
			}
		})
	}
}

func TestChunkSplit100kSyntheticLines(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "synthetic_100k.log")

	// Create 100,000 synthetic log lines
	// Distribute between 4 patterns:
	// Pattern 1: INFO user login
	// Pattern 2: WARN slow query
	// Pattern 3: ERROR timeout
	// Pattern 4: DEBUG trace
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("failed to create log file: %v", err)
	}

	totalExpectedLines := 100000
	buf := make([]byte, 0, 1024*1024)

	for i := 0; i < totalExpectedLines; i++ {
		var line string
		switch i % 4 {
		case 0:
			line = fmt.Sprintf("2024-03-29 10:%02d:%02d [INFO] User %d logged in from 192.168.1.%d in %dms\n", (i/60)%60, i%60, i, i%255, (i%50)+1)
		case 1:
			line = fmt.Sprintf("2024-03-29 11:%02d:%02d [WARN] Slow query took %dms at /app/db/query.go:%d\n", (i/60)%60, i%60, 500+(i%100), i%500)
		case 2:
			line = fmt.Sprintf("2024-03-29 12:%02d:%02d [ERROR] Connection timeout to 10.0.0.%d error=\"connection lost\"\n", (i/60)%60, i%60, i%255)
		case 3:
			line = fmt.Sprintf("2024-03-29 13:%02d:%02d [DEBUG] Trace packet 0x%08x verified\n", (i/60)%60, i%60, i)
		}
		buf = append(buf, line...)
		if len(buf) >= 512*1024 {
			if _, err := f.Write(buf); err != nil {
				t.Fatalf("failed to write buffer: %v", err)
			}
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		if _, err := f.Write(buf); err != nil {
			t.Fatalf("failed to flush buffer: %v", err)
		}
	}
	f.Close()

	// Run AnalyzeFile with 8 chunks forced to test parallel worker splitting
	res, err := AnalyzeFile(logPath, AnalysisOptions{
		Top:       10,
		MinCount:  1,
		StoreDir:  filepath.Join(tmpDir, "patterns"),
		NumChunks: 8,
	})
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	// Verify total lines == 100,000
	if res.TotalLines != int64(totalExpectedLines) {
		t.Errorf("expected total_lines %d, got %d", totalExpectedLines, res.TotalLines)
	}

	// Verify sum of counts of all patterns == 100,000
	var sumPatternCounts int64
	for _, p := range res.TopPatterns {
		sumPatternCounts += p.Count
	}
	// Note: there are only 4 distinct patterns, so Top 10 includes all patterns
	if sumPatternCounts != int64(totalExpectedLines) {
		t.Errorf("expected sum of pattern counts %d, got %d", totalExpectedLines, sumPatternCounts)
	}

	// Verify levels count
	if res.Levels["INFO"] != 25000 {
		t.Errorf("expected 25000 INFO lines, got %d", res.Levels["INFO"])
	}
	if res.Levels["WARN"] != 25000 {
		t.Errorf("expected 25000 WARN lines, got %d", res.Levels["WARN"])
	}
	if res.Levels["ERROR"] != 25000 {
		t.Errorf("expected 25000 ERROR lines, got %d", res.Levels["ERROR"])
	}
	if res.Levels["DEBUG"] != 25000 {
		t.Errorf("expected 25000 DEBUG lines, got %d", res.Levels["DEBUG"])
	}
}

func TestAnomaliesMinCount(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "anomalies.log")

	content := strings.Join([]string{
		"2024-03-29 10:00:00 [INFO] normal ping",
		"2024-03-29 10:00:01 [INFO] normal ping",
		"2024-03-29 10:00:02 [INFO] normal ping",
		"2024-03-29 10:00:03 [ERROR] rare critical corruption",
	}, "\n") + "\n"

	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write log: %v", err)
	}

	res, err := AnalyzeFile(logPath, AnalysisOptions{
		Top:       10,
		MinCount:  1,
		StoreDir:  filepath.Join(tmpDir, "patterns"),
		NumChunks: 1,
	})
	if err != nil {
		t.Fatalf("AnalyzeFile failed: %v", err)
	}

	if res.TotalLines != 4 {
		t.Fatalf("expected 4 lines, got %d", res.TotalLines)
	}

	// Rare line should be in anomalies because count == 1 <= min_count (1)
	foundRare := false
	for _, a := range res.Anomalies {
		if strings.Contains(a.Pattern, "rare critical corruption") {
			foundRare = true
			if a.Count != 1 {
				t.Errorf("expected count 1 for rare pattern, got %d", a.Count)
			}
		}
	}
	if !foundRare {
		t.Errorf("expected rare pattern to be flagged as anomaly")
	}
}
