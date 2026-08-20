// Package repository 定义 chat 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 会话/消息/已读游标三类数据访问全部经此接口；跨表事务由
// ConversationStore.WithinTx 开启，仓库方法以 tx 参数汇入同一事务。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/chat/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// ErrMessageDuplicate 同 (sender_id, client_request_id) 已有消息（幂等命中）。
var ErrMessageDuplicate = errors.New("message with same client_request_id already exists")

// TxRunner 事务运行器：开启跨表单事务（会话存在性 + 消息落库 + 最近消息推进）。
type TxRunner = transaction.Runner

// ConversationRepository 会话数据访问接口。
type ConversationRepository interface {
	// Ensure 会话不存在则创建（user_a < user_b），已存在保持原样（INSERT IGNORE 语义）。
	Ensure(ctx context.Context, tx *transaction.Handle, c *model.Conversation) error
	// GetByKey 按会话键取会话；不存在返回 (nil, nil)。
	GetByKey(ctx context.Context, key string) (*model.Conversation, error)
	// ListByUser 我的会话列表（游标分页：beforeLastMessageID 为上一页末位的
	// last_message_id，取更早的会话；0 取最近；latest_message_id 倒序，limit 条）。
	ListByUser(ctx context.Context, userID int64, beforeLastMessageID int64, limit int) ([]model.Conversation, error)
	// TouchLastMessage 推进会话最近消息 id（与消息落库同事务）。
	TouchLastMessage(ctx context.Context, tx *transaction.Handle, key string, messageID int64) error
}

// MessageRepository 消息数据访问接口。
type MessageRepository interface {
	// Create 落库消息；同 (sender_id, client_request_id) 已有行返回 ErrMessageDuplicate。
	Create(ctx context.Context, tx *transaction.Handle, m *model.Message) error
	// GetByID 按 id 取消息；不存在返回 (nil, nil)。
	GetByID(ctx context.Context, id int64) (*model.Message, error)
	// GetByIdempotencyKey 按 (sender_id, client_request_id) 取已有消息（幂等重放返回原消息）。
	GetByIdempotencyKey(ctx context.Context, senderID int64, requestID string) (*model.Message, error)
	// GetByIDs 批量取消息（会话列表预览用），返回 id → 消息。
	GetByIDs(ctx context.Context, ids []int64) (map[int64]model.Message, error)
	// CanAccessMedia 判断用户是否为引用该媒体的任一消息参与方。
	CanAccessMedia(ctx context.Context, userID int64, reference string) (bool, error)
	// ListAfter 游标分页：id > afterID 的消息（ASC，limit 条）。
	ListAfter(ctx context.Context, key string, afterID int64, limit int) ([]model.Message, error)
	// ListBefore 游标分页：id < beforeID 的消息（DESC，limit 条；调用方反转成正序）。
	ListBefore(ctx context.Context, key string, beforeID int64, limit int) ([]model.Message, error)
	// ListLatest 会话最近 limit 条消息（DESC；调用方反转成正序）。
	ListLatest(ctx context.Context, key string, limit int) ([]model.Message, error)
}

// ReadStateRepository 已读游标数据访问接口（每用户每会话一行）。
type ReadStateRepository interface {
	// MarkRead 幂等推进已读游标：仅当 newID 大于现值时更新（只进不退）；
	// 无行则插入（last_read_message_id = newID）。
	MarkRead(ctx context.Context, userID int64, key string, messageID int64) error
	// ListUnreadCounts 批量未读数：会话中发给我的且 id > 已读游标的消息数，
	// 返回 conversation_key → 未读数。
	ListUnreadCounts(ctx context.Context, userID int64, keys []string) (map[string]int64, error)
}

// Store 聚合 chat 模块各仓储，作为 service 的构造入参。
type Store struct {
	Conversations ConversationRepository
	Messages      MessageRepository
	Reads         ReadStateRepository
	Tx            TxRunner
}
