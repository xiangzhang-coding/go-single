package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

func TestMigrationsUpgradeV17HistoryToLatest(t *testing.T) {
	cfg, err := config.LoadFrom("../../configs")
	require.NoError(t, err)
	migrationsPath, err := filepath.Abs("../../migrations")
	require.NoError(t, err)

	root, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		routerEnvOr("GO_SINGLE_MYSQL_ROOT_USER", "root"),
		routerEnvOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"),
		cfg.MySQL.Host, cfg.MySQL.Port,
	))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	rootPingCtx, rootPingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	testsupport.RequireDependency(t, "MySQL", root.PingContext(rootPingCtx))
	rootPingCancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	databaseName := "go_shop_mig17_" + suffix
	userName := "go_mig17_" + suffix
	password := "Mig17_" + suffix + "_A9"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, dropErr := root.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS `"+databaseName+"`"); dropErr != nil {
			t.Errorf("删除 migration 历史测试数据库: %v", dropErr)
		}
		if _, dropErr := root.ExecContext(cleanupCtx, "DROP USER IF EXISTS '"+userName+"'@'%'"); dropErr != nil {
			t.Errorf("删除 migration 历史测试用户: %v", dropErr)
		}
	})
	require.NoError(t, migrationHistoryExec(root, "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"))
	require.NoError(t, migrationHistoryExec(root, "CREATE USER '"+userName+"'@'%' IDENTIFIED BY '"+password+"'"))
	require.NoError(t, migrationHistoryExec(root, "GRANT ALL PRIVILEGES ON `"+databaseName+"`.* TO '"+userName+"'@'%'"))

	testCfg := *cfg
	testCfg.MySQL.User = userName
	testCfg.MySQL.Password = password
	testCfg.MySQL.Database = databaseName
	testCfg.Migrations.Path = migrationsPath

	migrator, err := migrate.New("file://"+migrationsPath, "mysql://"+testCfg.MySQL.DSN())
	require.NoError(t, err)
	t.Cleanup(func() {
		if migrator != nil {
			sourceCloseErr, databaseCloseErr := migrator.Close()
			if sourceCloseErr != nil {
				t.Errorf("关闭 migration source: %v", sourceCloseErr)
			}
			if databaseCloseErr != nil {
				t.Errorf("关闭 migration database: %v", databaseCloseErr)
			}
		}
	})
	require.NoError(t, migrator.Migrate(17))
	version, dirty, err := migrator.Version()
	require.NoError(t, err)
	require.Equal(t, uint(17), version)
	require.False(t, dirty)
	sourceErr, databaseErr := migrator.Close()
	require.NoError(t, sourceErr)
	require.NoError(t, databaseErr)
	migrator = nil

	db, err := sql.Open("mysql", testCfg.MySQL.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	fixturePingCtx, fixturePingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	testsupport.RequireDependency(t, "MySQL migration fixture", db.PingContext(fixturePingCtx))
	fixturePingCancel()
	insertMigrationV17History(t, db)

	require.NoError(t, runMigrations(&testCfg, zap.NewNop()))
	require.NoError(t, db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Equal(t, latestMigrationVersion(t, migrationsPath), version)
	require.False(t, dirty)

	assertMigration18History(t, db)
	assertMigration19Invariants(t, db, databaseName)
	assertMigrations20To22(t, db, databaseName)
	assertMigration23BinaryIdentifiers(t, db, databaseName)
	assertMigration24UploadLedger(t, db, databaseName)
}

func migrationHistoryExec(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}

func latestMigrationVersion(t *testing.T, migrationsPath string) uint {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(migrationsPath, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, matches)
	var latest uint64
	for _, match := range matches {
		prefix := strings.SplitN(filepath.Base(match), "_", 2)[0]
		version, parseErr := strconv.ParseUint(prefix, 10, 64)
		require.NoError(t, parseErr, match)
		if version > latest {
			latest = version
		}
	}
	return uint(latest)
}

func insertMigrationV17History(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`INSERT INTO users (id, username, password_hash, role) VALUES
			(1001, 'mig17_alice', 'unused', 'user'), (1002, 'mig17_bob', 'unused', 'user')`,
		`INSERT INTO friend_requests (id, from_user_id, to_user_id, status) VALUES
			(1101, 1001, 1002, 'pending')`,
		`INSERT INTO categories (id, name) VALUES (2001, 'migration-history-category')`,
		`INSERT INTO products (id, category_id, title, description, status) VALUES
			(2101, 2001, 'migration-history-product', 'historical migration fixture', 'on_sale')`,
		`INSERT INTO skus (id, product_id, specs, price, stock) VALUES
			(2201, 2101, '{"color":"blue"}', 2500, 20)`,
		`INSERT INTO flashsale_activities
			(id, sku_id, title, price, stock, per_user_limit, status, start_at, end_at) VALUES
			(2301, 2201, 'migration-history-flashsale', 1900, 8, 3, 'on_sale',
			 CURRENT_TIMESTAMP(3) - INTERVAL 1 HOUR, CURRENT_TIMESTAMP(3) + INTERVAL 1 DAY)`,
		`INSERT INTO orders
			(order_no, user_id, order_type, status, activity_id, total_amount, discount_amount, pay_amount,
			 receiver, phone, province, city, district, detail, expire_at, user_activity_key) VALUES
			('hist-normal', 1001, 'normal', 'pending_payment', NULL, 3000, 500, 2500,
			 'Alice', '13800138000', 'Guangdong', 'Shenzhen', 'Nanshan', 'History Road 1',
			 CURRENT_TIMESTAMP(3) + INTERVAL 30 MINUTE, NULL),
			('hist-linked', 1001, 'seckill', 'pending_payment', 2301, 1900, 0, 1900,
			 'Alice', '13800138000', 'Guangdong', 'Shenzhen', 'Nanshan', 'History Road 1',
			 CURRENT_TIMESTAMP(3) + INTERVAL 10 MINUTE, '1001:2301'),
			('hist-fallback', 1002, 'seckill', 'pending_payment', 2301, 1900, 0, 1900,
			 'Bob', '13900139000', 'Guangdong', 'Shenzhen', 'Futian', 'History Road 2',
			 CURRENT_TIMESTAMP(3) + INTERVAL 10 MINUTE, '1002:2301')`,
		`INSERT INTO order_items
			(id, order_no, sku_id, product_id, title, specs, price, quantity, subtotal) VALUES
			(3101, 'hist-normal', 2201, 2101, 'migration-history-product', '{"color":"blue"}', 1500, 2, 3000),
			(3102, 'hist-linked', 2201, 2101, 'migration-history-product', '{"color":"blue"}', 1900, 1, 1900),
			(3103, 'hist-fallback', 2201, 2101, 'migration-history-product', '{"color":"blue"}', 1900, 1, 1900)`,
		`INSERT INTO flashsale_pre_deductions
			(id, user_id, activity_id, order_no, quantity, status, publish_attempts, rollback_attempts,
			 last_error, legacy, ordered_at, reservation_released_at) VALUES
			(5001, 1001, 2301, 'hist-linked', 1, 'ordered', 1, 0, '', 0, CURRENT_TIMESTAMP(3), CURRENT_TIMESTAMP(3)),
			(5002, 1001, 2301, NULL, 1, 'pending_publish', 0, 0, '', 0, NULL, NULL)`,
		`INSERT INTO payments (id, payment_id, order_no, user_id, amount, result) VALUES
			(7001, 'Case-Payment', 'hist-normal', 1001, 2500, 'fail')`,
		`INSERT INTO conversations (conversation_key, user_a, user_b, last_message_id) VALUES
			('1001:1002', 1001, 1002, 0)`,
		`INSERT INTO messages
			(id, conversation_key, sender_id, recipient_id, type, content, url, client_request_id) VALUES
			(6001, '1001:1002', 1001, 1002, 'text', 'historical message', '', 'Case-Message')`,
		`UPDATE conversations SET last_message_id = 6001 WHERE conversation_key = '1001:1002'`,
	}
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		_, err = tx.Exec(statement)
		require.NoError(t, err, statement)
	}
	require.NoError(t, tx.Commit())
}

