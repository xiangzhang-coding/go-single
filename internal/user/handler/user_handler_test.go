package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRequiresAuthenticationRateLimits(t *testing.T) {
	require.Panics(t, func() { New(nil, nil, nil) })
}
