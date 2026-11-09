package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/llm"
)

var defaultAllowlist = map[string]bool{
	"ls":    true,
	"cat":   true,
	"tail":  true,
	"head":  true,
	"grep":  true,
	"wc":    true,
	"du":    true,
	"df":    true,
	"ps":    true,
	"find":  true,
	"stat":  true,
	"file":  true,
	"date":  true,
	"uname": true,
	"sleep": true,
	"echo":  true,
}

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

func (r *Registry) handleRunCommand(argsJSON string) (string, error) {
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

	// If unrestricted shell is not enabled, enforce safe allowlist
	if !allowShell {
		if err := validateAllowlist(cmdStr); err != nil {
			return "", err
		}
	}

	timeoutSec := in.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
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

// validateAllowlist ensures all sub-commands in pipelines/chains belong to the allowlist.
func validateAllowlist(cmdStr string) error {
	// Disallow write redirection in safe mode
	if strings.Contains(cmdStr, ">") {
		return fmt.Errorf("file redirection '>' is not permitted in safe mode. Set GOAGENT_ALLOW_SHELL=true to enable write operations")
	}

	// Split by delimiters: ;, &&, ||, |, &
	replacer := strings.NewReplacer("&&", ";", "||", ";", "|", ";", "&", ";")
	normalized := replacer.Replace(cmdStr)
	segments := strings.Split(normalized, ";")

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}

		rawBinary := fields[0]
		binary := filepath.Base(rawBinary)

		if !defaultAllowlist[binary] {
			keys := make([]string, 0, len(defaultAllowlist))
			for k := range defaultAllowlist {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return fmt.Errorf("command '%s' is not in the safe allowlist (%s). Set GOAGENT_ALLOW_SHELL=true to allow all commands", binary, strings.Join(keys, ", "))
		}
	}

	return nil
}
