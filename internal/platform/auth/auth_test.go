package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "test-secret"

func newTestJWT(ttl time.Duration) *JWT {
	return NewJWT(JWTConfig{Secret: testSecret, TTL: ttl})
}

func TestJWTIssueAndVerify(t *testing.T) {
	j := newTestJWT(2 * time.Hour)

	token, err := j.Issue(42, "user")
	require.NoError(t, err)

	claims, err := j.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, int64(42), claims.UserID)
	assert.Equal(t, "user", claims.Role)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), claims.ExpiresAt, time.Minute)
}

func TestJWTRejectExpiredToken(t *testing.T) {
	j := newTestJWT(2 * time.Hour)

	// 手工签发一个已过期的令牌。
	claims := jwtClaims{
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "1",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-3 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, err = j.Verify(context.Background(), token)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestJWTRejectTokenWithoutExpiration(t *testing.T) {
	j := newTestJWT(2 * time.Hour)
	claims := jwtClaims{
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "1",
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, err = j.Verify(context.Background(), token)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestJWTRejectTamperedToken(t *testing.T) {
	j := newTestJWT(2 * time.Hour)

	token, err := j.Issue(1, "user")
	require.NoError(t, err)

	// 篡改签名部分。
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	tampered := parts[0] + "." + parts[1] + ".AAAA"

	_, err = j.Verify(context.Background(), tampered)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestJWTRejectWrongSecret(t *testing.T) {
	j := newTestJWT(2 * time.Hour)
	other := NewJWT(JWTConfig{Secret: "other-secret", TTL: 2 * time.Hour})

	token, err := other.Issue(1, "user")
	require.NoError(t, err)

	_, err = j.Verify(context.Background(), token)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestJWTRejectMalformedToken(t *testing.T) {
	j := newTestJWT(2 * time.Hour)
	_, err := j.Verify(context.Background(), "not-a-jwt")
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestMiddlewareRequiresValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	j := newTestJWT(2 * time.Hour)

	router := gin.New()
	router.GET("/protected", Middleware(j), func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		require.True(t, ok)
		c.JSON(http.StatusOK, gin.H{"user_id": claims.UserID, "role": claims.Role})
	})

	// 无 token → 401。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 非法 token → 401。
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 过期 token → 401。
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	})
	expiredStr, err := expired.SignedString([]byte(testSecret))
	require.NoError(t, err)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+expiredStr)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 合法 token → 200 且声明正确。
	valid, err := j.Issue(7, "admin")
	require.NoError(t, err)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+valid)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"user_id":7,"role":"admin"}`, w.Body.String())
}

func TestRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	j := newTestJWT(2 * time.Hour)

	router := gin.New()
	router.GET("/admin", Middleware(j), RequireAdmin(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	// 普通用户 → 403。
	userToken, err := j.Issue(1, "user")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)

	// admin → 204。
	adminToken, err := j.Issue(2, "admin")
	require.NoError(t, err)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
}
