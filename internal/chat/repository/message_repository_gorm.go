package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/chat/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// GORMConversationRepository 会话仓储的 GORM 实现。
type GORMConversationRepository struct {
	db *gorm.DB
}

// NewGORMConversation 基于已连接的 *gorm.DB 构造会话仓储。
func NewGORMConversation(db *gorm.DB) *GORMConversationRepository {
	return &GORMConversationRepository{db: db}
}

// WithinTx 开启跨表事务；fn 返回错误则整体回滚。
func (r *GORMConversationRepository) WithinTx(ctx context.Context, fn func(tx *transaction.Handle) error) error {
	return transaction.WithinGORM(ctx, r.db, fn)
}

// Ensure 会话不存在则创建；并发同键插入撞主键冲突属正常（已由他人创建）。
func (r *GORMConversationRepository) Ensure(ctx context.Context, handle *transaction.Handle, c *model.Conversation) error {
	tx, unwrapErr := transaction.GORM(handle)
	if unwrapErr != nil {
		return unwrapErr
	}
	err := tx.WithContext(ctx).Create(c).Error
	if err == nil || isDuplicateKey(err) {
		return nil
	}
	return err
}

func (r *GORMConversationRepository) GetByKey(ctx context.Context, key string) (*model.Conversation, error) {
	var c model.Conversation
	if err := r.db.WithContext(ctx).First(&c, "conversation_key = ?", key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func (r *GORMConversationRepository) ListByUser(ctx context.Context, userID int64, beforeLastMessageID int64, limit int) ([]model.Conversation, error) {
	q := r.db.WithContext(ctx).Where("user_a = ? OR user_b = ?", userID, userID)
	if beforeLastMessageID > 0 {
		q = q.Where("last_message_id < ?", beforeLastMessageID)
	}
	var list []model.Conversation
	err := q.Order("last_message_id DESC").Limit(limit).Find(&list).Error
	return list, err
}

func (r *GORMConversationRepository) TouchLastMessage(ctx context.Context, handle *transaction.Handle, key string, messageID int64) error {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Model(&model.Conversation{}).
		Where("conversation_key = ?", key).
		Update("last_message_id", gorm.Expr("GREATEST(last_message_id, ?)", messageID)).Error
}

var _ ConversationRepository = (*GORMConversationRepository)(nil)

// GORMMessageRepository 消息仓储的 GORM 实现。
type GORMMessageRepository struct {
	db *gorm.DB
}

// NewGORMMessage 基于已连接的 *gorm.DB 构造消息仓储。
func NewGORMMessage(db *gorm.DB) *GORMMessageRepository {
	return &GORMMessageRepository{db: db}
}

// Create 落库消息；(sender_id, client_request_id) 撞唯一键（含并发重放）
// 返回 ErrMessageDuplicate，其余错误原样返回。
func (r *GORMMessageRepository) Create(ctx context.Context, handle *transaction.Handle, m *model.Message) error {
	tx, unwrapErr := transaction.GORM(handle)
	if unwrapErr != nil {
		return unwrapErr
	}
	err := tx.WithContext(ctx).Create(m).Error
	if err == nil {
		return nil
	}
	if isDuplicateKey(err) {
		return ErrMessageDuplicate
	}
	return err
}

func (r *GORMMessageRepository) GetByID(ctx context.Context, id int64) (*model.Message, error) {
	var m model.Message
	if err := r.db.WithContext(ctx).First(&m, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *GORMMessageRepository) GetByIdempotencyKey(ctx context.Context, senderID int64, requestID string) (*model.Message, error) {
	var m model.Message
	if err := r.db.WithContext(ctx).
		Where("sender_id = ? AND client_request_id = ?", senderID, requestID).
		First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *GORMMessageRepository) GetByIDs(ctx context.Context, ids []int64) (map[int64]model.Message, error) {
	out := make(map[int64]model.Message, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var list []model.Message
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&list).Error; err != nil {
		return nil, err
	}
	for i := range list {
		out[list[i].ID] = list[i]
	}
	return out, nil
}

func (r *GORMMessageRepository) CanAccessMedia(ctx context.Context, userID int64, reference string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Message{}).
		Where("url = ? AND (sender_id = ? OR recipient_id = ?)", reference, userID, userID).
		Limit(1).Count(&count).Error
	return count > 0, err
}

func (r *GORMMessageRepository) ListAfter(ctx context.Context, key string, afterID int64, limit int) ([]model.Message, error) {
	var list []model.Message
	err := r.db.WithContext(ctx).
		Where("conversation_key = ? AND id > ?", key, afterID).
		Order("id ASC").
		Limit(limit).Find(&list).Error
	return list, err
}

func (r *GORMMessageRepository) ListBefore(ctx context.Context, key string, beforeID int64, limit int) ([]model.Message, error) {
	var list []model.Message
	err := r.db.WithContext(ctx).
		Where("conversation_key = ? AND id < ?", key, beforeID).
		Order("id DESC").
		Limit(limit).Find(&list).Error
	return list, err
}

func (r *GORMMessageRepository) ListLatest(ctx context.Context, key string, limit int) ([]model.Message, error) {
	var list []model.Message
	err := r.db.WithContext(ctx).
		Where("conversation_key = ?", key).
		Order("id DESC").
		Limit(limit).Find(&list).Error
	return list, err
}

var _ MessageRepository = (*GORMMessageRepository)(nil)

// GORMReadStateRepository 已读游标仓储的 GORM 实现。
type GORMReadStateRepository struct {
	db *gorm.DB
}

// NewGORMReadState 基于已连接的 *gorm.DB 构造已读游标仓储。
func NewGORMReadState(db *gorm.DB) *GORMReadStateRepository {
	return &GORMReadStateRepository{db: db}
}

// MarkRead 只进不退：INSERT ... ON DUPLICATE KEY UPDATE 取最大者。
func (r *GORMReadStateRepository) MarkRead(ctx context.Context, userID int64, key string, messageID int64) error {
	return r.db.WithContext(ctx).Exec(`
		INSERT INTO conversation_reads (user_id, conversation_key, last_read_message_id)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE last_read_message_id = GREATEST(last_read_message_id, ?)`,
		userID, key, messageID, messageID).Error
}

// ListUnreadCounts 批量未读数：会话中发给我的且 id > 已读游标的消息数
// （LEFT JOIN 已读游标，未读过该会话视为游标 0）。
func (r *GORMReadStateRepository) ListUnreadCounts(ctx context.Context, userID int64, keys []string) (map[string]int64, error) {
	out := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := r.db.WithContext(ctx).Raw(`
		SELECT m.conversation_key, COUNT(*) AS cnt
		FROM messages m
		LEFT JOIN conversation_reads r
			ON r.conversation_key = m.conversation_key AND r.user_id = ?
		WHERE m.recipient_id = ? AND m.conversation_key IN (?)
			AND m.id > COALESCE(r.last_read_message_id, 0)
		GROUP BY m.conversation_key`,
		userID, userID, keys).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var cnt int64
		if err := rows.Scan(&key, &cnt); err != nil {
			return nil, err
		}
		out[key] = cnt
	}
	return out, rows.Err()
}

var _ ReadStateRepository = (*GORMReadStateRepository)(nil)

// isDuplicateKey MySQL 唯一键/主键冲突（1062）。
func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
