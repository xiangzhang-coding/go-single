// Package model 定义 social 模块的领域模型：好友申请与好友关系。
package model

import "time"

// 好友申请状态：待处理 → 通过 / 拒绝。
const (
	RequestStatusPending  = "pending"
	RequestStatusAccepted = "accepted"
	RequestStatusRejected = "rejected"
)

// FriendRequest 好友申请：由 from 发起，对方（to）通过或拒绝。
// 每对 (from, to) 唯一一行：被拒后重新申请复用原行回到 pending。
type FriendRequest struct {
	ID         int64     `json:"id"`
	FromUserID int64     `json:"from_user_id"`
	ToUserID   int64     `json:"to_user_id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FriendRequestView 我的申请列表条目（含对方用户名，由 service 跨模块补齐）。
type FriendRequestView struct {
	FriendRequest
	PeerUsername string `json:"peer_username"`
}

// Friendship 好友关系：一对好友存方向相反的两行（(A,B) 与 (B,A)），双向可查。
type Friendship struct {
	ID        int64     `json:"-"`
	UserID    int64     `json:"user_id"`
	FriendID  int64     `json:"friend_id"`
	CreatedAt time.Time `json:"created_at"`
}

// FriendView 好友列表条目。
type FriendView struct {
	UserID   int64     `json:"user_id"`
	Username string    `json:"username"`
	Since    time.Time `json:"since"` // 成为好友的时间（双向两行同时写入）
}
