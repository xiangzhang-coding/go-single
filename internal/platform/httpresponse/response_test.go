package httpresponse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dependencyTimeout struct{}

func (dependencyTimeout) Error() string   { return "dial tcp 10.0.0.1: secret timeout" }
func (dependencyTimeout) Timeout() bool   { return true }
func (dependencyTimeout) Temporary() bool { return true }

func TestWriteErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	errValidation := errors.New("invalid input")
	errUnauthenticated := errors.New("invalid credentials")
	errForbidden := errors.New("forbidden")
	errNotFound := errors.New("not found")
	errConflict := errors.New("conflict")
	errRateLimited := errors.New("rate limited")
	rules := []Rule{
		{Status: http.StatusBadRequest, Errors: []error{errValidation}},
		{Status: http.StatusUnauthorized, Errors: []error{errUnauthenticated}, Message: "invalid username or password"},
		{Status: http.StatusForbidden, Errors: []error{errForbidden}},
		{Status: http.StatusNotFound, Errors: []error{errNotFound}},
		{Status: http.StatusConflict, Errors: []error{errConflict}},
		{Status: http.StatusTooManyRequests, Errors: []error{errRateLimited}},
	}

	tests := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{name: "validation", err: fmt.Errorf("field detail: %w", errValidation), status: http.StatusBadRequest, body: `{"error":"invalid input"}`},
		{name: "unauthenticated", err: errUnauthenticated, status: http.StatusUnauthorized, body: `{"error":"invalid username or password"}`},
		{name: "forbidden", err: errForbidden, status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{name: "not found", err: errNotFound, status: http.StatusNotFound, body: `{"error":"not found"}`},
		{name: "conflict", err: errConflict, status: http.StatusConflict, body: `{"error":"conflict"}`},
		{name: "rate limited", err: errRateLimited, status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`},
		{name: "business error with dependency detail", err: errors.Join(errConflict, errors.New("redis password=secret")), status: http.StatusConflict, body: `{"error":"conflict"}`},
		{name: "context deadline", err: fmt.Errorf("database query: %w", context.DeadlineExceeded), status: http.StatusGatewayTimeout, body: `{"error":"request timeout"}`},
		{name: "dependency timeout", err: fmt.Errorf("redis command: %w", dependencyTimeout{}), status: http.StatusGatewayTimeout, body: `{"error":"request timeout"}`},
		{name: "unknown", err: errors.New("mysql password=secret"), status: http.StatusInternalServerError, body: `{"error":"internal error"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)

			WriteError(c, tt.err, rules...)

			require.Equal(t, tt.status, recorder.Code)
			require.JSONEq(t, tt.body, recorder.Body.String())
			require.NotContains(t, recorder.Body.String(), "secret")
		})
	}
}

func TestWriteUsesErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	Write(c, http.StatusUnauthorized, "missing token")

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.JSONEq(t, `{"error":"missing token"}`, recorder.Body.String())
}
