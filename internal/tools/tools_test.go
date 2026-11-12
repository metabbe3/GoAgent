package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/logskill"
)

func TestReadWriteFileRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sub", "test.txt")

	cfg := &config.Config{AllowShell: false}
	reg := NewRegistry(cfg)

	// Write file
	writeArgs, _ := json.Marshal(writeFileInput{
		Path:    filePath,
		Content: "Line 1\nLine 2\nLine 3\n",
	})
	wRes, err := reg.Execute(context.Background(), "write_file", string(writeArgs))
	if err != nil {
		t.Fatalf("write_file returned error: %v", err)
	}
	if !strings.Contains(wRes, "Successfully wrote") {
		t.Errorf("unexpected write result: %s", wRes)
	}

	// Read file
	readArgs, _ := json.Marshal(readFileInput{
		Path:   filePath,
		Offset: 2,
		Limit:  2,
	})
	rRes, err := reg.Execute(context.Background(), "read_file", string(readArgs))
	if err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	if !strings.Contains(rRes, "Line 2") || !strings.Contains(rRes, "Line 3") {
		t.Errorf("unexpected read content: %s", rRes)
	}
	if strings.Contains(rRes, "Line 1") {
		t.Errorf("read_file did not honor offset: %s", rRes)
	}
}

func TestListDir(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)
	_ = os.Mkdir(filepath.Join(tmpDir, "folder"), 0755)

	reg := NewRegistry(nil)
	args, _ := json.Marshal(listDirInput{Path: tmpDir})
	res, err := reg.Execute(context.Background(), "list_dir", string(args))
	if err != nil {
		t.Fatalf("list_dir failed: %v", err)
	}

	if !strings.Contains(res, "a.txt") || !strings.Contains(res, "folder") {
		t.Errorf("list_dir missing entries: %s", res)
	}
}

func TestRunCommandAllowlist(t *testing.T) {
	reg := NewRegistry(&config.Config{AllowShell: false})

	// Allowed command
	allowedArgs, _ := json.Marshal(runCommandInput{
		Cmd: "uname -s",
	})
	res, err := reg.Execute(context.Background(), "run_command", string(allowedArgs))
	if err != nil {
		t.Fatalf("allowed command returned error: %v", err)
	}
	if strings.Contains(res, "is not in the safe allowlist") {
		t.Errorf("uname should be allowed, got: %s", res)
	}

	// Disallowed command blocked by guardrail
	disallowedArgs, _ := json.Marshal(runCommandInput{
		Cmd: "rm -rf /tmp/some_file",
	})
	res, err = reg.Execute(context.Background(), "run_command", string(disallowedArgs))
	if err != nil {
		t.Fatalf("Execute should return formatted error message, got: %v", err)
	}
	if !strings.Contains(res, "Command blocked by guardrail") {
		t.Errorf("expected guardrail error message, got: %s", res)
	}

	// Disallowed command outside allowlist
	nonAllowlistArgs, _ := json.Marshal(runCommandInput{
		Cmd: "git status",
	})
	res, err = reg.Execute(context.Background(), "run_command", string(nonAllowlistArgs))
	if err != nil {
		t.Fatalf("Execute should return formatted error message, got: %v", err)
	}
	if !strings.Contains(res, "is not in the safe allowlist") {
		t.Errorf("expected allowlist error message, got: %s", res)
	}

	// Redirection in safe mode
	redirectArgs, _ := json.Marshal(runCommandInput{
		Cmd: "echo 'bad' > /tmp/bad.txt",
	})
	res, err = reg.Execute(context.Background(), "run_command", string(redirectArgs))
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	if !strings.Contains(res, "redirection '>' is not permitted") {
		t.Errorf("expected redirection error, got: %s", res)
	}
}

func TestRunCommandTimeout(t *testing.T) {
	reg := NewRegistry(&config.Config{AllowShell: false})

	// sleep is in allowlist, test timeout with sleep 2 and timeout 1
	timeoutArgs, _ := json.Marshal(runCommandInput{
		Cmd:        "sleep 2",
		TimeoutSec: 1,
	})

	res, err := reg.Execute(context.Background(), "run_command", string(timeoutArgs))
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	if !strings.Contains(res, "timed out after 1 seconds") {
		t.Errorf("expected timeout message, got: %s", res)
	}
}

