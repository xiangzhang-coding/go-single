package health

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeDB struct{ err error }

func (f *fakeDB) PingContext(context.Context) error { return f.err }

type fakeDep struct{ err error }

func (f *fakeDep) Ping(context.Context) error { return f.err }
func (f *fakeDep) Close() error               { return nil }

func TestCheckAllOK(t *testing.T) {
	h := &Checker{MySQL: &fakeDB{}, Cache: &fakeDep{}, MQ: &fakeDep{}}

	res := h.Check(context.Background())
	require.Equal(t, "ok", res.Status)
	require.Equal(t, map[string]string{"mysql": "ok", "redis": "ok", "mq": "ok"}, res.Checks)
}

func TestCheckDegraded(t *testing.T) {
	h := &Checker{
		MySQL: &fakeDB{err: errors.New("conn refused")},
		Cache: &fakeDep{},
		MQ:    &fakeDep{err: errors.New("unreachable")},
	}

	res := h.Check(context.Background())
	require.Equal(t, "degraded", res.Status)
	require.Contains(t, res.Checks["mysql"], "unreachable")
	require.Equal(t, "ok", res.Checks["redis"])
	require.Contains(t, res.Checks["mq"], "unreachable")
}

func TestCheckAllDown(t *testing.T) {
	h := &Checker{
		MySQL: &fakeDB{err: errors.New("x")},
		Cache: &fakeDep{err: errors.New("x")},
		MQ:    &fakeDep{err: errors.New("x")},
	}

	res := h.Check(context.Background())
	require.Equal(t, "degraded", res.Status)
	for _, v := range res.Checks {
		require.Contains(t, v, "unreachable")
	}
}
