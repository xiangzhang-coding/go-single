package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrderPurchaseSlotMarshalsAsDecimalString(t *testing.T) {
	slot := int64(9_007_199_254_740_993)
	body, err := json.Marshal(Order{OrderNo: "seckill-1", PurchaseSlot: &slot})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Equal(t, "9007199254740993", payload["purchase_slot"])
}