func assertMigration18History(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT id, client_request_id, sku_id, price, purchase_slot
		FROM flashsale_pre_deductions ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	type preDeduction struct {
		id, skuID, price, slot int64
		requestID              string
	}
	var got []preDeduction
	for rows.Next() {
		var row preDeduction
		require.NoError(t, rows.Scan(&row.id, &row.requestID, &row.skuID, &row.price, &row.slot))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []preDeduction{
		{id: 5001, requestID: "migrated-r05-5001", skuID: 2201, price: 1900, slot: 5001},
		{id: 5002, requestID: "migrated-r05-5002", skuID: 2201, price: 1900, slot: 5002},
	}, got)

	for _, want := range []struct {
		orderNo string
		slot    sql.NullInt64
		key     sql.NullString
	}{
		{orderNo: "hist-normal"},
		{orderNo: "hist-linked", slot: sql.NullInt64{Int64: 5001, Valid: true}, key: sql.NullString{String: "1001:2301:5001", Valid: true}},
		{orderNo: "hist-fallback", slot: sql.NullInt64{Int64: 1, Valid: true}, key: sql.NullString{String: "1002:2301:1", Valid: true}},
	} {
		var slot sql.NullInt64
		var key sql.NullString
		require.NoError(t, db.QueryRow(`SELECT purchase_slot, user_activity_key FROM orders WHERE order_no = ?`, want.orderNo).Scan(&slot, &key))
		require.Equal(t, want.slot, slot, want.orderNo)
		require.Equal(t, want.key, key, want.orderNo)
	}
}

