package file_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

func TestGORMUsageConcurrentReservationsNeverExceedQuota(t *testing.T) {
	db, err := openUsageTestDB()
	testsupport.RequireDependency(t, "MySQL", err)

	username := fmt.Sprintf("quota_%d", time.Now().UnixNano())
	res := db.Exec(`INSERT INTO users (username, password_hash, role) VALUES (?, ?, 'user')`, username,
		"$2a$10$YDcE3V.LXJpDdAcovEV/D.ZLd2pWN66gelFHvaxI0IHxnCs2yEYRq")
	require.NoError(t, res.Error)
	var ownerID int64
	require.NoError(t, db.Raw("SELECT id FROM users WHERE username = ?", username).Scan(&ownerID).Error)
	t.Cleanup(func() { _ = db.Exec("DELETE FROM users WHERE id = ?", ownerID).Error })

	usage := file.NewGORMUsage(db)
	const attempts = 10
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- usage.Reserve(context.Background(), ownerID, 10, 30, 3)
		}()
	}
	wg.Wait()
	close(errs)

	allowed, rejected := 0, 0
	for reserveErr := range errs {
		switch {
		case reserveErr == nil:
			allowed++
		case errors.Is(reserveErr, file.ErrQuotaExceeded):
			rejected++
		default:
			require.NoError(t, reserveErr)
		}
	}
	require.Equal(t, 3, allowed)
	require.Equal(t, 7, rejected)

	var stored struct {
		UsedBytes   int64
		ObjectCount int64
	}
	require.NoError(t, db.Raw(
		"SELECT used_bytes, object_count FROM user_upload_usage WHERE user_id = ?", ownerID,
	).Scan(&stored).Error)
	require.Equal(t, int64(30), stored.UsedBytes)
	require.Equal(t, int64(3), stored.ObjectCount)
}

func openUsageTestDB() (*gorm.DB, error) {
	const dbName = "go_shop_test"
	rootDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/", envOr("GO_SINGLE_MYSQL_ROOT_USER", "root"),
		envOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"), envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"),
		envOr("GO_SINGLE_MYSQL_PORT", "3306"))
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := root.PingContext(ctx); err != nil {
		return nil, err
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+dbName); err != nil {
		return nil, err
	}
	if _, err := root.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+dbName+".* TO '"+envOr("GO_SINGLE_MYSQL_USER", "shop")+"'@'%'"); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		envOr("GO_SINGLE_MYSQL_USER", "shop"), envOr("GO_SINGLE_MYSQL_PASSWORD", "shop123"),
		envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"), envOr("GO_SINGLE_MYSQL_PORT", "3306"), dbName)
	m, err := migrate.New("file://../../../migrations", "mysql://"+dsn)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return nil, err
	}
	return gorm.Open(mysql.Open(dsn), &gorm.Config{})
}
