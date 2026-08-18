package service

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

func TestCheckedAmountArithmeticBoundaries(t *testing.T) {
	t.Run("multiplication maximum", func(t *testing.T) {
		got, err := checkedAmountMul(math.MaxInt64/99, 99)
		require.NoError(t, err)
		require.Equal(t, int64((math.MaxInt64/99)*99), got)

		_, err = checkedAmountMul(math.MaxInt64/99+1, 99)
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("multiplication wrapping to zero", func(t *testing.T) {
		_, err := checkedAmountMul(int64(1)<<62, 4)
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("addition maximum", func(t *testing.T) {
		got, err := checkedAmountAdd(math.MaxInt64-1, 1)
		require.NoError(t, err)
		require.Equal(t, int64(math.MaxInt64), got)

		_, err = checkedAmountAdd(math.MaxInt64-1, 2)
		require.ErrorIs(t, err, ErrInvalidInput)
	})
}

func TestValidateAmountConsistency(t *testing.T) {
	validOrder := model.Order{
		OrderType:      model.OrderTypeNormal,
		TotalAmount:    300,
		DiscountAmount: 50,
		PayAmount:      250,
	}
	validItems := []model.OrderItem{
		{Price: 100, Quantity: 1, Subtotal: 100},
		{Price: 100, Quantity: 2, Subtotal: 200},
	}
	require.NoError(t, validateAmountConsistency(&validOrder, validItems))

	tests := []struct {
		name  string
		order model.Order
		items []model.OrderItem
	}{
		{"pay relationship", model.Order{OrderType: model.OrderTypeNormal, TotalAmount: 300, DiscountAmount: 50, PayAmount: 251}, validItems},
		{"discount exceeds total", model.Order{OrderType: model.OrderTypeNormal, TotalAmount: 300, DiscountAmount: 301, PayAmount: 0}, validItems},
		{"item subtotal", validOrder, []model.OrderItem{{Price: 100, Quantity: 1, Subtotal: 99}, {Price: 100, Quantity: 2, Subtotal: 200}}},
		{"item sum", model.Order{OrderType: model.OrderTypeNormal, TotalAmount: 301, DiscountAmount: 51, PayAmount: 250}, validItems},
		{"seckill discount", model.Order{OrderType: model.OrderTypeSeckill, TotalAmount: 300, DiscountAmount: 50, PayAmount: 250}, validItems},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, validateAmountConsistency(&tc.order, tc.items), ErrInvalidInput)
		})
	}
}
