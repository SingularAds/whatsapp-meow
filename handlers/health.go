package handlers

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// Health handles GET /api/health.
// Returns the webhook URL target and DB dir so production misconfiguration is
// immediately visible without enabling DEBUG mode or reading Cloud Run env vars.
func Health(c *gin.Context) {
	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		webhookURL = "http://localhost:8000 (default — NOT set)"
	}
	webhookOK := webhookURL != "" &&
		!strings.HasPrefix(webhookURL, "http://localhost") &&
		!strings.HasPrefix(webhookURL, "http://127.0.0.1")

	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"webhook_url": webhookURL,
		"webhook_ok":  webhookOK,
		"db_dir":      os.Getenv("DB_DIR"),
		"session_id":  os.Getenv("DEFAULT_SESSION_ID"),
	})
}

// NOTE: the former GET /api/debug/env endpoint was removed. It returned every
// environment variable and the raw .env file over an UNAUTHENTICATED route —
// bridge credentials, webhook secret, and API keys included. Never reintroduce
// an endpoint like that; use `gcloud run services describe` / host shell access
// to inspect production configuration instead.
