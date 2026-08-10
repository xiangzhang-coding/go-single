// Package repository 定义 social 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 好友关系数据访问全部经此接口：申请/通过/拒绝流程之外，
// "免申请互加"（互相关注即好友，backlog）只需替换 service 层流程，
// FriendshipRepository 保持不变，即插即换。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
)

// ErrRequestPairExists 同一对 (from, to) 已有申请行（唯一键冲突，含并发提交）。
var ErrRequestPairExists = errors.New("friend request pair already exists")

// ErrFriendshipExists 该方向好友关系已存在（唯一键冲突，含并发重复通过）。
var ErrFriendshipExists = errors.New("friendship already exists")

// FriendRequestRepository 好友申请数据访问接口。
type FriendRequestRepository interface {
	// Create 落库一条申请；同 (from, to) 已有行时返回 ErrRequestPairExists。
	Create(ctx context.Context, r *model.FriendRequest) error
	GetByID(ctx context.Context, id int64) (*model.FriendRequest, error)
	// GetByPair 按 (from, to) 取申请（每对唯一一行，判重/收敛用）。
	GetByPair(ctx context.Context, fromUserID, toUserID int64) (*model.FriendRequest, error)
	// UpdateStatus 状态迁移：pending→accepted/rejected；rejected→pending（重新申请）。
	UpdateStatus(ctx context.Context, id int64, status string) error
	// ListByUser 我的申请：scope=incoming（我收到的）/outgoing（我发出的）；
	// status 为空返回全部，否则按状态筛选；id 倒序（最新在前）。
	ListByUser(ctx context.Context, userID int64, scope, status string) ([]model.FriendRequest, error)
}

// FriendshipRepository 好友关系数据访问接口。
type FriendshipRepository interface {
	// CreatePair 写入双向两行（(a,b) 与 (b,a)）；任一行冲突返回 ErrFriendshipExists。
	CreatePair(ctx context.Context, userID, friendID int64) error
	// Exists 两人是否已是好友。
	Exists(ctx context.Context, userID, friendID int64) (bool, error)
	// ListByUser 我的好友关系行（id 倒序，最新好友在前）。
	ListByUser(ctx context.Context, userID int64) ([]model.Friendship, error)
}

// Store 聚合 social 模块各仓储，作为 service 的构造入参。
type Store struct {
	Requests    FriendRequestRepository
	Friendships FriendshipRepository
}
