package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration values for GoAgent.
type Config struct {
	TelegramToken  string
	LLMBaseURL     string
	LLMAPIKey      string
	LLMModel       string
	LLMHeavyModel  string
	LLMEmbedURL    string
	LLMEmbedModel  string
	AdminChatID    int64
	AllowShell     bool
	MaxIterations  int
	MaskIP         bool
	AllowedRoots   string
	AllowEnv       bool
	MeshRole       string // "solo", "leader", "worker"
	MeshListen     string // leader listen address, e.g. ":7801"
	MeshToken      string // shared authentication token
	LeaderAddr     string // worker connection to leader
	WorkerID       string // worker identity
	Capabilities   []string
	TaskTimeout    time.Duration // default 300s
	MissionTimeout time.Duration // default 1800s
}

// Default values
const (
	DefaultLLMBaseURL        = "https://api.openai.com/v1"
	DefaultLLMModel          = "gpt-4o-mini"
	DefaultMaxIterations     = 8
	DefaultEnvFile           = "goagent.env"
	DefaultMeshRole          = "solo"
	DefaultMeshListen        = ":7801"
	DefaultTaskTimeoutSec    = 300
	DefaultMissionTimeoutSec = 1800
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

	meshRole := strings.ToLower(strings.TrimSpace(getVal("GOAGENT_ROLE", DefaultMeshRole)))
	if meshRole == "" {
		meshRole = DefaultMeshRole
	}
	meshListen := getVal("GOAGENT_MESH_LISTEN", DefaultMeshListen)
	meshToken := getVal("GOAGENT_MESH_TOKEN", "")
	leaderAddr := getVal("GOAGENT_LEADER_ADDR", "")

	telegramToken := getVal("TELEGRAM_TOKEN", "")
	if meshRole == "solo" && telegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_TOKEN is required. Please set it in %s or run './deploy.sh install'", filepath.Base(envPath))
	}
	if meshRole == "leader" && meshToken == "" {
		return nil, fmt.Errorf("GOAGENT_MESH_TOKEN is required for leader")
	}
	if meshRole == "worker" {
		if meshToken == "" {
			return nil, fmt.Errorf("GOAGENT_MESH_TOKEN is required for worker")
		}
		if leaderAddr == "" {
			return nil, fmt.Errorf("GOAGENT_LEADER_ADDR is required for worker")
		}
	}

	workerID := getVal("GOAGENT_WORKER_ID", "")
	if workerID == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "worker"
		}
		workerID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	var caps []string
	if capStr := getVal("GOAGENT_CAPABILITIES", ""); capStr != "" {
		for _, part := range strings.Split(capStr, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				caps = append(caps, part)
			}
		}
	}

	taskTimeout := time.Duration(DefaultTaskTimeoutSec) * time.Second
	if ttStr := getVal("GOAGENT_TASK_TIMEOUT", ""); ttStr != "" {
		if s, err := strconv.Atoi(ttStr); err == nil && s > 0 {
			taskTimeout = time.Duration(s) * time.Second
		}
	}

	missionTimeout := time.Duration(DefaultMissionTimeoutSec) * time.Second
	if mtStr := getVal("GOAGENT_MISSION_TIMEOUT", ""); mtStr != "" {
		if s, err := strconv.Atoi(mtStr); err == nil && s > 0 {
			missionTimeout = time.Duration(s) * time.Second
		}
	}

	baseURL := getVal("LLM_BASE_URL", DefaultLLMBaseURL)
	baseURL = strings.TrimRight(baseURL, "/")

	model := getVal("LLM_MODEL", DefaultLLMModel)
	heavyModel := getVal("LLM_HEAVY_MODEL", "")
	embedURL := getVal("LLM_EMBED_URL", "")
	embedModel := getVal("LLM_EMBED_MODEL", "")
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
		TelegramToken:  telegramToken,
		LLMBaseURL:     baseURL,
		LLMAPIKey:      apiKey,
		LLMModel:       model,
		LLMHeavyModel:  heavyModel,
		LLMEmbedURL:    embedURL,
		LLMEmbedModel:  embedModel,
		AdminChatID:    adminChatID,
		AllowShell:     allowShell,
		MaxIterations:  maxIter,
		MaskIP:         maskIP,
		AllowedRoots:   allowedRoots,
		AllowEnv:       allowEnv,
		MeshRole:       meshRole,
		MeshListen:     meshListen,
		MeshToken:      meshToken,
		LeaderAddr:     leaderAddr,
		WorkerID:       workerID,
		Capabilities:   caps,
		TaskTimeout:    taskTimeout,
		MissionTimeout: missionTimeout,
	}, nil
}
