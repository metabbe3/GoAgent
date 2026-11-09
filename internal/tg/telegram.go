package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/agent"
	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/guardrail"
	"github.com/metabbe3/GoAgent/internal/llm"
)

const (
	MaxMessageLength = 4096
	PollTimeoutSec   = 25
)

// Update represents an incoming update from Telegram.
type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

// BotResponse represents Telegram API response envelope.
type BotResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

// SendMessageRequest is the payload for sendMessage.
type SendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// Bot manages the Telegram long-polling loop and message transport.
type Bot struct {
	token       string
	apiBaseURL  string
	adminChatID int64
	agentLoop   *agent.Loop
	httpClient  *http.Client
	chatHistory map[int64][]llm.ChatMessage
}

// NewBot initializes a new Telegram Bot client.
func NewBot(cfg *config.Config, agentLoop *agent.Loop) *Bot {
	return NewBotWithBaseURL(cfg, agentLoop, "https://api.telegram.org")
}

// NewBotWithBaseURL allows specifying custom API base URL for testing.
func NewBotWithBaseURL(cfg *config.Config, agentLoop *agent.Loop, apiBaseURL string) *Bot {
	return &Bot{
		token:       cfg.TelegramToken,
		apiBaseURL:  strings.TrimRight(apiBaseURL, "/"),
		adminChatID: cfg.AdminChatID,
		agentLoop:   agentLoop,
		httpClient: &http.Client{
			Timeout: 40 * time.Second,
		},
		chatHistory: make(map[int64][]llm.ChatMessage),
	}
}

// SplitMessage splits a long string into chunks <= maxLen, splitting on newlines when possible.
func SplitMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = MaxMessageLength
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	remaining := text

	for len(remaining) > maxLen {
		// Look for the last newline within the allowed chunk
		splitIdx := strings.LastIndex(remaining[:maxLen], "\n")
		if splitIdx <= 0 {
			// No newline found, hard split at maxLen
			splitIdx = maxLen
		}

		chunk := strings.TrimRight(remaining[:splitIdx], "\r\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}

		if splitIdx < len(remaining) && remaining[splitIdx] == '\n' {
			remaining = remaining[splitIdx+1:]
		} else {
			remaining = remaining[splitIdx:]
		}
	}

	remaining = strings.TrimSpace(remaining)
	if remaining != "" {
		chunks = append(chunks, remaining)
	}

	return chunks
}

// SendMessage delivers text to the given chat ID with automatic Markdown fallback and splitting.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	chunks := SplitMessage(text, MaxMessageLength)
	for _, chunk := range chunks {
		if err := b.sendSingleChunk(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) sendSingleChunk(ctx context.Context, chatID int64, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", b.apiBaseURL, b.token)

	// First try: Markdown
	reqBody := SendMessageRequest{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "Markdown",
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send telegram request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var apiResp BotResponse[any]
	_ = json.Unmarshal(respBytes, &apiResp)

	// If Markdown fails (HTTP 400 or !ok), retry as plain text
	if resp.StatusCode == http.StatusBadRequest || (!apiResp.OK && apiResp.ErrorCode == 400) {
		log.Printf("[tg] Markdown parsing failed, falling back to plain text for chat %d", chatID)
		plainReqBody := SendMessageRequest{
			ChatID: chatID,
			Text:   stripMarkdown(text),
		}
		plainData, err := json.Marshal(plainReqBody)
		if err != nil {
			return err
		}

		plainReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(plainData))
		if err != nil {
			return err
		}
		plainReq.Header.Set("Content-Type", "application/json")

		plainResp, err := b.httpClient.Do(plainReq)
		if err != nil {
			return fmt.Errorf("plain text retry failed: %w", err)
		}
		defer plainResp.Body.Close()

		if plainResp.StatusCode < 200 || plainResp.StatusCode >= 300 {
			bBytes, _ := io.ReadAll(plainResp.Body)
			return fmt.Errorf("telegram api error (plain retry, status %d): %s", plainResp.StatusCode, string(bBytes))
		}
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram api error (status %d): %s", resp.StatusCode, string(respBytes))
	}

	return nil
}

func stripMarkdown(s string) string {
	r := strings.NewReplacer("*", "", "_", "", "`", "", "[", "", "]", "", "(", "", ")", "")
	return r.Replace(s)
}

// GetUpdates fetches new messages from Telegram using long-polling.
func (b *Bot) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	url := fmt.Sprintf("%s/bot%s/getUpdates", b.apiBaseURL, b.token)

	bodyMap := map[string]any{
		"offset":  offset,
		"timeout": PollTimeoutSec,
	}
	data, _ := json.Marshal(bodyMap)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram getUpdates error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var botResp BotResponse[[]Update]
	if err := json.Unmarshal(bodyBytes, &botResp); err != nil {
		return nil, fmt.Errorf("failed to decode getUpdates response: %w", err)
	}

	return botResp.Result, nil
}

// StartLongPoll runs the polling loop until ctx is canceled.
func (b *Bot) StartLongPoll(ctx context.Context) error {
	offset := 0
	log.Printf("[tg] Telegram bot started listening for updates...")

	for {
		select {
		case <-ctx.Done():
			log.Printf("[tg] Long poll shutting down...")
			return nil
		default:
		}

		updates, err := b.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[tg] polling error: %v (retrying in 3s)", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}

			if u.Message == nil || u.Message.Text == "" {
				continue
			}

			b.handleIncomingMessage(ctx, u.Message)
		}
	}
}

func (b *Bot) handleIncomingMessage(ctx context.Context, msg *Message) {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	log.Printf("[tg] incoming message from chat %d: %s", chatID, text)

	// Admin authorization check
	if b.adminChatID != 0 && chatID != b.adminChatID {
		log.Printf("[tg] unauthorized chat %d (expected %d)", chatID, b.adminChatID)
		_ = b.sendSingleChunk(ctx, chatID, "Unauthorized. GoAgent is restricted to its administrator.")
		return
	}

	if text == "/start" {
		intro := "*GoAgent v0.2* — General-Purpose Log Analysis & Diagnostic Agent\n\n" +
			"Equipped with Hermes-grade pattern intelligence, multi-tool parallel reasoning, and zero-leak guardrails.\n\n" +
			"Try commands like:\n" +
			"- `Analyze /var/log/system.log`\n" +
			"- `Show timeline of errors in app.log`\n" +
			"- `Show recent errors in app.log`\n" +
			"- `List files in current directory`"
		_ = b.SendMessage(ctx, chatID, intro)
		return
	}

	history := b.chatHistory[chatID]
	reply, newHistory, err := b.agentLoop.Run(ctx, text, history)
	if err != nil {
		log.Printf("[tg] agent loop error: %v", err)
		_ = b.SendMessage(ctx, chatID, fmt.Sprintf("⚠️ *Error processing request:*\n`%v`", err))
		return
	}

	// Persist recent conversation history in memory (keep last 12 messages)
	if len(newHistory) > 12 {
		newHistory = newHistory[len(newHistory)-12:]
	}
	b.chatHistory[chatID] = newHistory

	// Filter reply with guardrail (secret redaction -> strip markup -> leak protection -> cap 3800)
	filteredReply := guardrail.FilterReply(reply)

	if err := b.SendMessage(ctx, chatID, filteredReply); err != nil {
		log.Printf("[tg] failed to send reply to %d: %v", chatID, err)
	}
}
