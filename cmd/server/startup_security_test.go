package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/xiangzhang-coding/go-single/internal/platform/config"
)

type stubSeedAdminChecker struct {
	exists bool
	err    error
	called bool
}

func (s *stubSeedAdminChecker) HasKnownSeedAdmin(context.Context) (bool, error) {
	s.called = true
	return s.exists, s.err
}

func TestValidateReleaseDatabaseSkipsNonReleaseModes(t *testing.T) {
	for _, mode := range []string{"debug", "test"} {
		t.Run(mode, func(t *testing.T) {
			checker := &stubSeedAdminChecker{exists: true}
			require.NoError(t, validateReleaseDatabase(context.Background(), mode, checker))
			require.False(t, checker.called)
		})
	}
}

func TestValidateReleaseDatabaseRejectsKnownSeedAdmin(t *testing.T) {
	checker := &stubSeedAdminChecker{exists: true}

	err := validateReleaseDatabase(context.Background(), "release", checker)
	require.Error(t, err)
	require.True(t, checker.called)
	require.Contains(t, err.Error(), "docs/DEPLOYMENT.md")
	require.NotContains(t, err.Error(), "admin123")
	require.NotContains(t, err.Error(), "$2a$")
}

func TestKnownSeedAdminHashMatchesMigration(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/000002_users.up.sql")
	require.NoError(t, err)
	require.Contains(t, string(migration), knownSeedAdminHash)
}

func TestKnownSeedAdminPasswordMatchesNewBcryptSalt(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte(knownSeedAdminPassword), bcrypt.DefaultCost)
	require.NoError(t, err)
	require.True(t, knownSeedAdminPasswordMatches(string(hash)))
}

func TestValidateReleaseDatabaseFailsClosedWhenCheckFails(t *testing.T) {
	checker := &stubSeedAdminChecker{err: errors.New("database unavailable")}

	err := validateReleaseDatabase(context.Background(), "release", checker)
	require.ErrorContains(t, err, "检查演示管理员凭据")
}

func TestValidateReleaseDatabaseAcceptsRotatedAdmin(t *testing.T) {
	checker := &stubSeedAdminChecker{}
	require.NoError(t, validateReleaseDatabase(context.Background(), "release", checker))
}

func TestListenAddressIncludesConfiguredHost(t *testing.T) {
	require.Equal(t, "127.0.0.1:8080", listenAddress(config.Server{Host: "127.0.0.1", Port: 8080}))
	require.Equal(t, "[::1]:8080", listenAddress(config.Server{Host: "::1", Port: 8080}))
}
