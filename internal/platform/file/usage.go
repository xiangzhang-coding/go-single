package file

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

var (
	// ErrQuotaExceeded means a legal upload would exceed the user's storage budget.
	ErrQuotaExceeded = errors.New("upload quota exceeded")
	// ErrQuotaConfig means upload quota enforcement was not configured safely.
	ErrQuotaConfig = errors.New("invalid upload quota config")
)

// QuotaConfig bounds both stored bytes and object count per user.
type QuotaConfig struct {
	MaxBytesPerUser   int64
	MaxObjectsPerUser int64
}

// UsageStore atomically reserves durable storage budget before object upload.
type UsageStore interface {
	Reserve(ctx context.Context, ownerID, size, maxBytes, maxObjects int64) error
	Release(ctx context.Context, ownerID, size int64) error
}

// GORMUsage stores per-user upload usage in MySQL.
type GORMUsage struct {
	db *gorm.DB
}

func NewGORMUsage(db *gorm.DB) *GORMUsage {
	return &GORMUsage{db: db}
}

// Reserve first creates an empty usage row, then performs one conditional
// UPDATE so concurrent processes cannot collectively exceed either limit.
func (s *GORMUsage) Reserve(ctx context.Context, ownerID, size, maxBytes, maxObjects int64) error {
	if ownerID <= 0 || size <= 0 || size > maxBytes || maxObjects < 1 {
		return ErrQuotaExceeded
	}
	if err := s.db.WithContext(ctx).Exec(
		"INSERT IGNORE INTO user_upload_usage (user_id, used_bytes, object_count) VALUES (?, 0, 0)", ownerID,
	).Error; err != nil {
		return fmt.Errorf("initialize upload usage: %w", err)
	}
	res := s.db.WithContext(ctx).Exec(
		`UPDATE user_upload_usage
		 SET used_bytes = used_bytes + ?, object_count = object_count + 1
		 WHERE user_id = ? AND used_bytes <= ? AND object_count < ?`,
		size, ownerID, maxBytes-size, maxObjects,
	)
	if res.Error != nil {
		return fmt.Errorf("reserve upload quota: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		return ErrQuotaExceeded
	}
	return nil
}

func (s *GORMUsage) Release(ctx context.Context, ownerID, size int64) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE user_upload_usage
		 SET used_bytes = IF(used_bytes >= ?, used_bytes - ?, 0),
		     object_count = IF(object_count > 0, object_count - 1, 0)
		 WHERE user_id = ?`,
		size, size, ownerID,
	)
	if res.Error != nil {
		return fmt.Errorf("release upload quota: %w", res.Error)
	}
	return nil
}

var _ UsageStore = (*GORMUsage)(nil)
