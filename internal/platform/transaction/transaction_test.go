package transaction_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

func TestGORMRejectsMissingHandle(t *testing.T) {
	_, err := transaction.GORM(nil)
	require.ErrorIs(t, err, transaction.ErrHandleRequired)
}

func TestWithinGORMRejectsMissingDatabase(t *testing.T) {
	err := transaction.WithinGORM(context.Background(), nil, func(*transaction.Handle) error { return nil })
	require.ErrorIs(t, err, transaction.ErrHandleRequired)
}
