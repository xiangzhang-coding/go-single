package model

import "time"

// Role 用户角色：普通用户与管理员。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User 用户账号：登录凭证 + 角色 + 个人资料（昵称/头像）。
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`   // 昵称（可空，展示用；空时前端回退 username）
	AvatarURL string `json:"avatar_url"` // 头像 URL（可空，经 POST /api/files 上传后写入）
	// PasswordHash 不对外暴露。
	PasswordHash     string    `json:"-"`
	Role             string    `json:"role"`
	DefaultAddressID *int64    `json:"-"` // 默认地址指针（地址簿唯一默认由它保证，不对外暴露）
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// IsAdmin 是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }
