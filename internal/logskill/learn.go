package logskill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PatternRecord stores historical knowledge about a learned log pattern.
type PatternRecord struct {
	Count      int64     `json:"count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Sample     string    `json:"sample"`
	LevelGuess string    `json:"level_guess"`
}

// LearnedSummary is a concise summary of a learned pattern for LLM / tools.
type LearnedSummary struct {
	Pattern    string    `json:"pattern"`
	Count      int64     `json:"count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	LevelGuess string    `json:"level_guess"`
	Sample     string    `json:"sample"`
	SourceFile string    `json:"source_file,omitempty"`
}

var storeMutex sync.Mutex

// DefaultStoreDir returns ~/.goagent/patterns directory.
func DefaultStoreDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("unable to determine user home directory: %w", err)
	}
	dir := filepath.Join(home, ".goagent", "patterns")
	return dir, nil
}

// SanitizeLogPath converts a file path into a safe filename for pattern storage.
func SanitizeLogPath(logPath string) string {
	clean := filepath.Clean(logPath)
	var b strings.Builder
	for _, r := range clean {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	sanitized := strings.Trim(b.String(), "_")
	if sanitized == "" {
		sanitized = "general"
	}
	return sanitized + ".json"
}

// GetStorePath determines the JSON file path for saving patterns.
func GetStorePath(logPath, customDir string) (string, error) {
	dir := customDir
	if dir == "" {
		var err error
		dir, err = DefaultStoreDir()
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create store dir %s: %w", dir, err)
	}
	return filepath.Join(dir, SanitizeLogPath(logPath)), nil
}

// LoadStore reads learned patterns from the given JSON file.
func LoadStore(storePath string) (map[string]PatternRecord, error) {
	storeMutex.Lock()
	defer storeMutex.Unlock()

	return loadStoreUnlocked(storePath)
}

func loadStoreUnlocked(storePath string) (map[string]PatternRecord, error) {
	store := make(map[string]PatternRecord)
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		return store, nil
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read pattern store %s: %w", storePath, err)
	}
	if len(data) == 0 {
		return store, nil
	}

	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pattern store: %w", err)
	}
	return store, nil
}

// SaveStore atomically writes the pattern store to disk.
func SaveStore(storePath string, store map[string]PatternRecord) error {
	storeMutex.Lock()
	defer storeMutex.Unlock()

	return saveStoreUnlocked(storePath, store)
}

func saveStoreUnlocked(storePath string, store map[string]PatternRecord) error {
	dir := filepath.Dir(storePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal pattern store: %w", err)
	}

	tmpFile := storePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write tmp pattern file %s: %w", tmpFile, err)
	}

	if err := os.Rename(tmpFile, storePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to commit pattern store %s: %w", storePath, err)
	}

	return nil
}

// UpdateLearnedPatterns loads the store, merges current patterns, flags new ones,
// and atomically updates the store file.
func UpdateLearnedPatterns(storePath string, currentPatterns map[string]*PatternData, now time.Time) (map[string]bool, error) {
	storeMutex.Lock()
	defer storeMutex.Unlock()

	store, err := loadStoreUnlocked(storePath)
	if err != nil {
		return nil, err
	}

	isNew := make(map[string]bool)

	for pat, data := range currentPatterns {
		if rec, exists := store[pat]; exists {
			rec.Count += data.Count
			rec.LastSeen = now
			if data.Sample != "" {
				rec.Sample = data.Sample
			}
			if data.Level != "" && data.Level != "OTHER" {
				rec.LevelGuess = data.Level
			}
			store[pat] = rec
			isNew[pat] = false
		} else {
			store[pat] = PatternRecord{
				Count:      data.Count,
				FirstSeen:  now,
				LastSeen:   now,
				Sample:     data.Sample,
				LevelGuess: data.Level,
			}
			isNew[pat] = true
		}
	}

	if err := saveStoreUnlocked(storePath, store); err != nil {
		return nil, err
	}

	return isNew, nil
}

// ListLearnedPatterns returns learned pattern summaries across stores or for a specific file.
func ListLearnedPatterns(storeDir, fileFilter string) ([]LearnedSummary, error) {
	dir := storeDir
	if dir == "" {
		var err error
		dir, err = DefaultStoreDir()
		if err != nil {
			return nil, err
		}
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return []LearnedSummary{}, nil
	}

	var results []LearnedSummary
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read store directory %s: %w", dir, err)
	}

	var targetFile string
	if fileFilter != "" {
		targetFile = SanitizeLogPath(fileFilter)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if targetFile != "" && entry.Name() != targetFile && !strings.Contains(entry.Name(), strings.TrimSuffix(targetFile, ".json")) {
			continue
		}

		fullPath := filepath.Join(dir, entry.Name())
		store, err := LoadStore(fullPath)
		if err != nil {
			continue
		}

		for pat, rec := range store {
			results = append(results, LearnedSummary{
				Pattern:    pat,
				Count:      rec.Count,
				FirstSeen:  rec.FirstSeen,
				LastSeen:   rec.LastSeen,
				LevelGuess: rec.LevelGuess,
				Sample:     rec.Sample,
				SourceFile: entry.Name(),
			})
		}
	}

	return results, nil
}
