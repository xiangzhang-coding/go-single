package model

import "time"

// Role 用户角色：普通用户与管理员。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User 用户账号：登录凭证 + 角色。
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// IsAdmin 是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }
