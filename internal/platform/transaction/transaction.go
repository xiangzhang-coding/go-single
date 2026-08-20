// Package transaction carries an opaque database transaction across module seams.
// Business modules may pass a Handle to repositories but cannot execute driver APIs.
package transaction

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

var (
	ErrHandleRequired = errors.New("transaction handle is required")
	ErrInvalidHandle  = errors.New("transaction handle has an unexpected driver")
)

// Handle is valid only for the duration of the Runner callback that supplied it.
type Handle struct {
	driver any
}

// Runner owns commit and rollback; callers only return an error from fn.
type Runner interface {
	WithinTx(ctx context.Context, fn func(*Handle) error) error
}

// WithinGORM adapts a GORM transaction to the shared opaque handle.
func WithinGORM(ctx context.Context, db *gorm.DB, fn func(*Handle) error) error {
	if db == nil {
		return ErrHandleRequired
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&Handle{driver: tx})
	})
}

// GORM unwraps a Handle inside a GORM repository adapter.
func GORM(handle *Handle) (*gorm.DB, error) {
	if handle == nil {
		return nil, ErrHandleRequired
	}
	db, ok := handle.driver.(*gorm.DB)
	if !ok || db == nil {
		return nil, ErrInvalidHandle
	}
	return db, nil
}
