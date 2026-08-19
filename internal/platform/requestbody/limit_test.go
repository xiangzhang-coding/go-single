package requestbody

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLimitJSONRejectsKnownOversizeBeforeReading(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := LimitJSON(8)
	require.NoError(t, err)

	read := 0
	body := &countingReader{reader: bytes.NewReader([]byte(`{"a":1234}`)), read: &read}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 9

	called := false
	r := gin.New()
	r.POST("/", limit, func(c *gin.Context) { called = true })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.False(t, called)
	require.Zero(t, read)
}

func TestLimitJSONRejectsUnknownLengthWithoutReadingWholeBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := LimitJSON(8)
	require.NoError(t, err)

	read := 0
	body := &countingReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), read: &read}
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1

	r := gin.New()
	r.POST("/", limit, func(c *gin.Context) { t.Fatal("oversize request reached handler") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.LessOrEqual(t, read, 9)
}

func TestLimitJSONPreservesAllowedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := LimitJSON(8)
	require.NoError(t, err)

	r := gin.New()
	r.POST("/", limit, func(c *gin.Context) {
		body, readErr := io.ReadAll(c.Request.Body)
		require.NoError(t, readErr)
		require.Equal(t, []byte(`{"a":1}`), body)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestLimitJSONCannotBeBypassedWithSpoofedContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := LimitJSON(8)
	require.NoError(t, err)

	r := gin.New()
	r.POST("/", limit, func(c *gin.Context) { t.Fatal("oversize request reached handler") })
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"padding":"large"}`))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestLimitJSONRejectsInvalidLimit(t *testing.T) {
	_, err := LimitJSON(0)
	require.Error(t, err)
}

type countingReader struct {
	reader io.Reader
	read   *int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	*r.read += n
	return n, err
}
