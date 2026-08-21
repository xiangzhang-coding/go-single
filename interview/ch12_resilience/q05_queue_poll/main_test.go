package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskStatusesPublishesOrderedLifecycle(t *testing.T) {
	updates := taskStatuses(0)

	require.Equal(t, "pending_order", <-updates)
	require.Equal(t, "ordered", <-updates)
	_, open := <-updates
	require.False(t, open)
}
