package limiter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

// AuthAttemptConfig is one authentication action's fixed-window budget.
type AuthAttemptConfig struct {
	PerIPMax      int
	PerAccountMax int
	Window        time.Duration
}

// AuthAttemptsConfig keeps login and registration budgets independent.
type AuthAttemptsConfig struct {
	Login    AuthAttemptConfig
	Register AuthAttemptConfig
}

// AuthAttempts limits public authentication work by source IP and account.
type AuthAttempts struct {
	store       cache.FixedWindowStore
	accountKeys AuthAccountKeyResolver
	cfg         AuthAttemptsConfig
}

// AuthAccountKeyResolver maps a submitted username to the database's exact
// equality identity, so account limits match the users unique index collation.
type AuthAccountKeyResolver interface {
	AuthenticationAccountKey(ctx context.Context, username string) (string, error)
}

// NewAuthAttempts validates and constructs the Redis-backed authentication limiter.
func NewAuthAttempts(store cache.FixedWindowStore, accountKeys AuthAccountKeyResolver, cfg AuthAttemptsConfig) (*AuthAttempts, error) {
	if store == nil || accountKeys == nil {
		return nil, fmt.Errorf("%w: auth limiter dependencies are nil", ErrConfig)
	}
	if err := validateAuthAttemptConfig("login", cfg.Login); err != nil {
		return nil, err
	}
	if err := validateAuthAttemptConfig("register", cfg.Register); err != nil {
		return nil, err
	}
	return &AuthAttempts{store: store, accountKeys: accountKeys, cfg: cfg}, nil
}

// AllowLogin consumes one login attempt from both source-IP and account budgets.
func (l *AuthAttempts) AllowLogin(ctx context.Context, ip, account string) (bool, error) {
	return l.allow(ctx, "login", l.cfg.Login, ip, account)
}

// AllowRegister consumes one registration attempt from both source-IP and account budgets.
func (l *AuthAttempts) AllowRegister(ctx context.Context, ip, account string) (bool, error) {
	return l.allow(ctx, "register", l.cfg.Register, ip, account)
}

func (l *AuthAttempts) allow(ctx context.Context, action string, cfg AuthAttemptConfig, ip, account string) (bool, error) {
	ipCount, err := l.store.IncrementFixedWindow(ctx, authAttemptKey(action, "ip", normalizeIP(ip)), cfg.Window)
	if err != nil {
		return false, err
	}
	if ipCount > int64(cfg.PerIPMax) {
		return false, nil
	}

	accountKey, err := l.accountKeys.AuthenticationAccountKey(ctx, account)
	if err != nil {
		return false, err
	}
	accountCount, err := l.store.IncrementFixedWindow(ctx, authAttemptKey(action, "account", accountKey), cfg.Window)
	if err != nil {
		return false, err
	}
	return accountCount <= int64(cfg.PerAccountMax), nil
}

func validateAuthAttemptConfig(action string, cfg AuthAttemptConfig) error {
	if cfg.PerIPMax < 1 || cfg.PerAccountMax < 1 || cfg.Window < time.Second {
		return fmt.Errorf("%w: auth %s per_ip=%d per_account=%d window=%s",
			ErrConfig, action, cfg.PerIPMax, cfg.PerAccountMax, cfg.Window)
	}
	return nil
}

func authAttemptKey(action, dimension, value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("auth:rl:%s:%s:%x", action, dimension, digest)
}

func normalizeIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if parsed := net.ParseIP(ip); parsed != nil {
		return parsed.String()
	}
	return ip
}
