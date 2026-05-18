package handlers

import (
	"log/slog"
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

// DebugEnv handles GET /api/debug/env
// Returns all environment variables and .env file contents for testing/debugging.
// WARNING: This exposes all secrets and credentials — use only in development!
func DebugEnv(c *gin.Context) {
	slog.Warn("DebugEnv endpoint called — exposing all environment variables",
		"remote_ip", c.RemoteIP(),
		"user_agent", c.Request.UserAgent(),
	)

	envVars := make(map[string]string)
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envVars[parts[0]] = parts[1]
		}
	}

	// Try to read .env file from current working directory
	envFileContent := "-- .env file not found or not readable --"
	envFilePath := ".env"
	if data, err := os.ReadFile(envFilePath); err == nil {
		envFileContent = string(data)
	} else {
		slog.Debug("DebugEnv: could not read .env file",
			"path", envFilePath,
			"error", err,
		)
		// Try alternative paths
		for _, altPath := range []string{"/app/.env", "/root/.env", "/home/.env"} {
			if data, err := os.ReadFile(altPath); err == nil {
				envFileContent = string(data)
				envFilePath = altPath
				break
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"warning":          "SECURITY: This endpoint exposes all secrets and credentials",
		"environment_vars": envVars,
		"env_file_path":    envFilePath,
		"env_file_content": envFileContent,
		"request_info": gin.H{
			"remote_ip":  c.RemoteIP(),
			"user_agent": c.Request.UserAgent(),
			"timestamp":  c.GetString("timestamp"),
		},
	})
}
