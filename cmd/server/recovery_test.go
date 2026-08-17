package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
)

type blockingPreDeductionRecovery struct{}

func (blockingPreDeductionRecovery) RecoverPreDeductions(ctx context.Context) (flashsalesvc.RecoveryStats, error) {
	<-ctx.Done()
	return flashsalesvc.RecoveryStats{}, ctx.Err()
}

type blockingReservationCleanup struct{}

func (blockingReservationCleanup) CleanupOrderedReservations(ctx context.Context) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestStartupPreDeductionRecoveryHasTimeout(t *testing.T) {
	started := time.Now()
	_, err := recoverPreDeductionsAtStartup(blockingPreDeductionRecovery{}, 20*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}

func TestStartupReservationCleanupHasTimeout(t *testing.T) {
	started := time.Now()
	_, err := cleanupReservationsAtStartup(blockingReservationCleanup{}, 20*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}
