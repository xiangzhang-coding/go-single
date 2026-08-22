package service

import (
	"context"
	"strconv"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

func (s *flashsaleService) ListActivities(ctx context.Context) ([]model.ActivityView, error) {
	all, err := s.store.Activities.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]model.ActivityView, 0, len(all))
	for i := range all {
		a := all[i]
		v := model.ActivityView{Activity: a}
		switch {
		case !a.IsOnSale():
			v.State = model.ActivityStateOffSale
		case now.Before(a.StartAt):
			v.State = model.ActivityStateNotStarted
		case now.After(a.EndAt):
			v.State = model.ActivityStateEnded
		default:
			v.State = model.ActivityStateInProgress
		}
		s.attachSKU(ctx, &v)
		views = append(views, v)
	}
	return views, nil
}

// ListUserActivities 秒杀页活动列表（T23）：过滤 已上架 && 未结束，
// 剩余库存读 Redis 预扣余量（key 缺失/读失败降级配置库存，规格"缓存挂直查 DB"），
// 派生状态与服务端时间对齐（进行中 / 即将开始），并拼接 SKU 规格/原价与商品标题。
// 单个 SKU/商品读取失败仅留空摘要（活动仍展示，摘要缺失不影响抢购）。
func (s *flashsaleService) ListUserActivities(ctx context.Context) ([]model.ActivityView, error) {
	all, err := s.store.Activities.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]model.ActivityView, 0, len(all))
	for i := range all {
		a := all[i]
		if !a.IsOnSale() || now.After(a.EndAt) {
			continue
		}
		v := model.ActivityView{Activity: a}
		switch {
		case now.Before(a.StartAt):
			v.State = model.ActivityStateNotStarted
		default:
			v.State = model.ActivityStateInProgress
		}
		if remaining, err := s.cache.Get(ctx, stockKey(a.ID)); err == nil {
			if n, convErr := strconv.Atoi(remaining); convErr == nil {
				v.Stock = n
				// 库存余量 gauge 同步（T19c）：秒杀页浏览即刷新余量。
				s.metrics.SetSeckillStock(a.ID, n)
			}
		}
		s.attachSKU(ctx, &v)
		views = append(views, v)
	}
	return views, nil
}

// attachSKU 拼接 SKU 规格/原价与商品标题到活动视图（admin 列表与秒杀页共用）；
// 单个 SKU/商品读取失败仅留空摘要（活动仍展示，摘要缺失不影响抢购/管理）。
func (s *flashsaleService) attachSKU(ctx context.Context, v *model.ActivityView) {
	sku, skuErr := s.products.GetSKU(ctx, v.SKUID)
	if skuErr != nil {
		return
	}
	v.SKU = model.SKUView{
		ID:        sku.ID,
		ProductID: sku.ProductID,
		Specs:     sku.Specs,
		Price:     sku.Price,
	}
	if p, pErr := s.products.GetProduct(ctx, sku.ProductID); pErr == nil {
		v.ProductTitle = p.Title
	}
}
