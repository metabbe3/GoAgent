package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoadFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "test.env")

	content := `
# Sample comment
TELEGRAM_TOKEN="test-token-123"
LLM_BASE_URL=https://custom-api.example.com/v1/
LLM_API_KEY='secret-key'
LLM_MODEL=gpt-4o
GOAGENT_ADMIN_CHAT_ID=987654321
GOAGENT_ALLOW_SHELL=true
GOAGENT_MAX_ITER=12
`
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test env: %v", err)
	}

	cfg, err := LoadFromFile(envFile)
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.TelegramToken != "test-token-123" {
		t.Errorf("expected token 'test-token-123', got '%s'", cfg.TelegramToken)
	}
	if cfg.LLMBaseURL != "https://custom-api.example.com/v1" {
		t.Errorf("expected trimmed base URL, got '%s'", cfg.LLMBaseURL)
	}
	if cfg.LLMAPIKey != "secret-key" {
		t.Errorf("expected key 'secret-key', got '%s'", cfg.LLMAPIKey)
	}
	if cfg.LLMModel != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got '%s'", cfg.LLMModel)
	}
	if cfg.AdminChatID != 987654321 {
		t.Errorf("expected admin chat ID 987654321, got %d", cfg.AdminChatID)
	}
	if !cfg.AllowShell {
		t.Errorf("expected AllowShell to be true")
	}
	if cfg.MaxIterations != 12 {
		t.Errorf("expected MaxIterations 12, got %d", cfg.MaxIterations)
	}
	if cfg.SubtaskAttempts != 2 {
		t.Errorf("expected default SubtaskAttempts 2, got %d", cfg.SubtaskAttempts)
	}
	if cfg.FanoutMax != 3 {
		t.Errorf("expected default FanoutMax 3, got %d", cfg.FanoutMax)
	}
}

func TestConfigSubtaskAttemptsAndFanout(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "test.env")
	content := `
TELEGRAM_TOKEN=test-token
GOAGENT_SUBTASK_ATTEMPTS=4
GOAGENT_FANOUT_MAX=5
`
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test env: %v", err)
	}

	cfg, err := LoadFromFile(envFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SubtaskAttempts != 4 {
		t.Errorf("expected SubtaskAttempts 4, got %d", cfg.SubtaskAttempts)
	}
	if cfg.FanoutMax != 5 {
		t.Errorf("expected FanoutMax 5, got %d", cfg.FanoutMax)
	}
}

func TestConfigMissingToken(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "empty.env")
	if err := os.WriteFile(envFile, []byte("LLM_MODEL=test\n"), 0644); err != nil {
		t.Fatalf("failed to write test env: %v", err)
	}

	// Ensure env var is not set
	os.Unsetenv("TELEGRAM_TOKEN")

	_, err := LoadFromFile(envFile)
	if err == nil {
		t.Fatalf("expected error for missing TELEGRAM_TOKEN, got nil")
	}
}

func TestConfigEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "test.env")
	if err := os.WriteFile(envFile, []byte("TELEGRAM_TOKEN=from-file\nLLM_MODEL=from-file\n"), 0644); err != nil {
		t.Fatalf("failed to write test env: %v", err)
	}

	t.Setenv("TELEGRAM_TOKEN", "from-env")
	t.Setenv("LLM_MODEL", "from-env-model")

	cfg, err := LoadFromFile(envFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.TelegramToken != "from-env" {
		t.Errorf("expected 'from-env', got '%s'", cfg.TelegramToken)
	}
	if cfg.LLMModel != "from-env-model" {
		t.Errorf("expected 'from-env-model', got '%s'", cfg.LLMModel)
	}
}
