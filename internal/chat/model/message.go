// Package model 定义 chat 模块的领域模型：会话与消息。
package model

import (
	"strconv"
	"time"
)

// 消息类型：text 携带文本内容；image/file 携带 platform/file 返回的托管引用。
const (
	MessageTypeText  = "text"
	MessageTypeImage = "image"
	MessageTypeFile  = "file"
)

// ConversationKey 由会话双方 id 构造：min(uidA,uidB):max(uidA,uidB) 有序用户对，
// 与谁先发消息无关，同一对用户唯一一个会话。
func ConversationKey(uidA, uidB int64) string {
	if uidA < uidB {
		return strconv.FormatInt(uidA, 10) + ":" + strconv.FormatInt(uidB, 10)
	}
	return strconv.FormatInt(uidB, 10) + ":" + strconv.FormatInt(uidA, 10)
}

// Message 单条用户间消息：落库并支持离线拉取（游标分页）。
// 字段约定：text 用 Content，image/file 用 URL（URL 为空串表示未填）。
type Message struct {
	ID              int64     `json:"id"`
	ConversationKey string    `json:"conversation_key" gorm:"column:conversation_key"`
	SenderID        int64     `json:"sender_id"`
	RecipientID     int64     `json:"recipient_id"`
	Type            string    `json:"type"`
	Content         string    `json:"content,omitempty"`
	URL             string    `json:"url,omitempty"`
	ClientRequestID *string   `json:"-" gorm:"column:client_request_id"`
	CreatedAt       time.Time `json:"created_at"`
}

// Conversation 会话：有序用户对 + 最近消息指针（列表排序与预览）。
// 双方 id 不直接暴露，由 service 按当前用户推导对方。
type Conversation struct {
	ConversationKey string    `json:"conversation_key" gorm:"column:conversation_key"`
	UserA           int64     `json:"-" gorm:"column:user_a"`
	UserB           int64     `json:"-" gorm:"column:user_b"`
	LastMessageID   int64     `json:"-" gorm:"column:last_message_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ConversationView 会话列表条目：对方用户（id + 用户名跨模块补齐）+
// 最近消息 + 我的未读数（recipient = 我 且 id > 已读游标）。
type ConversationView struct {
	ConversationKey string   `json:"conversation_key"`
	PeerUserID      int64    `json:"peer_user_id"`
	PeerUsername    string   `json:"peer_username"`
	LastMessage     *Message `json:"last_message,omitempty"`
	UnreadCount     int64    `json:"unread_count"`
}
