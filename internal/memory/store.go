// Package memory persists conversation history and document summaries to disk.
// Writes are atomic (temp file + rename) and load-time TTL filtering ensures
// stale conversations are not restored after restarts.
package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/metabbe3/GoAgent/internal/llm"
)

// KnowledgeEntry is a summarized document or note to remember.
type KnowledgeEntry struct {
	Time    time.Time `json:"time"`
	File    string    `json:"file"`
	Author  string    `json:"author"`
	Summary string    `json:"summary"`
}

// MaxKnowledgeEntries bounds how many document summaries are returned for prompt injection.
const MaxKnowledgeEntries = 8

// HistoryEntry is one persisted conversation.
type HistoryEntry struct {
	Messages []llm.ChatMessage `json:"messages"`
	LastSeen time.Time         `json:"last_seen"`
}

type historyFile struct {
	Conversations map[string]HistoryEntry `json:"conversations"`
}

// Store manages conversation histories and document memories.
type Store struct {
	dir           string
	historyPath   string
	knowledgePath string
	historyTTL    time.Duration

	hmu     sync.Mutex
	history map[string]HistoryEntry

	kmu       sync.Mutex
	knowledge []KnowledgeEntry
}

// NewStore initializes or loads memory from dir.
func NewStore(dir string) *Store {
	if dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	s := &Store{
		dir:           dir,
		historyPath:   filepath.Join(dir, "history.json"),
		knowledgePath: filepath.Join(dir, "knowledge.jsonl"),
		history:       make(map[string]HistoryEntry),
		knowledge:     make([]KnowledgeEntry, 0),
	}
	if dir != "" {
		s.loadHistory()
		s.loadKnowledge()
	}
	return s
}

// SetHistoryTTL sets the load-time TTL filter.
func (s *Store) SetHistoryTTL(ttl time.Duration) {
	s.historyTTL = ttl
}

// AppendKnowledge records a document summary and maintains an in-memory cap of 200 items.
func (s *Store) AppendKnowledge(e KnowledgeEntry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}

	s.kmu.Lock()
	defer s.kmu.Unlock()

	if s.dir != "" {
		f, err := os.OpenFile(s.knowledgePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}

	s.knowledge = append(s.knowledge, e)
	// Cap in-memory list to newest 200 items to bound prompt memory footprint
	if len(s.knowledge) > 200 {
		s.knowledge = s.knowledge[len(s.knowledge)-200:]
	}
	return nil
}

// Knowledge returns up to MaxKnowledgeEntries newest document summaries.
func (s *Store) Knowledge() []KnowledgeEntry {
	s.kmu.Lock()
	defer s.kmu.Unlock()

	if len(s.knowledge) > MaxKnowledgeEntries {
		return append([]KnowledgeEntry{}, s.knowledge[len(s.knowledge)-MaxKnowledgeEntries:]...)
	}
	return append([]KnowledgeEntry{}, s.knowledge...)
}

// All returns a shallow copy of all persisted conversations.
func (s *Store) All() map[string]HistoryEntry {
	s.hmu.Lock()
	defer s.hmu.Unlock()

	out := make(map[string]HistoryEntry, len(s.history))
	for id, e := range s.history {
		out[id] = e
	}
	return out
}

// GetHistory retrieves the message history for a conversation ID.
func (s *Store) GetHistory(conversationID string) []llm.ChatMessage {
	s.hmu.Lock()
	defer s.hmu.Unlock()

	if e, ok := s.history[conversationID]; ok {
		return e.Messages
	}
	return nil
}

// SaveHistory updates and atomically persists a conversation history.
// If ttl > 0, conversations inactive beyond ttl are pruned.
func (s *Store) SaveHistory(conversationID string, messages []llm.ChatMessage, ttl time.Duration) error {
	s.hmu.Lock()
	s.history[conversationID] = HistoryEntry{
		Messages: messages,
		LastSeen: time.Now(),
	}
	if ttl > 0 {
		for id, e := range s.history {
			if time.Since(e.LastSeen) > ttl {
				delete(s.history, id)
			}
		}
	}
	convs := make(map[string]HistoryEntry, len(s.history))
	for id, e := range s.history {
		convs[id] = e
	}
	s.hmu.Unlock()

	if s.dir == "" {
		return nil
	}
	return s.writeJSON(s.historyPath, historyFile{Conversations: convs})
}

func (s *Store) loadHistory() {
	data, err := os.ReadFile(s.historyPath)
	if err != nil {
		return
	}
	var hf historyFile
	if err := json.Unmarshal(data, &hf); err != nil || hf.Conversations == nil {
		return
	}
	if s.historyTTL > 0 {
		for id, e := range hf.Conversations {
			if time.Since(e.LastSeen) > s.historyTTL {
				delete(hf.Conversations, id)
			}
		}
	}
	s.history = hf.Conversations
}

func (s *Store) loadKnowledge() {
	data, err := os.ReadFile(s.knowledgePath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e KnowledgeEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.File != "" {
			s.knowledge = append(s.knowledge, e)
		}
	}
	if len(s.knowledge) > 200 {
		s.knowledge = s.knowledge[len(s.knowledge)-200:]
	}
}

func (s *Store) writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
