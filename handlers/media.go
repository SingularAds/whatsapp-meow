package handlers

import (
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// MediaHandler returns a Gin handler for GET /media/:filename
// It serves files from mediaDir. A path-traversal guard ensures only files
// inside mediaDir can be accessed regardless of what the client sends.
func MediaHandler(mediaDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// c.Param includes the leading '/', e.g. "/abc123.ogg"
		filename := filepath.Base(c.Param("filename"))
		if filename == "" || filename == "." || filename == "/" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
			return
		}

		// Build the absolute path and serve.
		absPath := filepath.Join(mediaDir, filename)
		c.File(absPath) // returns 404 automatically if file is absent
	}
}
