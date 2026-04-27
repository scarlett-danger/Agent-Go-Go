package main

import (
	"context"

	"log"

	"net/http"

	"os"

	"os/signal"

	"path/filepath"

	"syscall"

	"time"

	sentrygin "github.com/getsentry/sentry-go/gin"

	"github.com/gin-gonic/gin"

	"github.com/slack-go/slack"

	temporalclient "go.temporal.io/sdk/client"

	"go.temporal.io/sdk/worker"

	"github.com/scarlett-danger/Agent-Go-Go/config"

	"github.com/scarlett-danger/Agent-Go-Go/handlers"

	"github.com/scarlett-danger/Agent-Go-Go/llm"

	"github.com/scarlett-danger/Agent-Go-Go/mcp"

	"github.com/scarlett-danger/Agent-Go-Go/memory"

	"github.com/scarlett-danger/Agent-Go-Go/observability"

	"github.com/scarlett-danger/Agent-Go-Go/router"

	bottemporal "github.com/scarlett-danger/Agent-Go-Go/temporal"
)

// systemPrompt shapes how every expert model replies. Tightly coupled to

// the human-disguise behavior — it's a code-level concern, not routing.

const systemPrompt = `You are responding as a real human in a Slack conversation.

Keep replies concise, conversational, and natural. Never mention that you are

an AI, model, assistant, or bot. Match the tone of the channel. Avoid markdown

headers and bullet lists unless the question explicitly asks for a list.`

