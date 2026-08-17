// Package repository 定义 flashsale 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

// TxRunner 为 flashsale 跨模块应用编排开启共享数据库事务。
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

// ActivityRepository 秒杀活动数据访问接口。
type ActivityRepository interface {
	Create(ctx context.Context, a *model.Activity) error
	// Update 编辑活动（不含状态；状态经 UpdateStatus 走 上架/下架 端点）。
	Update(ctx context.Context, a *model.Activity) error
	UpdateInTx(ctx context.Context, tx *gorm.DB, a *model.Activity) error
	GetByID(ctx context.Context, id int64) (*model.Activity, error)
	GetByIDForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*model.Activity, error)
	List(ctx context.Context) ([]model.Activity, error)
	// UpdateStatus 上架/下架状态迁移。
	UpdateStatus(ctx context.Context, id int64, status string) error
	// DeductStock 事务内条件扣减活动库存（stock >= quantity，防超卖）；
	// 返回是否扣减成功（库存不足返回 (false, nil)）。供秒杀异步落单在订单
	// 事务内扣减（MySQL 为落单事实源，与 Redis 预扣对账）。
	DeductStock(ctx context.Context, tx *gorm.DB, id int64, quantity int) (bool, error)
	// RestoreStock 事务内回补活动库存（stock + quantity）。供秒杀订单取消
	// 在订单事务内回补（与状态迁移同事务，MySQL 为落单事实源）。
	RestoreStock(ctx context.Context, tx *gorm.DB, id int64, quantity int) error
}

// Store 聚合活动仓储，作为 service 的构造入参。
type Store struct {
	Activities    ActivityRepository
	PreDeductions PreDeductionRepository
	Tx            TxRunner
}
