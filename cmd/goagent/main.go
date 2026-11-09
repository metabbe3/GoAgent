package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/metabbe3/GoAgent/internal/agent"
	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/tg"
	"github.com/metabbe3/GoAgent/internal/tools"
)

const version = "0.1.0"

func main() {
	log.Printf("Starting GoAgent v%s...", version)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Please configure GoAgent by running:\n")
		fmt.Fprintf(os.Stderr, "  ./deploy.sh install\n")
		fmt.Fprintf(os.Stderr, "or by copying goagent.env.example to goagent.env and setting TELEGRAM_TOKEN.\n")
		os.Exit(1)
	}

	log.Printf("Loaded config: LLM Model=%s, BaseURL=%s, ShellAllowed=%v, MaxIter=%d",
		cfg.LLMModel, cfg.LLMBaseURL, cfg.AllowShell, cfg.MaxIterations)
	if cfg.AdminChatID != 0 {
		log.Printf("Admin restriction active: only chat ID %d will be answered", cfg.AdminChatID)
	}

	// Initialize components
	llmClient := llm.NewClient(cfg)
	toolRegistry := tools.NewRegistry(cfg)
	agentLoop := agent.NewLoop(llmClient, toolRegistry, cfg.MaxIterations)
	bot := tg.NewBot(cfg, agentLoop)

	// Context with graceful shutdown on interrupt
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("GoAgent is online and polling Telegram. Press Ctrl+C to stop.")
	if err := bot.StartLongPoll(ctx); err != nil {
		log.Fatalf("Fatal bot error: %v", err)
	}

	log.Printf("GoAgent exited cleanly.")
}
