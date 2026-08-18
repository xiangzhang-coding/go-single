package service

import (
	"fmt"
	"math"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
)

func checkedAmountMul(price int64, quantity int) (int64, error) {
	if price < 0 || quantity < 1 || quantity > 99 {
		return 0, fmt.Errorf("%w: invalid amount operands", ErrInvalidInput)
	}
	if price > math.MaxInt64/int64(quantity) {
		return 0, fmt.Errorf("%w: amount multiplication overflow", ErrInvalidInput)
	}
	return price * int64(quantity), nil
}

func checkedAmountAdd(total, subtotal int64) (int64, error) {
	if total < 0 || subtotal < 0 {
		return 0, fmt.Errorf("%w: negative amount", ErrInvalidInput)
	}
	if total > math.MaxInt64-subtotal {
		return 0, fmt.Errorf("%w: amount addition overflow", ErrInvalidInput)
	}
	return total + subtotal, nil
}

func validateAmountConsistency(order *model.Order, items []model.OrderItem) error {
	if order == nil || len(items) == 0 {
		return fmt.Errorf("%w: missing order amounts", ErrInvalidInput)
	}
	if order.TotalAmount < 0 || order.DiscountAmount < 0 || order.PayAmount < 0 ||
		order.DiscountAmount > order.TotalAmount ||
		order.PayAmount != order.TotalAmount-order.DiscountAmount {
		return fmt.Errorf("%w: inconsistent order amounts", ErrInvalidInput)
	}
	if order.OrderType == model.OrderTypeSeckill && order.DiscountAmount != 0 {
		return fmt.Errorf("%w: seckill discount is not allowed", ErrInvalidInput)
	}

	var itemTotal int64
	for i := range items {
		item := &items[i]
		if item.Price < 0 || item.Price > productmodel.MaxPriceCents {
			return fmt.Errorf("%w: invalid item price", ErrInvalidInput)
		}
		subtotal, err := checkedAmountMul(item.Price, item.Quantity)
		if err != nil {
			return err
		}
		if item.Subtotal != subtotal {
			return fmt.Errorf("%w: inconsistent item subtotal", ErrInvalidInput)
		}
		itemTotal, err = checkedAmountAdd(itemTotal, subtotal)
		if err != nil {
			return err
		}
	}
	if itemTotal != order.TotalAmount {
		return fmt.Errorf("%w: item total does not match order total", ErrInvalidInput)
	}
	return nil
}
