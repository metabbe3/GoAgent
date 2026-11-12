package memory

import (
	"fmt"
	"testing"
	"time"

	"github.com/metabbe3/GoAgent/internal/llm"
)

func TestSaveAndLoadHistoryWithTTL(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	msgs := []llm.ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}

	if err := store.SaveHistory("conv-1", msgs, time.Hour); err != nil {
		t.Fatalf("save history failed: %v", err)
	}

	loaded := store.GetHistory("conv-1")
	if len(loaded) != 2 || loaded[0].Content != "hello" {
		t.Fatalf("unexpected loaded history: %v", loaded)
	}

	// Reopen with expired TTL
	store2 := NewStore("")
	store2.dir = tmpDir
	store2.historyPath = store.historyPath
	store2.SetHistoryTTL(1 * time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	store2.loadHistory()

	if len(store2.All()) != 0 {
		t.Errorf("expected history to be filtered by TTL at load, got %d items", len(store2.All()))
	}
}

func TestKnowledgeCap200(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	for i := 0; i < 250; i++ {
		err := store.AppendKnowledge(KnowledgeEntry{
			File:    fmt.Sprintf("file_%d.txt", i),
			Summary: fmt.Sprintf("summary %d", i),
		})
		if err != nil {
			t.Fatalf("append knowledge failed: %v", err)
		}
	}

	if len(store.knowledge) != 200 {
		t.Fatalf("expected in-memory cap of 200, got %d", len(store.knowledge))
	}

	// MaxKnowledgeEntries bounds recall
	recalls := store.Knowledge()
	if len(recalls) != MaxKnowledgeEntries {
		t.Fatalf("expected %d entries from Knowledge(), got %d", MaxKnowledgeEntries, len(recalls))
	}
}
