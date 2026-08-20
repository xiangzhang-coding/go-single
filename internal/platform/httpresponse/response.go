// Package httpresponse defines the shared HTTP error contract for API handlers.
package httpresponse

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	timeoutMessage  = "request timeout"
	internalMessage = "internal error"
)

// ErrorResponse is the stable error envelope consumed by API clients.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Rule maps module-owned business errors to an HTTP status and public message.
// An empty Message uses the matched sentinel's text, never the wrapped error.
type Rule struct {
	Status  int
	Errors  []error
	Message string
}

// Write sends the shared error envelope.
func Write(c *gin.Context, status int, message string) {
	c.JSON(status, ErrorResponse{Error: message})
}

// WriteInternal sends the generic 500 response without exposing a cause.
func WriteInternal(c *gin.Context) {
	Write(c, http.StatusInternalServerError, internalMessage)
}

// WriteError maps timeouts and module-owned business errors, then safely hides
// every unknown error behind the generic 500 response.
func WriteError(c *gin.Context, err error, rules ...Rule) {
	if IsTimeout(err) {
		Write(c, http.StatusGatewayTimeout, timeoutMessage)
		return
	}

	for _, rule := range rules {
		for _, target := range rule.Errors {
			if target == nil || !errors.Is(err, target) {
				continue
			}
			message := rule.Message
			if message == "" {
				message = target.Error()
			}
			Write(c, rule.Status, message)
			return
		}
	}

	WriteInternal(c)
}

// IsTimeout recognizes both context deadlines and timeout-capable dependency
// errors through wrapping and joined errors.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
