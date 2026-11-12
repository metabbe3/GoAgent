package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/metabbe3/GoAgent/internal/agent"
	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/kb"
	"github.com/metabbe3/GoAgent/internal/llm"
	"github.com/metabbe3/GoAgent/internal/mesh"
	"github.com/metabbe3/GoAgent/internal/tg"
	"github.com/metabbe3/GoAgent/internal/tools"
)

const version = "0.3.0"

func main() {
	log.Printf("Starting GoAgent v%s (GoMesh orchestration)...", version)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Please configure GoAgent by running:\n")
		fmt.Fprintf(os.Stderr, "  ./deploy.sh install\n")
		fmt.Fprintf(os.Stderr, "or by setting environment variables in goagent.env.\n")
		os.Exit(1)
	}

	if cfg.MeshRole != "solo" {
		log.Printf("[mesh] Loaded config: Role=%s, LLM Model=%s, BaseURL=%s, ShellAllowed=%v, MaxIter=%d",
			cfg.MeshRole, cfg.LLMModel, cfg.LLMBaseURL, cfg.AllowShell, cfg.MaxIterations)
	} else {
		log.Printf("Loaded config: Role=%s, LLM Model=%s, BaseURL=%s, ShellAllowed=%v, MaxIter=%d",
			cfg.MeshRole, cfg.LLMModel, cfg.LLMBaseURL, cfg.AllowShell, cfg.MaxIterations)
	}
	if cfg.AdminChatID != 0 {
		log.Printf("Admin restriction active: only chat ID %d will be answered", cfg.AdminChatID)
	}

	// Initialize LLM and core tools
	llmClient := llm.NewClient(cfg)
	toolRegistry := tools.NewRegistry(cfg)

	// Attach Knowledge Base (RAG)
	kbIndex := kb.NewIndex("data/kb", cfg.LLMEmbedModel, llmClient)
	toolRegistry.RegisterKnowledgeTools(kbIndex)

	// Create agent loop with configured role
	agentLoop := agent.NewLoop(llmClient, toolRegistry, cfg.MaxIterations)
	agentLoop.SetRole(cfg.MeshRole)

	// Context with graceful shutdown on interrupt
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch cfg.MeshRole {
	case "leader":
		if cfg.MeshToken == "" {
			log.Fatalf("[mesh] GOAGENT_MESH_TOKEN is required for leader mode")
		}
		log.Printf("[mesh] starting GoAgent in LEADER mode on %s...", cfg.MeshListen)
		queue := mesh.NewQueue("data", cfg.TaskTimeout, cfg.MissionTimeout)
		server := mesh.NewServer(cfg.MeshListen, cfg.MeshToken, queue)

		// Register leader orchestration tools for LLM
		mesh.RegisterLeaderTools(toolRegistry, server)

		if err := server.Start(); err != nil {
			log.Fatalf("[mesh] fatal mesh leader server error: %v", err)
		}
		defer func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopCancel()
			_ = server.Stop(stopCtx)
		}()

		if cfg.TelegramToken != "" {
			log.Printf("[mesh] Telegram token found: starting dual Telegram bot on leader...")
			bot := tg.NewBot(cfg, agentLoop)
			if err := bot.StartLongPoll(ctx); err != nil {
				log.Fatalf("Fatal bot error: %v", err)
			}
		} else {
			log.Printf("[mesh] mesh leader running without Telegram bot. Press Ctrl+C to stop.")
			<-ctx.Done()
		}

	case "worker":
		if cfg.MeshToken == "" {
			log.Fatalf("[mesh] GOAGENT_MESH_TOKEN is required for worker mode")
		}
		if cfg.LeaderAddr == "" {
			log.Fatalf("[mesh] GOAGENT_LEADER_ADDR is required for worker mode")
		}
		log.Printf("[mesh] starting GoAgent in WORKER mode (ID: %s, Caps: %v)...", cfg.WorkerID, cfg.Capabilities)
		runFunc := func(taskCtx context.Context, goal string) (string, error) {
			reply, _, err := agentLoop.Run(taskCtx, goal, nil)
			return reply, err
		}

		client := mesh.NewClient(cfg.LeaderAddr, cfg.MeshToken, cfg.WorkerID, cfg.Capabilities, runFunc)
		defer client.Stop()

		go func() {
			for {
				if err := client.Start(ctx); err != nil && ctx.Err() == nil {
					log.Printf("[mesh] worker error: %v, reconnecting in 5s...", err)
					select {
					case <-time.After(5 * time.Second):
					case <-ctx.Done():
						return
					}
				} else {
					return
				}
			}
		}()

		if cfg.TelegramToken != "" {
			log.Printf("[mesh] Telegram token found: starting dual Telegram bot on worker...")
			bot := tg.NewBot(cfg, agentLoop)
			if err := bot.StartLongPoll(ctx); err != nil {
				log.Fatalf("Fatal bot error: %v", err)
			}
		} else {
			log.Printf("[mesh] mesh worker running. Polling tasks from leader. Press Ctrl+C to stop.")
			<-ctx.Done()
		}

	default: // "solo"
		log.Printf("GoAgent is online (solo mode) and polling Telegram. Press Ctrl+C to stop.")
		bot := tg.NewBot(cfg, agentLoop)
		if err := bot.StartLongPoll(ctx); err != nil {
			log.Fatalf("Fatal bot error: %v", err)
		}
	}

	log.Printf("GoAgent exited cleanly.")
}
