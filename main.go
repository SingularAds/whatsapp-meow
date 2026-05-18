package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"whatsapp-bridge/analytics"
	"whatsapp-bridge/client"
	"whatsapp-bridge/config"
	"whatsapp-bridge/handlers"
	"whatsapp-bridge/intent"
	"whatsapp-bridge/middleware"
	"whatsapp-bridge/webhook"
)

func main() {
	// ── Environment ───────────────────────────────────────────────────────────
	if err := godotenv.Load(); err != nil {
		slog.Warn(".env file not found, relying on environment variables")
	}

	cfg := config.Load()
	setupLogger(cfg.LogLevel)
	logProductionWarnings(cfg)

	// ── Media directory ───────────────────────────────────────────────────────
	mediaDir := filepath.Join(cfg.DBDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		slog.Error("cannot create media dir", "error", err)
		os.Exit(1)
	}

	// ── Core services ───────────────────────────────────────────────────────────
	sender := webhook.NewSender(cfg.WebhookURL, cfg.WebhookSecret)

	// Always log the webhook destination at startup so Cloud Run logs confirm
	// the correct backend URL is in use.  A localhost URL in production is the
	// most common cause of "no messages processed" after a container restart.
	slog.Info("webhook target configured",
		"url", cfg.WebhookURL,
		"secret_set", cfg.WebhookSecret != "",
	)
	if cfg.WebhookURL == "" || cfg.WebhookURL == "http://localhost:8000" || cfg.WebhookURL == "http://127.0.0.1:8000" {
		slog.Warn("WEBHOOK_URL appears to be a localhost address — webhooks will NOT reach the Python backend in production. Set WEBHOOK_URL to the Cloud Run backend URL.",
			"webhook_url", cfg.WebhookURL,
		)
	}

	// Intent state store: caches per-chat business/personal classification.
	intentStore := intent.NewStateStore()

	// PostHog tracker: no-op when POSTHOG_API_KEY is empty.
	tracker := analytics.NewTracker(cfg.PostHogAPIKey, cfg.PostHogHost)
	if cfg.PostHogAPIKey != "" {
		slog.Info("[analytics] PostHog tracking enabled", "host", cfg.PostHogHost)
	} else {
		slog.Info("[analytics] PostHog tracking disabled (POSTHOG_API_KEY not set)")
	}

	mgr := client.NewManager(cfg.DBDir, mediaDir, cfg.PublicURL, sender, intentStore, tracker, cfg.DefaultSessionID)

	// Load sessions that already have databases on disk.
	mgr.LoadExisting(context.Background())

	// Always ensure the default session (global onboarding number) is initialised.
	// If its DB file didn't exist LoadExisting skipped it; EnsureSession creates it
	// and starts a QR-code flow so it can be paired immediately.
	if cfg.DefaultSessionID != "" {
		if _, err := mgr.EnsureSession(context.Background(), cfg.DefaultSessionID); err != nil {
			slog.Warn("could not ensure default session",
				"session_id", cfg.DefaultSessionID, "error", err)
		}
	}

	// ── HTTP router ───────────────────────────────────────────────────────────
	if cfg.LogLevel != "DEBUG" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(ginLogger())

	// Health check — no auth required.
	router.GET("/api/health", handlers.Health)
	router.GET("/api/debug/env", handlers.DebugEnv)

	// All other routes require Basic-Auth.
	auth := middleware.BasicAuth(cfg.BridgeUsername, cfg.BridgePassword)

	router.POST("/send/message", auth, handlers.SendMessageHandler(mgr))
	router.GET("/media/*filename", auth, handlers.MediaHandler(mediaDir))
	router.GET("/api/sessions", auth, handlers.SessionsListHandler(mgr))
	router.GET("/api/sessions/:session_id", auth, handlers.SessionHandler(mgr))
	router.POST("/api/sessions/:session_id/reconnect", auth, handlers.SessionReconnectHandler(mgr))
	router.POST("/api/sessions/:session_id/logout", auth, handlers.SessionLogoutHandler(mgr))
	router.POST("/api/pair-code", auth, handlers.PairCodeHandler(mgr))
	router.GET("/api/default-session", auth, handlers.DefaultSessionStatusHandler(mgr, cfg.DefaultSessionID))
	router.POST("/api/default-session/reconnect", auth, handlers.DefaultSessionReconnectHandler(mgr, cfg.DefaultSessionID))
	router.POST("/api/default-session/pair-code", auth, handlers.DefaultSessionPairCodeHandler(mgr, cfg.DefaultSessionID))

	// QR-code pairing flow:
	// POST /api/qr-payload   — start (or reuse) QR session, block until first payload ready
	// GET  /api/qr-current/:session_id — fetch the latest payload without blocking (for refresh)
	router.POST("/api/qr-payload", auth, handlers.QRPayloadHandler(mgr))
	router.GET("/api/qr-current/:session_id", auth, handlers.QRCurrentHandler(mgr))

	// ── HTTP server with graceful shutdown ────────────────────────────────────
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("WhatsApp bridge starting", "port", cfg.Port, "public_url", cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start the media TTL cleanup goroutine.
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	go cleanupLoop(cleanupCtx, mediaDir, cfg.MediaTTLSeconds)

	// Wait for OS signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down…")
	cleanupCancel()
	intentStore.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := mgr.Close(ctx); err != nil {
		slog.Error("session manager shutdown error", "error", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
	slog.Info("bridge stopped")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}

// ginLogger returns a minimal Gin middleware that uses slog.
func ginLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Debug("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency", time.Since(start),
		)
	}
}

// logProductionWarnings warns about settings that are acceptable for local dev
// but risky for production deployments.
func logProductionWarnings(cfg config.Config) {
	if !filepath.IsAbs(cfg.DBDir) {
		slog.Warn("DB_DIR is relative; use an absolute persistent path in production", "db_dir", cfg.DBDir)
	}
	if strings.Contains(strings.ToLower(cfg.PublicURL), "localhost") {
		slog.Warn("PUBLIC_URL points to localhost; media_url links will be unreachable outside this host", "public_url", cfg.PublicURL)
	}
	if strings.Contains(strings.ToLower(cfg.WebhookURL), "localhost") {
		slog.Warn("WEBHOOK_URL points to localhost; webhooks will fail unless backend runs on the same host", "webhook_url", cfg.WebhookURL)
	}
	if cfg.BridgeUsername == "admin" && cfg.BridgePassword == "changeme" {
		slog.Warn("default bridge credentials are in use; set BRIDGE_USERNAME/BRIDGE_PASSWORD before production")
	}
}

// cleanupLoop runs every minute and removes media files older than ttlSeconds.
func cleanupLoop(ctx context.Context, mediaDir string, ttlSeconds int) {
	ttl := time.Duration(ttlSeconds) * time.Second
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		entries, err := os.ReadDir(mediaDir)
		if err != nil {
			continue
		}
		cutoff := time.Now().Add(-ttl)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(mediaDir, e.Name())
				if err := os.Remove(path); err == nil {
					slog.Debug("cleaned up media file", "file", e.Name())
				}
			}
		}
	}
}
