package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all configuration values for GoAgent.
type Config struct {
	TelegramToken string
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	AdminChatID   int64
	AllowShell    bool
	MaxIterations int
	MaskIP        bool
	AllowedRoots  string
	AllowEnv      bool
}

// Default values
const (
	DefaultLLMBaseURL    = "https://api.openai.com/v1"
	DefaultLLMModel      = "gpt-4o-mini"
	DefaultMaxIterations = 8
	DefaultEnvFile       = "goagent.env"
)

// Load reads configuration from the default env file (if present)
// and overrides it with current process environment variables.
func Load() (*Config, error) {
	return LoadFromFile(DefaultEnvFile)
}

// LoadFromFile reads configuration from a specific env file path.
func LoadFromFile(envPath string) (*Config, error) {
	envMap := make(map[string]string)

	// If file exists, load KEY=VALUE pairs
	if _, err := os.Stat(envPath); err == nil {
		file, err := os.Open(envPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open env file %s: %w", envPath, err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			idx := strings.Index(line, "=")
			if idx <= 0 {
				continue
			}

			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])

			// Remove quotes if present
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			envMap[key] = val
		}

		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("error reading env file %s: %w", envPath, err)
		}
	}

	// Helper to get from OS env first, fallback to envMap, fallback to default
	getVal := func(key, defVal string) string {
		if osVal := os.Getenv(key); osVal != "" {
			return osVal
		}
		if mapVal, ok := envMap[key]; ok && mapVal != "" {
			return mapVal
		}
		return defVal
	}

	telegramToken := getVal("TELEGRAM_TOKEN", "")
	if telegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_TOKEN is required. Please set it in %s or run './deploy.sh install'", filepath.Base(envPath))
	}

	baseURL := getVal("LLM_BASE_URL", DefaultLLMBaseURL)
	baseURL = strings.TrimRight(baseURL, "/")

	model := getVal("LLM_MODEL", DefaultLLMModel)
	apiKey := getVal("LLM_API_KEY", "")

	var adminChatID int64
	if adminStr := getVal("GOAGENT_ADMIN_CHAT_ID", ""); adminStr != "" {
		parsed, err := strconv.ParseInt(adminStr, 10, 64)
		if err == nil {
			adminChatID = parsed
		}
	}

	allowShell := false
	if shellStr := getVal("GOAGENT_ALLOW_SHELL", "false"); strings.ToLower(shellStr) == "true" || shellStr == "1" {
		allowShell = true
	}

	maxIter := DefaultMaxIterations
	if iterStr := getVal("GOAGENT_MAX_ITER", ""); iterStr != "" {
		if parsed, err := strconv.Atoi(iterStr); err == nil && parsed > 0 {
			maxIter = parsed
		}
	}

	maskIP := false
	if maskStr := getVal("GOAGENT_MASK_IP", "false"); strings.ToLower(maskStr) == "true" || maskStr == "1" {
		maskIP = true
		if os.Getenv("GOAGENT_MASK_IP") == "" {
			_ = os.Setenv("GOAGENT_MASK_IP", "true")
		}
	}

	allowedRoots := getVal("GOAGENT_ALLOWED_ROOTS", "")
	if allowedRoots != "" && os.Getenv("GOAGENT_ALLOWED_ROOTS") == "" {
		_ = os.Setenv("GOAGENT_ALLOWED_ROOTS", allowedRoots)
	}

	allowEnv := false
	if allowEnvStr := getVal("GOAGENT_ALLOW_ENV", "false"); strings.ToLower(allowEnvStr) == "true" || allowEnvStr == "1" {
		allowEnv = true
		if os.Getenv("GOAGENT_ALLOW_ENV") == "" {
			_ = os.Setenv("GOAGENT_ALLOW_ENV", "true")
		}
	}

	return &Config{
		TelegramToken: telegramToken,
		LLMBaseURL:    baseURL,
		LLMAPIKey:     apiKey,
		LLMModel:      model,
		AdminChatID:   adminChatID,
		AllowShell:    allowShell,
		MaxIterations: maxIter,
		MaskIP:        maskIP,
		AllowedRoots:  allowedRoots,
		AllowEnv:      allowEnv,
	}, nil
}