func assertMigration19Invariants(t *testing.T, db *sql.DB, databaseName string) {
	t.Helper()
	var historicalRequestIDs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM orders
		WHERE order_no LIKE 'hist-%' AND client_request_id IS NOT NULL`).Scan(&historicalRequestIDs))
	require.Zero(t, historicalRequestIDs)

	var checkCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND CONSTRAINT_TYPE = 'CHECK' AND CONSTRAINT_NAME IN (
		'chk_orders_amount_range', 'chk_orders_amount_relation', 'chk_skus_price_max',
		'chk_flashsale_activities_price_max', 'chk_flashsale_pre_deductions_price_max',
		'chk_order_items_price_max', 'chk_order_items_quantity', 'chk_order_items_subtotal')`, databaseName).Scan(&checkCount))
	require.Equal(t, 8, checkCount)

	_, err := db.Exec(`UPDATE orders SET pay_amount = pay_amount + 1 WHERE order_no = 'hist-normal'`)
	requireMySQLConstraintError(t, err)
	_, err = db.Exec(`UPDATE order_items SET subtotal = subtotal + 1 WHERE id = 3101`)
	requireMySQLConstraintError(t, err)
}

func assertMigrations20To22(t *testing.T, db *sql.DB, databaseName string) {
	t.Helper()
	for _, table := range []string{"user_upload_usage", "friend_pair_locks"} {
		var count int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, databaseName, table).Scan(&count))
		require.Equal(t, 1, count, table)
	}

	var constraintCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND CONSTRAINT_NAME IN (
		'fk_user_upload_usage_user', 'chk_friend_requests_status', 'chk_friend_pair_locks_order',
		'fk_friend_pair_locks_user_a', 'fk_friend_pair_locks_user_b')`, databaseName).Scan(&constraintCount))
	require.Equal(t, 5, constraintCount)
	var indexCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'friend_requests' AND INDEX_NAME IN (
		'idx_friend_requests_incoming_page', 'idx_friend_requests_outgoing_page',
		'idx_friend_requests_outgoing_status_page')`, databaseName).Scan(&indexCount))
	require.Equal(t, 3, indexCount)

	_, err := db.Exec(`UPDATE friend_requests SET status = 'unknown' WHERE id = 1101`)
	requireMySQLConstraintError(t, err)
	_, err = db.Exec(`INSERT INTO friend_pair_locks (user_a, user_b) VALUES (1002, 1001)`)
	requireMySQLConstraintError(t, err)
	_, err = db.Exec(`INSERT INTO friend_pair_locks (user_a, user_b) VALUES (1001, 1002)`)
	require.NoError(t, err)
}

