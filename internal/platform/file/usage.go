package file

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var (
	// ErrQuotaExceeded means a legal upload would exceed the user's storage budget.
	ErrQuotaExceeded = errors.New("upload quota exceeded")
	// ErrQuotaConfig means upload quota enforcement was not configured safely.
	ErrQuotaConfig               = errors.New("invalid upload quota config")
	ErrUploadReservationNotFound = errors.New("upload reservation not found")
	ErrUploadCommitUncertain     = errors.New("upload reservation commit outcome uncertain")
	ErrUploadRequestExists       = errors.New("upload request already exists")
)

const (
	UploadStatusPending   = "pending"
	UploadStatusCommitted = "committed"
)

// QuotaConfig bounds both stored bytes and object count per user.
type QuotaConfig struct {
	MaxBytesPerUser   int64
	MaxObjectsPerUser int64
}

// UsageStore atomically reserves durable storage budget before object upload.
type UsageStore interface {
	Reserve(ctx context.Context, ownerID int64, requestID, objectKey string, size, maxBytes, maxObjects int64) error
	Commit(ctx context.Context, ownerID int64, objectKey string) error
	Release(ctx context.Context, ownerID int64, objectKey string, size int64) error
	ListPending(ctx context.Context, minAge time.Duration, limit int) ([]UploadReservation, error)
	GetByRequestID(ctx context.Context, ownerID int64, requestID string) (*UploadReservation, error)
}

type UploadReservation struct {
	OwnerID   int64
	RequestID string
	ObjectKey string
	Size      int64
	Status    string
	CreatedAt time.Time
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
func (s *GORMUsage) Reserve(ctx context.Context, ownerID int64, requestID, objectKey string, size, maxBytes, maxObjects int64) error {
	if ownerID <= 0 || size <= 0 || size > maxBytes || maxObjects < 1 {
		return ErrQuotaExceeded
	}
	if err := s.db.WithContext(ctx).Exec(
		"INSERT IGNORE INTO user_upload_usage (user_id, used_bytes, object_count) VALUES (?, 0, 0)", ownerID,
	).Error; err != nil {
		return fmt.Errorf("initialize upload usage: %w", err)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(
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
		if err := tx.Exec(
			`INSERT INTO user_upload_objects (object_key, user_id, client_request_id, size, status) VALUES (?, ?, ?, ?, 'pending')`,
			objectKey, ownerID, requestID, size,
		).Error; err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				return ErrUploadRequestExists
			}
			return fmt.Errorf("record upload reservation: %w", err)
		}
		return nil
	})
}

func (s *GORMUsage) GetByRequestID(ctx context.Context, ownerID int64, requestID string) (*UploadReservation, error) {
	var reservation UploadReservation
	res := s.db.WithContext(ctx).Table("user_upload_objects").
		Select("user_id AS owner_id, client_request_id AS request_id, object_key, size, status, created_at").
		Where("user_id = ? AND client_request_id = ?", ownerID, requestID).
		Limit(1).Scan(&reservation)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	return &reservation, nil
}

func (s *GORMUsage) Commit(ctx context.Context, ownerID int64, objectKey string) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE user_upload_objects SET status = 'committed' WHERE object_key = ? AND user_id = ? AND status = 'pending'`,
		objectKey, ownerID,
	)
	if res.Error != nil {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		var status string
		confirmErr := s.db.WithContext(confirmCtx).Table("user_upload_objects").
			Select("status").Where("object_key = ? AND user_id = ?", objectKey, ownerID).
			Scan(&status).Error
		switch {
		case confirmErr == nil && status == "committed":
			return nil
		case confirmErr == nil && status == "pending":
			return res.Error
		default:
			return errors.Join(ErrUploadCommitUncertain, res.Error, confirmErr)
		}
	}
	if res.RowsAffected == 1 {
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Table("user_upload_objects").
		Where("object_key = ? AND user_id = ? AND status = 'committed'", objectKey, ownerID).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	return ErrUploadReservationNotFound
}

func (s *GORMUsage) Release(ctx context.Context, ownerID int64, objectKey string, size int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(
			`DELETE FROM user_upload_objects WHERE object_key = ? AND user_id = ? AND status = 'pending'`,
			objectKey, ownerID,
		)
		if res.Error != nil {
			return fmt.Errorf("delete upload reservation: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return nil
		}
		if err := tx.Exec(
			`UPDATE user_upload_usage
			 SET used_bytes = IF(used_bytes >= ?, used_bytes - ?, 0),
			     object_count = IF(object_count > 0, object_count - 1, 0)
			 WHERE user_id = ?`,
			size, size, ownerID,
		).Error; err != nil {
			return fmt.Errorf("release upload quota: %w", err)
		}
		return nil
	})
}

func (s *GORMUsage) ListPending(ctx context.Context, minAge time.Duration, limit int) ([]UploadReservation, error) {
	if limit < 1 {
		return []UploadReservation{}, nil
	}
	rows := make([]UploadReservation, 0, limit)
	err := s.db.WithContext(ctx).Table("user_upload_objects").
		Select("user_id AS owner_id, client_request_id AS request_id, object_key, size, status, created_at").
		Where("status = 'pending' AND created_at < CURRENT_TIMESTAMP(3) - INTERVAL ? MICROSECOND", minAge.Microseconds()).
		Order("created_at ASC").Limit(limit).Scan(&rows).Error
	return rows, err
}

var _ UsageStore = (*GORMUsage)(nil)
