package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"

	"golang.org/x/crypto/bcrypt"

	"github.com/xiangzhang-coding/go-single/internal/platform/config"
)

const knownSeedAdminHash = "$2a$10$YDcE3V.LXJpDdAcovEV/D.ZLd2pWN66gelFHvaxI0IHxnCs2yEYRq"
const knownSeedAdminPassword = "admin123"

func listenAddress(server config.Server) string {
	return net.JoinHostPort(server.Host, strconv.Itoa(server.Port))
}

type seedAdminChecker interface {
	HasKnownSeedAdmin(context.Context) (bool, error)
}

type sqlSeedAdminChecker struct {
	db *sql.DB
}

func (c sqlSeedAdminChecker) HasKnownSeedAdmin(ctx context.Context) (bool, error) {
	var hash string
	err := c.db.QueryRowContext(ctx, `
		SELECT password_hash FROM users
		WHERE username = 'admin' AND role = 'admin'
		LIMIT 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return knownSeedAdminPasswordMatches(hash), nil
}

func knownSeedAdminPasswordMatches(hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(knownSeedAdminPassword)) == nil
}

func validateReleaseDatabase(ctx context.Context, mode string, checker seedAdminChecker) error {
	if mode != "release" {
		return nil
	}
	exists, err := checker.HasKnownSeedAdmin(ctx)
	if err != nil {
		return fmt.Errorf("检查演示管理员凭据: %w", err)
	}
	if exists {
		return fmt.Errorf("release 模式检测到迁移创建的演示管理员凭据；请先轮换 admin 密码或删除种子账号（见 docs/DEPLOYMENT.md）")
	}
	return nil
}
