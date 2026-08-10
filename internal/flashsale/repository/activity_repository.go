// Package repository 定义 flashsale 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

// ActivityRepository 秒杀活动数据访问接口。
type ActivityRepository interface {
	Create(ctx context.Context, a *model.Activity) error
	// Update 编辑活动（不含状态；状态经 UpdateStatus 走 上架/下架 端点）。
	Update(ctx context.Context, a *model.Activity) error
	GetByID(ctx context.Context, id int64) (*model.Activity, error)
	List(ctx context.Context) ([]model.Activity, error)
	// UpdateStatus 上架/下架状态迁移。
	UpdateStatus(ctx context.Context, id int64, status string) error
}

// Store 聚合活动仓储，作为 service 的构造入参。
type Store struct {
	Activities ActivityRepository
}
