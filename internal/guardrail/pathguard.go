package guardrail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GetAllowedRoots returns the list of allowed root directories, resolving user home and cwd.
func GetAllowedRoots() []string {
	raw := os.Getenv("GOAGENT_ALLOWED_ROOTS")
	var rootList []string

	if strings.TrimSpace(raw) != "" {
		parts := strings.Split(raw, ":")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				rootList = append(rootList, p)
			}
		}
	} else {
		// Default roots: cwd, /var/log, /tmp, ~/Library/Logs, ~/.goagent, and os.TempDir()
		cwd, err := os.Getwd()
		if err == nil {
			rootList = append(rootList, cwd)
		} else {
			rootList = append(rootList, ".")
		}
		rootList = append(rootList, "/var/log", "/tmp", os.TempDir(), "~/Library/Logs", "~/.goagent")
	}

	home, _ := os.UserHomeDir()

	resolvedRoots := make([]string, 0, len(rootList))
	for _, r := range rootList {
		if strings.HasPrefix(r, "~") && home != "" {
			r = filepath.Join(home, r[1:])
		}
		abs, err := filepath.Abs(r)
		if err == nil {
			// Resolve symlink of the root directory itself if it exists (e.g. /var/log -> /private/var/log on macOS, /tmp -> /private/tmp)
			if eval, err := filepath.EvalSymlinks(abs); err == nil {
				abs = eval
			}
			resolvedRoots = append(resolvedRoots, filepath.Clean(abs))
		}
	}

	return resolvedRoots
}

// ValidatePath validates that a file or directory path is safe:
// 1. Blocks .env* files unless GOAGENT_ALLOW_ENV=true.
// 2. Resolves symlinks and ensures the final target resides inside one of the allowed roots.
func ValidatePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("path cannot be empty")
	}

	// 1. .env* check (case-insensitive base name)
	base := filepath.Base(trimmed)
	if strings.HasPrefix(strings.ToLower(base), ".env") {
		allowEnv := strings.ToLower(os.Getenv("GOAGENT_ALLOW_ENV"))
		if allowEnv != "true" && allowEnv != "1" {
			return "", fmt.Errorf("access to .env files is blocked by guardrail: %s", path)
		}
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("failed to determine absolute path for %s: %w", path, err)
	}

	// Resolve symlinks. If file does not exist yet (e.g., target for writing),
	// resolve the deepest existing parent directory and reconstruct the path.
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Find existing ancestor
			curr := absPath
			var rest []string
			for {
				parent := filepath.Dir(curr)
				rest = append([]string{filepath.Base(curr)}, rest...)
				if parent == curr {
					break
				}
				if evalParent, pErr := filepath.EvalSymlinks(parent); pErr == nil {
					curr = evalParent
					break
				}
				curr = parent
			}
			resolvedPath = filepath.Join(append([]string{curr}, rest...)...)
		} else {
			return "", fmt.Errorf("failed to resolve path symlinks for %s: %w", path, err)
		}
	}

	resolvedPath = filepath.Clean(resolvedPath)

	// Check resolved path again for .env base name
	resolvedBase := filepath.Base(resolvedPath)
	if strings.HasPrefix(strings.ToLower(resolvedBase), ".env") {
		allowEnv := strings.ToLower(os.Getenv("GOAGENT_ALLOW_ENV"))
		if allowEnv != "true" && allowEnv != "1" {
			return "", fmt.Errorf("access to .env files is blocked by guardrail: %s", path)
		}
	}

	// 2. Check against allowed roots
	allowedRoots := GetAllowedRoots()
	insideAllowed := false

	for _, root := range allowedRoots {
		rootClean := filepath.Clean(root)
		if resolvedPath == rootClean || strings.HasPrefix(resolvedPath, rootClean+string(filepath.Separator)) {
			insideAllowed = true
			break
		}
	}

	if !insideAllowed {
		return "", fmt.Errorf("path outside allowed roots: %s (allowed: %s)", path, strings.Join(allowedRoots, ":"))
	}

	return resolvedPath, nil
}
