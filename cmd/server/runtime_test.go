package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

type blockingRuntimeMQ struct {
	started chan string
}

func (*blockingRuntimeMQ) Ping(context.Context) error                    { return nil }
func (*blockingRuntimeMQ) Close() error                                  { return nil }
func (*blockingRuntimeMQ) Publish(context.Context, string, []byte) error { return nil }
func (m *blockingRuntimeMQ) Consume(ctx context.Context, queue string, _ mq.MessageHandler) error {
	m.started <- queue
	<-ctx.Done()
	return ctx.Err()
}

type runtimeRecovery struct{}

func (runtimeRecovery) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{}, nil
}

type runtimeCleanup struct{}

func (runtimeCleanup) CleanupOrderedReservations(context.Context) (int, error) { return 0, nil }

func TestApplicationRuntimeStopsConsumersAndCron(t *testing.T) {
	client := &blockingRuntimeMQ{started: make(chan string, 2)}
	runtime := &applicationRuntime{
		log: zap.NewNop(), mq: client, cron: platformcron.New(zap.NewNop(), time.Second),
		recovery: runtimeRecovery{}, reservationCleanup: runtimeCleanup{},
		consumers: []consumerBinding{
			{queue: "main", name: "main", handler: func(context.Context, []byte) error { return nil }},
			{queue: "dead", name: "dead", handler: func(context.Context, []byte) error { return nil }},
		},
	}

	runtime.Start()
	require.ElementsMatch(t, []string{"main", "dead"}, []string{<-client.started, <-client.started})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.Stop(ctx))
}
