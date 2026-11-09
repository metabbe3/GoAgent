package tg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metabbe3/GoAgent/internal/config"
)

func TestSplitMessage4096(t *testing.T) {
	// Create text with lines totaling > 5000 chars
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(fmt.Sprintf("Line %03d: This is a test log message with some content to fill up the buffer.\n", i))
	}
	longText := sb.String()

	if len(longText) <= MaxMessageLength {
		t.Fatalf("expected test text len > 4096, got %d", len(longText))
	}

	chunks := SplitMessage(longText, MaxMessageLength)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	for i, chunk := range chunks {
		if len(chunk) > MaxMessageLength {
			t.Errorf("chunk %d length %d exceeds max %d", i, len(chunk), MaxMessageLength)
		}
	}

	// Verify all content preserved
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "Line 000:") || !strings.Contains(joined, "Line 199:") {
		t.Errorf("split message lost initial or final content")
	}
}

func TestSendMessageFallbackPlainOnHttp400(t *testing.T) {
	markdownAttempted := false
	plainSucceeded := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			http.NotFound(w, r)
			return
		}

		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.ParseMode == "Markdown" {
			markdownAttempted = true
			// Simulate Telegram Markdown parse error
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(BotResponse[any]{
				OK:          false,
				ErrorCode:   400,
				Description: "Bad Request: can't parse entities: Character '_' is reserved",
			})
			return
		}

		// Plain text retry (no parse_mode)
		if req.ParseMode == "" {
			plainSucceeded = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(BotResponse[any]{
				OK:     true,
				Result: map[string]any{"message_id": 101},
			})
			return
		}
	}))
	defer server.Close()

	cfg := &config.Config{
		TelegramToken: "test-token",
	}

	bot := NewBotWithBaseURL(cfg, nil, server.URL)
	err := bot.SendMessage(context.Background(), 12345, "Unclosed *markdown _broken text")
	if err != nil {
		t.Fatalf("expected SendMessage to succeed via plain fallback, got error: %v", err)
	}

	if !markdownAttempted {
		t.Errorf("expected initial Markdown attempt")
	}
	if !plainSucceeded {
		t.Errorf("expected plain text retry to succeed")
	}
}

func TestGetUpdatesMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/getUpdates") {
			http.NotFound(w, r)
			return
		}

		resp := BotResponse[[]Update]{
			OK: true,
			Result: []Update{
				{
					UpdateID: 100,
					Message: &Message{
						MessageID: 1,
						Chat:      Chat{ID: 999},
						Text:      "Hello agent",
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.Config{
		TelegramToken: "test-token",
	}

	bot := NewBotWithBaseURL(cfg, nil, server.URL)
	updates, err := bot.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetUpdates failed: %v", err)
	}

	if len(updates) != 1 || updates[0].UpdateID != 100 {
		t.Errorf("unexpected updates returned: %+v", updates)
	}
}