func TestReadLogAndLogAnalysis(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "server.log")

	content := strings.Join([]string{
		"2024-03-29 10:00:00 [INFO] started",
		"2024-03-29 10:00:01 [ERROR] connection lost",
		"2024-03-29 10:00:02 [INFO] ping ok",
	}, "\n") + "\n"

	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write log: %v", err)
	}

	reg := NewRegistry(nil)
	reg.SetStoreDir(filepath.Join(tmpDir, "patterns"))

	// Test read_log
	readLogArgs, _ := json.Marshal(readLogInput{
		Path:        logFile,
		Tail:        2,
		FilterRegex: "ERROR|ping",
	})
	rlRes, err := reg.Execute(context.Background(), "read_log", string(readLogArgs))
	if err != nil {
		t.Fatalf("read_log failed: %v", err)
	}
	if !strings.Contains(rlRes, "connection lost") || !strings.Contains(rlRes, "ping ok") {
		t.Errorf("unexpected read_log output: %s", rlRes)
	}

	// Test analyze_logs
	analyzeArgs, _ := json.Marshal(analyzeLogsInput{
		Path: logFile,
		Top:  5,
	})
	anRes, err := reg.Execute(context.Background(), "analyze_logs", string(analyzeArgs))
	if err != nil {
		t.Fatalf("analyze_logs failed: %v", err)
	}

	var resObj logskill.AnalysisResult
	if err := json.Unmarshal([]byte(anRes), &resObj); err != nil {
		t.Fatalf("failed to unmarshal analyze_logs output JSON: %v\nOutput: %s", err, anRes)
	}
	if resObj.TotalLines != 3 {
		t.Errorf("expected 3 total lines, got %d", resObj.TotalLines)
	}

	// Test learned_patterns
	learnRes, err := reg.Execute(context.Background(), "learned_patterns", "{}")
	if err != nil {
		t.Fatalf("learned_patterns failed: %v", err)
	}
	if !strings.Contains(learnRes, "server.log") {
		t.Errorf("learned patterns missing server.log entry: %s", learnRes)
	}

	// Test log_timeline
	timelineArgs, _ := json.Marshal(logTimelineInput{
		Path:         logFile,
		PatternRegex: "connection lost",
		Context:      1,
	})
	tlRes, err := reg.Execute(context.Background(), "log_timeline", string(timelineArgs))
	if err != nil {
		t.Fatalf("log_timeline failed: %v", err)
	}
	if !strings.Contains(tlRes, "Timeline around line 2") {
		t.Errorf("expected timeline around line 2, got: %s", tlRes)
	}
	if !strings.Contains(tlRes, "connection lost") {
		t.Errorf("expected target line in timeline output, got: %s", tlRes)
	}
}

func TestReadDocument(t *testing.T) {
	tmpDir := t.TempDir()
	docFile := filepath.Join(tmpDir, "sample.md")
	_ = os.WriteFile(docFile, []byte("# Overview\nSystem diagnostics healthy"), 0o644)

	reg := NewRegistry(nil)
	args, _ := json.Marshal(map[string]string{"path": docFile})
	res, err := reg.Execute(context.Background(), "read_document", string(args))
	if err != nil {
		t.Fatalf("read_document failed: %v", err)
	}
	if !strings.Contains(res, "[document content from file sample.md") {
		t.Errorf("expected framed document header, got %s", res)
	}
	if !strings.Contains(res, "System diagnostics healthy") {
		t.Errorf("expected content in response, got %s", res)
	}
}

func TestGeneratePDF(t *testing.T) {
	tmpDir := t.TempDir()
	reg := NewRegistry(nil)
	reg.registerGenerateTools(tmpDir)

	args, _ := json.Marshal(map[string]string{
		"title":    "Diagnostic Report",
		"markdown": "# Header\n- Finding 1\n- Finding 2\n\nAll services normal.",
	})
	res, err := reg.Execute(context.Background(), "generate_pdf", string(args))
	if err != nil {
		t.Fatalf("generate_pdf failed: %v", err)
	}
	if !strings.Contains(res, "[generated-file:") {
		t.Errorf("missing generated file marker: %s", res)
	}

	// Verify CJK rejection
	cjkArgs, _ := json.Marshal(map[string]string{
		"title":    "Report",
		"markdown": "测试中文",
	})
	cjkRes, _ := reg.Execute(context.Background(), "generate_pdf", string(cjkArgs))
	if !strings.Contains(cjkRes, "Chinese/Japanese/Korean text") {
		t.Errorf("expected CJK rejection error, got: %s", cjkRes)
	}
}

func TestFetchURLSSRFBlocked(t *testing.T) {
	reg := NewRegistry(nil)
	args, _ := json.Marshal(map[string]string{"url": "http://127.0.0.1:9999/admin"})
	res, err := reg.Execute(context.Background(), "fetch_url", string(args))
	if err != nil {
		t.Fatalf("unexpected handler err: %v", err)
	}
	if !strings.Contains(res, "SSRF guard") && !strings.Contains(res, "not a public IP") {
		t.Errorf("expected SSRF block message, got: %s", res)
	}
}
