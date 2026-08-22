package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
)

type cronOrderSpy struct {
	ordersvc.Service
	calls int
}

func (s *cronOrderSpy) CancelExpired(context.Context) (int, int, error) {
	s.calls++
	return 2, 1, nil
}

type cronRecoverySpy struct {
	trace        *[]string
	periodic     int
	startupCalls int
}

func (s *cronRecoverySpy) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	s.periodic++
	*s.trace = append(*s.trace, "recover")
	return flashsalesvc.RecoveryStats{Published: 1}, nil
}

func (s *cronRecoverySpy) RecoverPreDeductionsAtStartup(context.Context) (flashsalesvc.RecoveryStats, error) {
	s.startupCalls++
	return flashsalesvc.RecoveryStats{}, nil
}

type cronGateSpy struct {
	trace   *[]string
	blocked bool
}

func (s *cronGateSpy) BlockPurchases() {
	s.blocked = true
	*s.trace = append(*s.trace, "block")
}

func (s *cronGateSpy) AllowPurchases() {
	s.blocked = false
	*s.trace = append(*s.trace, "allow")
}

func (s *cronGateSpy) PurchasesBlocked() bool { return s.blocked }

type cronCleanupSpy struct {
	trace *[]string
	calls int
}

func (s *cronCleanupSpy) CleanupOrderedReservations(context.Context) (int, error) {
	s.calls++
	*s.trace = append(*s.trace, "cleanup")
	return 1, nil
}

type cronUploadSpy struct{ calls int }

func (s *cronUploadSpy) ReconcilePendingUploads(context.Context) (int, error) {
	s.calls++
	return 1, nil
}

type cronSeckillCancellationSpy struct {
	expiredCalls int
	cancelCalls  int
}

func (s *cronSeckillCancellationSpy) CancelExpired(context.Context) (int, int, int, error) {
	s.expiredCalls++
	return 1, 0, 0, nil
}

func (s *cronSeckillCancellationSpy) Cancel(context.Context, int64, string) error {
	s.cancelCalls++
	return nil
}

type cronReconciliationSpy struct {
	activeCalls int
	endedCalls  int
}

func (s *cronReconciliationSpy) ReconcileActive(context.Context) ([]flashsalesvc.ReconcileWarning, error) {
	s.activeCalls++
	return nil, nil
}

func (s *cronReconciliationSpy) ReconcileEnded(context.Context) (int, error) {
	s.endedCalls++
	return 1, nil
}

type cronWiringFixture struct {
	jobs      []platformcron.Job
	order     *cronOrderSpy
	recovery  *cronRecoverySpy
	gate      *cronGateSpy
	cleanup   *cronCleanupSpy
	upload    *cronUploadSpy
	seckill   *cronSeckillCancellationSpy
	reconcile *cronReconciliationSpy
}

func newCronWiringFixture(t *testing.T) *cronWiringFixture {
	t.Helper()
	trace := make([]string, 0, 4)
	fixture := &cronWiringFixture{
		order:     &cronOrderSpy{},
		recovery:  &cronRecoverySpy{trace: &trace},
		gate:      &cronGateSpy{trace: &trace},
		cleanup:   &cronCleanupSpy{trace: &trace},
		upload:    &cronUploadSpy{},
		seckill:   &cronSeckillCancellationSpy{},
		reconcile: &cronReconciliationSpy{},
	}
	_, jobs, err := registerCron(zap.NewNop(), fixture.order, fixture.recovery, fixture.gate,
		fixture.cleanup, fixture.upload, fixture.seckill, fixture.reconcile)
	require.NoError(t, err)
	fixture.jobs = jobs
	return fixture
}

func registeredCronJob(t *testing.T, jobs []platformcron.Job, name string) platformcron.Job {
	t.Helper()
	var matches []platformcron.Job
	for _, job := range jobs {
		if job.Name == name {
			matches = append(matches, job)
		}
	}
	require.Len(t, matches, 1, "cron job %q must be registered exactly once", name)
	return matches[0]
}

func TestRegisterCronRegistersEveryJobWithExpectedSpec(t *testing.T) {
	fixture := newCronWiringFixture(t)
	want := map[string]string{
		"order-timeout-cancel":        "* * * * *",
		"flashsale-recovery":          "* * * * *",
		"upload-reservation-recovery": "* * * * *",
		"seckill-timeout-cancel":      "* * * * *",
		"flashsale-reconcile-active":  "0 * * * *",
		"flashsale-reconcile-ended":   "* * * * *",
	}
	got := make(map[string]string)
	for _, job := range fixture.jobs {
		require.NotEmpty(t, job.Name)
		require.NotNil(t, job.Fn)
		_, duplicate := got[job.Name]
		require.False(t, duplicate, "cron job %q registered more than once", job.Name)
		got[job.Name] = job.Spec
	}
	require.Equal(t, want, got)
}

func TestRegisteredCronJobsInvokeWiredDependencies(t *testing.T) {
	fixture := newCronWiringFixture(t)
	ctx := context.Background()

	require.NoError(t, registeredCronJob(t, fixture.jobs, "order-timeout-cancel").Fn(ctx))
	require.Equal(t, 1, fixture.order.calls)

	require.NoError(t, registeredCronJob(t, fixture.jobs, "seckill-timeout-cancel").Fn(ctx))
	require.Equal(t, 1, fixture.seckill.expiredCalls)
	require.Zero(t, fixture.seckill.cancelCalls)

	require.NoError(t, registeredCronJob(t, fixture.jobs, "upload-reservation-recovery").Fn(ctx))
	require.Equal(t, 1, fixture.upload.calls)

	require.NoError(t, registeredCronJob(t, fixture.jobs, "flashsale-reconcile-active").Fn(ctx))
	require.NoError(t, registeredCronJob(t, fixture.jobs, "flashsale-reconcile-ended").Fn(ctx))
	require.Equal(t, 1, fixture.reconcile.activeCalls)
	require.Equal(t, 1, fixture.reconcile.endedCalls)

	require.NoError(t, registeredCronJob(t, fixture.jobs, "flashsale-recovery").Fn(ctx))
	require.Equal(t, 1, fixture.recovery.periodic)
	require.Zero(t, fixture.recovery.startupCalls)
	require.Equal(t, 1, fixture.cleanup.calls)
	require.Equal(t, []string{"block", "recover", "cleanup", "allow"}, *fixture.recovery.trace)
	require.False(t, fixture.gate.PurchasesBlocked())
}
