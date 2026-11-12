package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/guardrail"
	"github.com/metabbe3/GoAgent/internal/llm"
)

type runCommandInput struct {
	Cmd        string `json:"cmd"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

func (r *Registry) registerShellTools() {
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "run_command",
			Description: "Execute a shell command with a timeout. In safe mode (default), only read-only diagnostic commands are allowed.",
			Parameters: schema(map[string]any{
				"cmd": map[string]any{
					"type":        "string",
					"description": "The shell command to run.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds (default 30).",
				},
			}, []string{"cmd"}),
		},
	}, r.handleRunCommand)
}

func (r *Registry) handleRunCommand(ctx context.Context, argsJSON string) (string, error) {
	var in runCommandInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	cmdStr := strings.TrimSpace(in.Cmd)
	if cmdStr == "" {
		return "", fmt.Errorf("command is required")
	}

	r.mu.RLock()
	allowShell := r.allowShell
	r.mu.RUnlock()

	// Enforce command guardrail (blocklist always checked; allowlist checked if !allowShell)
	if err := guardrail.ValidateCommand(cmdStr, allowShell); err != nil {
		return "", err
	}

	timeoutSec := in.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 30
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr)
	out, err := cmd.CombinedOutput()

	if runCtx.Err() == context.DeadlineExceeded || ctx.Err() != nil {
		return "", fmt.Errorf("command timed out after %d seconds", timeoutSec)
	}

	if err != nil {
		return fmt.Sprintf("Command exited with error (%v):\n%s", err, string(out)), nil
	}

	res := string(out)
	if strings.TrimSpace(res) == "" {
		return "(command completed with no output)", nil
	}

	return res, nil
}

// validateAllowlist delegates to the command guardrail for safe mode checks.
func validateAllowlist(cmdStr string) error {
	return guardrail.ValidateCommand(cmdStr, false)
}
