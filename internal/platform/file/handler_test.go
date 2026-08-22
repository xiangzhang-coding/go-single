package file

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

type readCounter struct {
	reads atomic.Int32
}

func (r *readCounter) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

func issueUploadToken(t *testing.T, verifier *auth.JWT, userID int64) string {
	t.Helper()
	token, err := verifier.Issue(userID, "user")
	require.NoError(t, err)
	return token
}

func uploadRequest(handler http.Handler, token, sourceIP string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.RemoteAddr = sourceIP + ":1234"
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUploadConcurrencyRejectsBeforeReadingBodyAndReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name       string
		cfg        UploadConcurrencyConfig
		firstUser  int64
		secondUser int64
		firstIP    string
		secondIP   string
	}{
		{
			name: "global", cfg: UploadConcurrencyConfig{MaxConcurrent: 1, MaxConcurrentPerUser: 10, MaxConcurrentPerIP: 10},
			firstUser: 1, secondUser: 2, firstIP: "192.0.2.1", secondIP: "192.0.2.2",
		},
		{
			name: "user", cfg: UploadConcurrencyConfig{MaxConcurrent: 10, MaxConcurrentPerUser: 1, MaxConcurrentPerIP: 10},
			firstUser: 1, secondUser: 1, firstIP: "192.0.2.1", secondIP: "192.0.2.2",
		},
		{
			name: "source IP", cfg: UploadConcurrencyConfig{MaxConcurrent: 10, MaxConcurrentPerUser: 10, MaxConcurrentPerIP: 1},
			firstUser: 1, secondUser: 2, firstIP: "192.0.2.1", secondIP: "192.0.2.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier := auth.NewJWT(auth.JWTConfig{Secret: "upload-concurrency-test", TTL: time.Hour})
			budget := newUploadConcurrency(tc.cfg)
			entered := make(chan struct{})
			unblock := make(chan struct{})
			r := gin.New()
			r.POST("/upload", auth.Middleware(verifier), budget.middleware(), func(c *gin.Context) {
				if c.GetHeader("X-Block") == "true" {
					close(entered)
					<-unblock
				}
				c.Status(http.StatusNoContent)
			})
			firstToken := issueUploadToken(t, verifier, tc.firstUser)
			secondToken := issueUploadToken(t, verifier, tc.secondUser)

			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				firstDone <- uploadRequest(r, firstToken, tc.firstIP, nil, map[string]string{"X-Block": "true"})
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first upload did not acquire its slot")
			}

			body := &readCounter{}
			rejected := uploadRequest(r, secondToken, tc.secondIP, body, nil)
			require.Equal(t, http.StatusTooManyRequests, rejected.Code)
			require.JSONEq(t, `{"error":"upload concurrency limit exceeded"}`, rejected.Body.String())
			require.Zero(t, body.reads.Load(), "rejected upload body must not be parsed")

			close(unblock)
			require.Equal(t, http.StatusNoContent, (<-firstDone).Code)
			require.Equal(t, http.StatusNoContent,
				uploadRequest(r, secondToken, tc.secondIP, nil, nil).Code,
				"slot must be reusable after the admitted request exits")
		})
	}
}

func TestUploadConcurrencyReleasesOnAbortAndPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := auth.NewJWT(auth.JWTConfig{Secret: "upload-concurrency-exit-test", TTL: time.Hour})
	budget := newUploadConcurrency(UploadConcurrencyConfig{
		MaxConcurrent: 1, MaxConcurrentPerUser: 1, MaxConcurrentPerIP: 1,
	})
	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	r.POST("/upload", auth.Middleware(verifier), budget.middleware(), func(c *gin.Context) {
		switch c.GetHeader("X-Exit") {
		case "abort":
			c.AbortWithStatus(http.StatusBadRequest)
		case "panic":
			panic("test panic")
		default:
			c.Status(http.StatusNoContent)
		}
	})
	token := issueUploadToken(t, verifier, 1)

	require.Equal(t, http.StatusBadRequest, uploadRequest(r, token, "192.0.2.1", nil, map[string]string{"X-Exit": "abort"}).Code)
	require.Equal(t, http.StatusInternalServerError, uploadRequest(r, token, "192.0.2.1", nil, map[string]string{"X-Exit": "panic"}).Code)
	require.Equal(t, http.StatusNoContent, uploadRequest(r, token, "192.0.2.1", nil, nil).Code)
}
