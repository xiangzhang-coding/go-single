package file_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
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
	type reserveResult struct {
		key string
		err error
	}
	results := make(chan reserveResult, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("users/%d/file/20260822/%032x.txt", ownerID, i+1)
			requestID := fmt.Sprintf("request-%d", i)
			results <- reserveResult{key: key, err: usage.Reserve(context.Background(), ownerID, requestID, key, 10, 30, 3)}
		}(i)
	}
	wg.Wait()
	close(results)

	allowed, rejected := 0, 0
	var reserved []reserveResult
	for result := range results {
		switch {
		case result.err == nil:
			allowed++
			reserved = append(reserved, result)
		case errors.Is(result.err, file.ErrQuotaExceeded):
			rejected++
		default:
			require.NoError(t, result.err)
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
	recent, err := usage.ListPending(context.Background(), 10*time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, recent, "10 分钟保护期内的在途上传不得被对账清理")

	pending, err := usage.ListPending(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Len(t, pending, 3)
	require.NoError(t, usage.Commit(context.Background(), ownerID, reserved[0].key))
	for _, reservation := range reserved[1:] {
		require.NoError(t, usage.Release(context.Background(), ownerID, reservation.key, 10))
	}
	pending, err = usage.ListPending(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, db.Raw(
		"SELECT used_bytes, object_count FROM user_upload_usage WHERE user_id = ?", ownerID,
	).Scan(&stored).Error)
	require.Equal(t, int64(10), stored.UsedBytes)
	require.Equal(t, int64(1), stored.ObjectCount)
}

func TestUploadIdempotencyReplaysThroughRealMySQLAndMinIO(t *testing.T) {
	db, err := openUsageTestDB()
	testsupport.RequireDependency(t, "MySQL", err)
	username := fmt.Sprintf("upl_%d", time.Now().UnixNano())
	require.NoError(t, db.Exec(
		`INSERT INTO users (username, password_hash, role) VALUES (?, ?, 'user')`, username,
		"$2a$10$YDcE3V.LXJpDdAcovEV/D.ZLd2pWN66gelFHvaxI0IHxnCs2yEYRq",
	).Error)
	var ownerID int64
	require.NoError(t, db.Raw("SELECT id FROM users WHERE username = ?", username).Scan(&ownerID).Error)
	t.Cleanup(func() { _ = db.Exec("DELETE FROM users WHERE id = ?", ownerID).Error })

	bucket := fmt.Sprintf("go-shop-upload-replay-%d", time.Now().UnixNano())
	cfg := file.MinIOConfig{
		Endpoint:  envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    bucket,
	}
	svc, err := file.NewMinIO(cfg, file.NewGORMUsage(db), file.QuotaConfig{MaxBytesPerUser: 1 << 20, MaxObjectsPerUser: 10})
	testsupport.RequireDependency(t, "MinIO", err)
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.RemoveBucket(context.Background(), bucket) })

	verifier := auth.NewJWT(auth.JWTConfig{Secret: "integration-test-secret", TTL: 2 * time.Hour})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	file.NewHandler(svc, verifier, nil, file.UploadConcurrencyConfig{}).RegisterRoutes(router.Group("/api"))
	env := &testEnv{router: router}
	requestID := fmt.Sprintf("real-replay-%d", time.Now().UnixNano())
	ownerToken := tokenFor(t, ownerID, 2*time.Hour)
	first := uploadFileKindWithRequestID(t, env, ownerToken, "avatar.png", png1x1, file.KindImage, requestID)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	second := uploadFileKindWithRequestID(t, env, ownerToken, "ignored.png", png1x1, file.KindImage, requestID)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var firstBody, secondBody map[string]any
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstBody))
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondBody))
	require.Equal(t, firstBody["url"], secondBody["url"])
	reference := firstBody["url"].(string)
	key := objectKey(t, env, reference)
	t.Cleanup(func() { _ = client.RemoveObject(context.Background(), bucket, key, minio.RemoveObjectOptions{}) })

	var usage struct {
		UsedBytes   int64
		ObjectCount int64
	}
	require.NoError(t, db.Table("user_upload_usage").Select("used_bytes", "object_count").
		Where("user_id = ?", ownerID).Scan(&usage).Error)
	require.Equal(t, int64(len(png1x1)), usage.UsedBytes)
	require.Equal(t, int64(1), usage.ObjectCount)
	var ledgerCount int64
	require.NoError(t, db.Table("user_upload_objects").Where("user_id = ?", ownerID).Count(&ledgerCount).Error)
	require.Equal(t, int64(1), ledgerCount)
	_, err = client.StatObject(context.Background(), bucket, key, minio.StatObjectOptions{})
	require.NoError(t, err)
}

func TestUploadReservationMigrationDownReleasesOnlyPendingQuota(t *testing.T) {
	dbName := fmt.Sprintf("go_shop_upload_migration_%d", time.Now().UnixNano())
	rootDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/", envOr("GO_SINGLE_MYSQL_ROOT_USER", "root"),
		envOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"), envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"),
		envOr("GO_SINGLE_MYSQL_PORT", "3306"))
	root, err := sql.Open("mysql", rootDSN)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	testsupport.RequireDependency(t, "MySQL", root.PingContext(ctx))
	_, err = root.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	_, err = root.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+dbName+".* TO '"+envOr("GO_SINGLE_MYSQL_USER", "shop")+"'@'%'")
	require.NoError(t, err)
	defer func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + dbName)
		_ = root.Close()
	}()

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		envOr("GO_SINGLE_MYSQL_USER", "shop"), envOr("GO_SINGLE_MYSQL_PASSWORD", "shop123"),
		envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"), envOr("GO_SINGLE_MYSQL_PORT", "3306"), dbName)
	m, err := migrate.New("file://../../../migrations", "mysql://"+dsn)
	require.NoError(t, err)
	defer m.Close()
	require.NoError(t, m.Migrate(24))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(
		`INSERT INTO users (username, password_hash, role) VALUES ('migration-upload-user', 'unused', 'user')`,
	).Error)
	var userID int64
	require.NoError(t, db.Table("users").Select("id").Where("username = 'migration-upload-user'").Scan(&userID).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO user_upload_usage (user_id, used_bytes, object_count) VALUES (?, 30, 2)", userID,
	).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO user_upload_objects (object_key, user_id, client_request_id, size, status)
		VALUES (?, ?, 'pending-request', 10, 'pending'), (?, ?, 'committed-request', 20, 'committed')`,
		fmt.Sprintf("users/%d/file/20260822/%032x.txt", userID, 1), userID,
		fmt.Sprintf("users/%d/file/20260822/%032x.txt", userID, 2), userID,
	).Error)

	require.NoError(t, m.Steps(-1))
	var usage struct {
		UsedBytes   int64
		ObjectCount int64
	}
	require.NoError(t, db.Table("user_upload_usage").Select("used_bytes", "object_count").
		Where("user_id = ?", userID).Scan(&usage).Error)
	require.Equal(t, int64(20), usage.UsedBytes)
	require.Equal(t, int64(1), usage.ObjectCount)
}

func openUsageTestDB() (*gorm.DB, error) {
	const dbName = "go_shop_test_file"
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
