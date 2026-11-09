package guardrail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathGuard(t *testing.T) {
	t.Run("reading /etc/passwd is rejected", func(t *testing.T) {
		_, err := ValidatePath("/etc/passwd")
		if err == nil {
			t.Fatalf("expected /etc/passwd to be rejected outside allowed roots")
		}
		if !strings.Contains(err.Error(), "path outside allowed roots") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("file in /var/log is permitted", func(t *testing.T) {
		resolved, err := ValidatePath("/var/log/system.log")
		if err != nil {
			t.Fatalf("expected /var/log path to be allowed, got error: %v", err)
		}
		if !strings.Contains(resolved, "var/log") {
			t.Errorf("unexpected resolved path: %s", resolved)
		}
	})

	t.Run("symlink pointing outside allowed root is rejected", func(t *testing.T) {
		tmpDir := t.TempDir()
		symlinkPath := filepath.Join(tmpDir, "evil_passwd_symlink")

		// Create symlink pointing to /etc/passwd
		if err := os.Symlink("/etc/passwd", symlinkPath); err != nil {
			t.Skipf("symlink creation not supported: %v", err)
		}

		_, err := ValidatePath(symlinkPath)
		if err == nil {
			t.Fatalf("expected symlink pointing outside root to be rejected")
		}
		if !strings.Contains(err.Error(), "path outside allowed roots") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run(".env file access is blocked by default", func(t *testing.T) {
		tmpDir := t.TempDir()
		envFile := filepath.Join(tmpDir, ".env")
		_ = os.WriteFile(envFile, []byte("SECRET=123"), 0644)

		os.Unsetenv("GOAGENT_ALLOW_ENV")
		_, err := ValidatePath(envFile)
		if err == nil {
			t.Fatalf("expected .env to be blocked by guardrail")
		}
		if !strings.Contains(err.Error(), "access to .env files is blocked") {
			t.Errorf("unexpected error: %v", err)
		}

		// When GOAGENT_ALLOW_ENV=true, .env access is allowed
		os.Setenv("GOAGENT_ALLOW_ENV", "true")
		defer os.Unsetenv("GOAGENT_ALLOW_ENV")

		resolved, err := ValidatePath(envFile)
		if err != nil {
			t.Errorf("expected .env to be allowed when GOAGENT_ALLOW_ENV=true, got: %v", err)
		}
		if resolved == "" {
			t.Errorf("expected non-empty resolved path")
		}
	})
}
