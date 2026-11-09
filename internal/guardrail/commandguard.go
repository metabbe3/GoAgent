package guardrail

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

type blockedRule struct {
	name    string
	pattern *regexp.Regexp
}

var blockedRules = []blockedRule{
	{name: "rm -rf (destructive recursive remove)", pattern: regexp.MustCompile(`\brm\s+(?:-[a-zA-Z]*[rf][a-zA-Z]*\s+|--recursive\s+|--force\s+)*-[a-zA-Z]*[rf]`)},
	{name: "mkfs (filesystem formatting)", pattern: regexp.MustCompile(`\bmkfs(?:\.[a-zA-Z0-9_-]+)?\b`)},
	{name: "dd to block device", pattern: regexp.MustCompile(`\bdd\s+.*?\bof=/dev/`)},
	{name: "sudo (privilege escalation)", pattern: regexp.MustCompile(`\bsudo\b`)},
	{name: "curl pipe to shell", pattern: regexp.MustCompile(`\bcurl\b.*?\|\s*(?:ba|z|k)?sh\b`)},
	{name: "wget pipe to shell", pattern: regexp.MustCompile(`\bwget\b.*?\|\s*(?:ba|z|k)?sh\b`)},
	{name: "write redirection to raw device", pattern: regexp.MustCompile(`>\s*/dev/sd[a-z0-9]`)},
	{name: "chmod 777 on root", pattern: regexp.MustCompile(`\bchmod\s+(?:-[a-zA-Z]*R\s+)?777\s+/`)},
	{name: "system shutdown or reboot", pattern: regexp.MustCompile(`\b(?:shutdown|reboot|halt|poweroff|init\s+0)\b`)},
	{name: "fork bomb", pattern: regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\};:`)},
}

// ValidateCommand checks a shell command against the guardrail blocklist and mode allowlist.
// If allowShell is true, safe commands outside the allowlist are permitted, but dangerous blocklist commands remain blocked.
// If allowShell is false, both the blocklist and safe allowlist are enforced.
func ValidateCommand(cmdStr string, allowShell bool) error {
	trimmed := strings.TrimSpace(cmdStr)
	if trimmed == "" {
		return fmt.Errorf("command is required")
	}

	// 1. Blocklist check (ALWAYS enforced, regardless of allowShell)
	for _, rule := range blockedRules {
		if rule.pattern.MatchString(trimmed) {
			return fmt.Errorf("Command blocked by guardrail: %s", rule.name)
		}
	}

	// 2. If allowShell is enabled, any non-blocked command is allowed
	if allowShell {
		return nil
	}

	// 3. In safe mode (allowShell == false), enforce default allowlist and forbid redirection
	if strings.Contains(trimmed, ">") {
		return fmt.Errorf("file redirection '>' is not permitted in safe mode. Set GOAGENT_ALLOW_SHELL=true to enable write operations")
	}

	// Split by command separators: ;, &&, ||, |, &
	replacer := strings.NewReplacer("&&", ";", "||", ";", "|", ";", "&", ";")
	normalized := replacer.Replace(trimmed)
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