func assertMigration23BinaryIdentifiers(t *testing.T, db *sql.DB, databaseName string) {
	t.Helper()
	for _, column := range []struct{ table, name string }{
		{table: "flashsale_pre_deductions", name: "client_request_id"},
		{table: "messages", name: "client_request_id"},
		{table: "payments", name: "payment_id"},
	} {
		var collation string
		require.NoError(t, db.QueryRow(`SELECT COLLATION_NAME FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			databaseName, column.table, column.name).Scan(&collation))
		require.Equal(t, "utf8mb4_0900_bin", collation, column.table+"."+column.name)
	}
	var paymentID, messageRequestID string
	require.NoError(t, db.QueryRow(`SELECT payment_id FROM payments WHERE id = 7001`).Scan(&paymentID))
	require.NoError(t, db.QueryRow(`SELECT client_request_id FROM messages WHERE id = 6001`).Scan(&messageRequestID))
	require.Equal(t, "Case-Payment", paymentID)
	require.Equal(t, "Case-Message", messageRequestID)
	_, err := db.Exec(`INSERT INTO payments (payment_id, order_no, user_id, amount, result)
		VALUES ('case-payment', 'hist-normal', 1001, 2500, 'fail')`)
	require.NoError(t, err, "binary payment identities must preserve case")
	_, err = db.Exec(`INSERT INTO messages
		(conversation_key, sender_id, recipient_id, type, content, url, client_request_id)
		VALUES ('1001:1002', 1001, 1002, 'text', 'case probe', '', 'case-message')`)
	require.NoError(t, err, "binary message identities must preserve case")
}

func assertMigration24UploadLedger(t *testing.T, db *sql.DB, databaseName string) {
	t.Helper()
	var objectCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM user_upload_objects`).Scan(&objectCount))
	require.Zero(t, objectCount, "migration must not invent historical object reservations")

	var objectCharset, objectCollation, requestType sql.NullString
	require.NoError(t, db.QueryRow(`SELECT CHARACTER_SET_NAME, COLLATION_NAME
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'user_upload_objects'
		AND COLUMN_NAME = 'object_key'`, databaseName).Scan(&objectCharset, &objectCollation))
	require.Equal(t, sql.NullString{String: "ascii", Valid: true}, objectCharset)
	require.Equal(t, sql.NullString{String: "ascii_bin", Valid: true}, objectCollation)
	require.NoError(t, db.QueryRow(`SELECT DATA_TYPE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'user_upload_objects' AND COLUMN_NAME = 'client_request_id'`,
		databaseName).Scan(&requestType))
	require.Equal(t, sql.NullString{String: "varbinary", Valid: true}, requestType)

	_, err := db.Exec(`INSERT INTO user_upload_objects (object_key, user_id, client_request_id, size)
		VALUES ('users/1001/file/Case.txt', 1001, 'Upload-Key', 10),
		       ('users/1001/file/case.txt', 1001, 'upload-key', 20)`)
	require.NoError(t, err, "binary object and request identities must preserve case")
	_, err = db.Exec(`INSERT INTO user_upload_objects (object_key, user_id, client_request_id, size)
		VALUES ('users/1001/file/duplicate.txt', 1001, 'Upload-Key', 1)`)
	var mysqlErr *mysqldriver.MySQLError
	require.ErrorAs(t, err, &mysqlErr)
	require.Equal(t, uint16(1062), mysqlErr.Number)
}

func requireMySQLConstraintError(t *testing.T, err error) {
	t.Helper()
	var mysqlErr *mysqldriver.MySQLError
	require.ErrorAs(t, err, &mysqlErr)
	require.Equal(t, uint16(3819), mysqlErr.Number)
}
