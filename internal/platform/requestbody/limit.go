// Package requestbody provides bounded request-body handling before decoders run.
package requestbody

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
)

// LimitJSON buffers at most maxBytes+1 bytes before JSON handlers run. Oversize
// streams are rejected without reading the remainder of the request body.
func LimitJSON(maxBytes int64) (gin.HandlerFunc, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("json body limit must be positive: %d", maxBytes)
	}
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBytes {
			c.Abort()
			httpresponse.Write(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBytes+1))
		if err != nil {
			c.Abort()
			httpresponse.Write(c, http.StatusBadRequest, "invalid request body")
			return
		}
		if int64(len(body)) > maxBytes {
			c.Abort()
			httpresponse.Write(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Next()
	}, nil
}
