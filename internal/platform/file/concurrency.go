package file

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
)

const (
	defaultMaxConcurrent        = 16
	defaultMaxConcurrentPerUser = 2
	defaultMaxConcurrentPerIP   = 4
)

// UploadConcurrencyConfig bounds multipart parsing and object-storage work in one process.
type UploadConcurrencyConfig struct {
	MaxConcurrent        int
	MaxConcurrentPerUser int
	MaxConcurrentPerIP   int
}

type uploadConcurrency struct {
	cfg UploadConcurrencyConfig

	mu     sync.Mutex
	total  int
	byUser map[int64]int
	byIP   map[string]int
}

func newUploadConcurrency(cfg UploadConcurrencyConfig) *uploadConcurrency {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.MaxConcurrentPerUser <= 0 {
		cfg.MaxConcurrentPerUser = defaultMaxConcurrentPerUser
	}
	if cfg.MaxConcurrentPerIP <= 0 {
		cfg.MaxConcurrentPerIP = defaultMaxConcurrentPerIP
	}
	return &uploadConcurrency{
		cfg: cfg, byUser: make(map[int64]int), byIP: make(map[string]int),
	}
}

func (b *uploadConcurrency) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := auth.ClaimsFrom(c)
		if !ok {
			c.Abort()
			httpresponse.Write(c, http.StatusUnauthorized, "missing token")
			return
		}
		release, ok := b.reserve(claims.UserID, c.ClientIP())
		if !ok {
			c.Header("Retry-After", "1")
			c.Abort()
			httpresponse.Write(c, http.StatusTooManyRequests, "upload concurrency limit exceeded")
			return
		}
		defer release()
		c.Next()
	}
}

func (b *uploadConcurrency) reserve(userID int64, sourceIP string) (func(), bool) {
	b.mu.Lock()
	if b.total >= b.cfg.MaxConcurrent ||
		b.byUser[userID] >= b.cfg.MaxConcurrentPerUser ||
		b.byIP[sourceIP] >= b.cfg.MaxConcurrentPerIP {
		b.mu.Unlock()
		return nil, false
	}
	b.total++
	b.byUser[userID]++
	b.byIP[sourceIP]++
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.total--
			b.byUser[userID]--
			if b.byUser[userID] == 0 {
				delete(b.byUser, userID)
			}
			b.byIP[sourceIP]--
			if b.byIP[sourceIP] == 0 {
				delete(b.byIP, sourceIP)
			}
		})
	}, true
}