func main() {

	// --- Config ------------------------------------------------------------

	cfg, err := config.Load()

	if err != nil {

		log.Fatalf("config: %v", err)

	}

	// --- Sentry (init FIRST; for non-LLM errors only) ---------------------

	// LLM observability lives in Langfuse via OpenRouter. Sentry catches

	// infrastructure failures: Slack handler errors, Temporal startup,

	// SQLite issues, MCP subprocess crashes.

	if _, err := observability.Init(observability.Config{

		DSN: cfg.SentryDSN,

		Environment: cfg.SentryEnvironment,

		Release: cfg.SentryRelease,

		TracesSampleRate: cfg.SentryTracesSampleRate,
	}); err != nil {

		log.Fatalf("sentry: %v", err)

	}

	defer observability.Flush(5 * time.Second)

	// --- SQLite memory store ----------------------------------------------

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {

		log.Fatalf("create db dir: %v", err)

	}

	store, err := memory.Open(cfg.DBPath)

	if err != nil {

		log.Fatalf("memory store: %v", err)

	}

	defer store.Close()

	log.Printf("[memory] sqlite ready at %s", cfg.DBPath)

	// --- OpenRouter client (one client serves every model) ---------------

	openRouter := llm.NewOpenRouter(cfg.OpenRouterAPIKey, cfg.OpenRouterURL)

	log.Printf("[llm] openrouter client ready, judge=%s", cfg.JudgeModel)

	// --- Routing config ---------------------------------------------------

	routingCfg, err := router.LoadConfig(cfg.RoutingConfigPath)

	if err != nil {

		log.Fatalf("routing config: %v", err)

	}

	log.Printf("[router] %d categories loaded, default=%s",

		len(routingCfg.Categories), routingCfg.Default)

	// --- MCP host (subprocess servers) ------------------------------------

	mcpFile, err := mcp.LoadConfig(cfg.MCPConfigPath)

	if err != nil {

		log.Fatalf("mcp config: %v", err)

	}

	mcpStartCtx, mcpCancel := context.WithTimeout(context.Background(), 60*time.Second)

	mcpHost, mcpErrs := mcp.NewHost(mcpStartCtx, mcpFile.Servers)

	mcpCancel()

	for _, e := range mcpErrs {

		// Don't crash on MCP startup failures — degrade gracefully so

		// no-tool categories still work. Sentry captures the failure.

		log.Printf("[mcp] startup error: %v", e)

		observability.CaptureError(context.Background(), e,

			map[string]string{"component": "mcp.startup"}, nil)

	}

	defer mcpHost.Close()

	// Validate routing.yaml against actual loaded tools (warn-only).

	availableTools := make(map[string]bool)

	for _, t := range mcpHost.AllTools() {

		availableTools[t.Name] = true

	}

	for catName, rule := range routingCfg.Categories {

		for _, toolName := range rule.Tools {

			if !availableTools[toolName] {

				log.Printf("[router] WARN: category %q references unknown tool %q",

					catName, toolName)

			}

		}

	}

	log.Printf("[mcp] host ready, %d total tools available", len(availableTools))

	// --- Judge ------------------------------------------------------------

	judge := router.NewJudge(openRouter, cfg.JudgeModel, routingCfg)

	// --- Slack client (User Token, xoxp-) --------------------------------

	slackClient := slack.New(cfg.SlackUserToken)

	if cfg.SlackBotUserID == "" {

		auth, err := slackClient.AuthTest()

		if err != nil {

			log.Fatalf("slack auth.test (verify SLACK_USER_TOKEN): %v", err)

		}

		cfg.SlackBotUserID = auth.UserID

		log.Printf("[slack] auto-detected user_id=%s team=%s", auth.UserID, auth.Team)

	}

	// --- Temporal client + worker ----------------------------------------

	tc, err := temporalclient.Dial(temporalclient.Options{

		HostPort: cfg.TemporalHostPort,

		Namespace: cfg.TemporalNamespace,
	})

	if err != nil {

		log.Fatalf("temporal dial: %v", err)

	}

	defer tc.Close()

	w := worker.New(tc, cfg.TemporalTaskQueue, worker.Options{})

	activities := &bottemporal.Activities{

		SlackClient: slackClient,

		BotUserID: cfg.SlackBotUserID,

		Store: store,

		OpenRouter: openRouter,

		Router: routingCfg,

		Judge: judge,

		MCPHost: mcpHost,

		SystemPrompt: systemPrompt,

		MaxTurns: cfg.MemoryMaxTurns,

		SummarizeAt: cfg.MemorySummarizeAt,

		SummarizeFold: cfg.MemorySummarizeFold,

		SummarizerModel: cfg.JudgeModel, // reuse judge model for summaries

	}

	w.RegisterWorkflow(bottemporal.ProcessMessageWorkflow)

	w.RegisterActivity(activities)

	workerErrCh := make(chan error, 1)

	go func() {

		log.Printf("[temporal] worker starting on task_queue=%s", cfg.TemporalTaskQueue)

		if err := w.Run(worker.InterruptCh()); err != nil {

			workerErrCh <- err

		}

	}()

	// --- HTTP server ------------------------------------------------------

	engine := gin.Default()

	engine.Use(sentrygin.New(sentrygin.Options{Repanic: true}))

	slackHandler := handlers.NewSlackHandler(

		cfg.SlackSigningSecret,

		cfg.SlackBotUserID,

		tc,

		cfg.TemporalTaskQueue,

		cfg.TriggerKeywords,
	)

	engine.POST("/slack/events", slackHandler.HandleEvents)

	engine.GET("/", func(c *gin.Context) {

		c.JSON(http.StatusOK, gin.H{

			"service": "Agent-Go-Go",

			"status": "ok",

			"endpoints": []string{"/health", "/slack/events"},
		})

	})

	engine.GET("/health", func(c *gin.Context) {

		c.JSON(http.StatusOK, gin.H{"status": "ok"})

	})

	server := &http.Server{

		Addr: ":" + cfg.ServerPort,

		Handler: engine,

		ReadTimeout: 15 * time.Second,

		WriteTimeout: 15 * time.Second,
	}

	go func() {

		log.Printf("[http] listening on :%s", cfg.ServerPort)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {

			log.Fatalf("http server: %v", err)

		}

	}()

	// --- Graceful shutdown ------------------------------------------------

	quit := make(chan os.Signal, 1)

	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {

	case <-quit:

		log.Println("shutdown signal received")

	case err := <-workerErrCh:

		log.Printf("worker exited: %v", err)

	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)

	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {

		log.Printf("http shutdown: %v", err)

	}

	w.Stop()

	log.Println("Agent-Go-Go signing off")

}
