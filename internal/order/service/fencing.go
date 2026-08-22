package service

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

func productIDsFromSnapshots(snapshots []lineSnapshot) []int64 {
	ids := make([]int64, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.productID)
	}
	return ids
}

func productIDsFromItems(items []model.OrderItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ProductID)
	}
	return ids
}

func (s *orderService) productIDsForCreate(ctx context.Context, userID int64, p *CreateParams) ([]int64, error) {
	if p.FromCart {
		items, err := s.cart.ListItems(ctx, userID)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, ErrCartEmpty
		}
		ids := make([]int64, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ProductID)
		}
		return ids, nil
	}

	lines := directLines(p)
	ids := make([]int64, 0, len(lines))
	for _, line := range lines {
		sku, err := s.products.GetSKU(ctx, line.skuID)
		if err != nil {
			return nil, translateProductError(err)
		}
		if sku == nil {
			return nil, ErrSKUNotFound
		}
		ids = append(ids, sku.ProductID)
	}
	return ids, nil
}

func mutationsCoverProducts(mutations []productDetailMutation, productIDs []int64) bool {
	fenced := make(map[int64]struct{}, len(mutations))
	for _, mutation := range mutations {
		fenced[mutation.productID] = struct{}{}
	}
	for _, productID := range productIDs {
		if _, ok := fenced[productID]; !ok {
			return false
		}
	}
	return true
}

type productDetailMutation struct {
	productID int64
	token     string
}

func (s *orderService) beginProductDetailMutations(ctx context.Context, productIDs []int64) ([]productDetailMutation, error) {
	seen := make(map[int64]struct{}, len(productIDs))
	begun := make([]productDetailMutation, 0, len(productIDs))
	for _, productID := range productIDs {
		if _, exists := seen[productID]; exists {
			continue
		}
		seen[productID] = struct{}{}
		token, err := s.products.BeginDetailMutation(ctx, productID)
		if err != nil {
			s.finishProductDetailMutations(ctx, begun)
			return nil, err
		}
		begun = append(begun, productDetailMutation{productID: productID, token: token})
	}
	return begun, nil
}

func (s *orderService) finishProductDetailMutations(parent context.Context, mutations []productDetailMutation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), productDetailCleanupTimeout)
	defer cancel()
	for _, mutation := range mutations {
		s.products.FinishDetailMutation(ctx, mutation.productID, mutation.token)
	}
}
