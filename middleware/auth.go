package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BasicAuth returns a Gin middleware that enforces HTTP Basic Authentication.
// Requests with missing or incorrect credentials receive a 401 response.
func BasicAuth(username, password string) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, p, ok := c.Request.BasicAuth()
		if !ok || u != username || p != password {
			c.Header("WWW-Authenticate", `Basic realm="whatsapp-bridge"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
