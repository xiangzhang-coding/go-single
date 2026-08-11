// Package service 承载 chat 模块业务（REST 通道）：发送消息（text/image/file，
// 可幂等重试）、会话列表与消息列表（游标分页）、已读游标推进（离线消息上线可拉取）。
// 跨模块：接收方存在性经 user 服务、好友关系经 social 服务校验（仅好友可单聊）；
// 仅会话双方可访问会话与消息（owner 校验，防 IDOR）。
// 实时通道（T18）：Send 落库成功后经 MessageNotifier 端口通知（实现为 WS Hub，
// 在 cmd/server 组装），向在线接收方推送；断线消息不丢由落库 + REST 补拉兜底。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/chat/model"
	"github.com/xiangzhang-coding/go-single/internal/chat/repository"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrInvalidInput          = errors.New("invalid input")
	ErrSelfMessage           = errors.New("cannot message yourself")
	ErrRecipientNotFound     = errors.New("recipient user not found")
	ErrNotFriends            = errors.New("recipient is not your friend")
	ErrConversationNotFound  = errors.New("conversation not found")
	ErrConversationForbidden = errors.New("conversation does not belong to user")
	ErrMessageNotFound       = errors.New("message not found")
)

// 字段上限与分页限制（与迁移列宽一致）。
const (
	maxContentRunes = 2000 // content VARCHAR(2000)
	maxURLRunes     = 500  // url VARCHAR(500)
	maxKeyRunes     = 64   // conversation_key VARCHAR(64)
	// 消息列表分页：默认页大小与上限。
	defaultListLimit = 20
	maxListLimit     = 50
)

// UserService user 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP）。
type UserService interface {
	GetByID(ctx context.Context, id int64) (*usermodel.User, error)
}

// SocialService social 模块暴露的最小查询接口：好友关系校验（仅好友可单聊）。
type SocialService interface {
	AreFriends(ctx context.Context, userID, friendID int64) (bool, error)
}

// MessageNotifier 消息落库成功后的实时推送端口（T18 WS 通道）：
// 实现方为 platform/ws Hub（cmd/server 组装适配器），nil 表示不推送。
// 调用不阻塞发送链路：实现方须异步/非阻塞投递（缓冲满关闭连接，客户端 REST 补拉）。
type MessageNotifier interface {
	NotifyMessageSent(ctx context.Context, msg *model.Message)
}

// SendParams 发送消息参数。
type SendParams struct {
	ToUserID        int64
	Type            string // text / image / file
	Content         string // text 内容
	URL             string // image/file 的 MinIO URL
	ClientRequestID string // 空串 = 不要求幂等
}

// SendResult 发送结果；Idempotent=true 表示命中幂等键（同 client_request_id 重放），
// Message 为既有消息（与首次发送同一 id）。
type SendResult struct {
	Message    *model.Message
	Idempotent bool
}

// Service chat 模块业务接口。
type Service interface {
	// Send 发送消息：校验接收方存在与好友关系 → 单事务（会话存在性 + 消息落库 +
	// 会话最近消息推进）；client_request_id 幂等重放返回原消息；
	// 落库成功后（非幂等重放）经 MessageNotifier 实时推送在线接收方（T18）。
	Send(ctx context.Context, senderID int64, p SendParams) (*SendResult, error)
	// ListConversations 我的会话列表（游标分页，latest_message_id 倒序）：
	// 最近消息预览 + 对方用户名 + 未读数；返回列表与是否有更多。
	ListConversations(ctx context.Context, userID int64, beforeID int64, limit int) ([]model.ConversationView, bool, error)
	// ListMessages 会话消息游标分页（仅会话双方可访问）：
	// after_id 拉新（id > afterID，正序）/ before_id 拉旧（id < beforeID）/
	// 均缺省取最近 limit 条；返回正序消息与是否有更多。
	ListMessages(ctx context.Context, userID int64, key string, afterID, beforeID int64, limit int) ([]model.Message, bool, error)
	// MarkRead 推进我的已读游标到指定消息（只进不退）；仅会话双方可操作。
	MarkRead(ctx context.Context, userID int64, key string, messageID int64) error
}

type messageService struct {
	store    repository.Store
	users    UserService
	social   SocialService
	notifier MessageNotifier // nil = 不推送（调用方未接实时通道）
}

// New 构造消息服务；notifier 可传 nil（跳过实时推送）。
func New(store repository.Store, users UserService, social SocialService, notifier MessageNotifier) Service {
	return &messageService{store: store, users: users, social: social, notifier: notifier}
}

// Send 发送流程：参数校验 → 接收方存在校验（跨模块）→ 好友关系校验（跨模块）→
// 单事务（会话存在性 + 消息落库 + 会话最近消息推进）；幂等键撞唯一键时
// 回滚后查既有消息返回（Idempotent=true，200），否则 201。
func (s *messageService) Send(ctx context.Context, senderID int64, p SendParams) (*SendResult, error) {
	if senderID <= 0 || p.ToUserID <= 0 {
		return nil, fmt.Errorf("%w: invalid user id", ErrInvalidInput)
	}
	if senderID == p.ToUserID {
		return nil, ErrSelfMessage
	}
	if err := validateMessage(p); err != nil {
		return nil, err
	}

	u, err := s.users.GetByID(ctx, p.ToUserID)
	if err != nil {
		if errors.Is(err, usersvc.ErrUserNotFound) {
			return nil, ErrRecipientNotFound
		}
		return nil, err
	}
	if u == nil {
		return nil, ErrRecipientNotFound
	}
	friends, err := s.social.AreFriends(ctx, senderID, p.ToUserID)
	if err != nil {
		return nil, err
	}
	if !friends {
		return nil, ErrNotFriends
	}

	key := model.ConversationKey(senderID, p.ToUserID)
	userA, userB := senderID, p.ToUserID
	if userA > userB {
		userA, userB = userB, userA
	}
	msg := &model.Message{
		ConversationKey: key,
		SenderID:        senderID,
		RecipientID:     p.ToUserID,
		Type:            p.Type,
		Content:         p.Content,
		URL:             p.URL,
	}
	if p.ClientRequestID != "" {
		reqID := p.ClientRequestID
		msg.ClientRequestID = &reqID
	}

	err = s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
		conv := &model.Conversation{ConversationKey: key, UserA: userA, UserB: userB}
		if err := s.store.Conversations.Ensure(ctx, tx, conv); err != nil {
			return err
		}
		if err := s.store.Messages.Create(ctx, tx, msg); err != nil {
			return err
		}
		return s.store.Conversations.TouchLastMessage(ctx, tx, key, msg.ID)
	})
	if err != nil {
		// 幂等命中：同 (sender_id, client_request_id) 已有消息，返回既有消息。
		if errors.Is(err, repository.ErrMessageDuplicate) && p.ClientRequestID != "" {
			existing, getErr := s.store.Messages.GetByIdempotencyKey(ctx, senderID, p.ClientRequestID)
			if getErr != nil {
				return nil, getErr
			}
			if existing == nil {
				return nil, fmt.Errorf("%w: idempotency hit but message missing", ErrInvalidInput)
			}
			return &SendResult{Message: existing, Idempotent: true}, nil
		}
		return nil, err
	}
	// T18 实时通道：仅首次落库推送（幂等重放不重复推）；实现方为 WS Hub，
	// 非阻塞投递；接收方离线时为无操作（消息已落库，上线 REST 补拉）。
	if s.notifier != nil {
		s.notifier.NotifyMessageSent(ctx, msg)
	}
	return &SendResult{Message: msg, Idempotent: false}, nil
}

// validateMessage 按类型校验：text 必填 content（≤2000 字符）且 url 为空；
// image/file 必填 URL（http/https，≤500 字符）且 content 为空。
func validateMessage(p SendParams) error {
	contentLen := utf8.RuneCountInString(p.Content)
	urlLen := utf8.RuneCountInString(p.URL)
	switch p.Type {
	case model.MessageTypeText:
		if contentLen < 1 || contentLen > maxContentRunes {
			return fmt.Errorf("%w: text content must be 1-%d chars", ErrInvalidInput, maxContentRunes)
		}
		if p.URL != "" {
			return fmt.Errorf("%w: text message must not carry url", ErrInvalidInput)
		}
	case model.MessageTypeImage, model.MessageTypeFile:
		if urlLen < 1 || urlLen > maxURLRunes {
			return fmt.Errorf("%w: url must be 1-%d chars", ErrInvalidInput, maxURLRunes)
		}
		if !validURL(p.URL) {
			return fmt.Errorf("%w: url must be http(s) URL", ErrInvalidInput)
		}
		if p.Content != "" {
			return fmt.Errorf("%w: image/file message must not carry content", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: type must be text/image/file", ErrInvalidInput)
	}
	return nil
}

// validURL 图片/文件消息的 URL 须为 http/https（文件经 platform/file 上传后引用 URL）。
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ListConversations 我的会话（游标分页）：会话行（最新在前，limit+1 探更多）→
// 批量补对方用户名、最近消息预览与未读数（各一次查询）。
func (s *messageService) ListConversations(ctx context.Context, userID int64, beforeID int64, limit int) ([]model.ConversationView, bool, error) {
	if userID <= 0 {
		return nil, false, fmt.Errorf("%w: invalid user id", ErrInvalidInput)
	}
	if beforeID < 0 {
		return nil, false, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}
	if limit < 1 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	convs, err := s.store.Conversations.ListByUser(ctx, userID, beforeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(convs) > limit
	if hasMore {
		convs = convs[:limit]
	}
	views := make([]model.ConversationView, 0, len(convs))
	if len(convs) == 0 {
		return views, hasMore, nil
	}

	keys := make([]string, 0, len(convs))
	lastIDs := make([]int64, 0, len(convs))
	peerIDs := make([]int64, 0, len(convs))
	for i := range convs {
		keys = append(keys, convs[i].ConversationKey)
		lastIDs = append(lastIDs, convs[i].LastMessageID)
		peerID := convs[i].UserB
		if peerID == userID {
			peerID = convs[i].UserA
		}
		peerIDs = append(peerIDs, peerID)
	}
	names, err := usernames(ctx, s.users, peerIDs)
	if err != nil {
		return nil, false, err
	}
	lastMsgs, err := s.store.Messages.GetByIDs(ctx, lastIDs)
	if err != nil {
		return nil, false, err
	}
	unreads, err := s.store.Reads.ListUnreadCounts(ctx, userID, keys)
	if err != nil {
		return nil, false, err
	}

	for i := range convs {
		var last *model.Message
		if m, ok := lastMsgs[convs[i].LastMessageID]; ok {
			last = &m
		}
		views = append(views, model.ConversationView{
			ConversationKey: convs[i].ConversationKey,
			PeerUserID:      peerIDs[i],
			PeerUsername:    names[peerIDs[i]],
			LastMessage:     last,
			UnreadCount:     unreads[convs[i].ConversationKey],
		})
	}
	return views, hasMore, nil
}

// ListMessages 游标分页：after_id 与 before_id 互斥；
// 超出页大小 +1 探更多（has_more），旧消息方向反转成正序返回。
func (s *messageService) ListMessages(ctx context.Context, userID int64, key string, afterID, beforeID int64, limit int) ([]model.Message, bool, error) {
	if err := s.ensureAccessible(ctx, userID, key); err != nil {
		return nil, false, err
	}
	if afterID < 0 || beforeID < 0 {
		return nil, false, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}
	if afterID > 0 && beforeID > 0 {
		return nil, false, fmt.Errorf("%w: after_id and before_id are exclusive", ErrInvalidInput)
	}
	if limit < 1 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var (
		msgs []model.Message
		err  error
	)
	switch {
	case afterID > 0:
		msgs, err = s.store.Messages.ListAfter(ctx, key, afterID, limit+1)
	case beforeID > 0:
		msgs, err = s.store.Messages.ListBefore(ctx, key, beforeID, limit+1)
	default:
		msgs, err = s.store.Messages.ListLatest(ctx, key, limit+1)
	}
	if err != nil {
		return nil, false, err
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	// 旧消息方向（before/缺省取最近）为倒序，反转成正序展示。
	if afterID == 0 {
		reverse(msgs)
	}
	return msgs, hasMore, nil
}

// MarkRead 推进已读游标：会话可达（404/403）→ 消息存在且属于该会话 → 只进不退。
func (s *messageService) MarkRead(ctx context.Context, userID int64, key string, messageID int64) error {
	if err := s.ensureAccessible(ctx, userID, key); err != nil {
		return err
	}
	if messageID <= 0 {
		return fmt.Errorf("%w: invalid message id", ErrInvalidInput)
	}
	m, err := s.store.Messages.GetByID(ctx, messageID)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrMessageNotFound
	}
	if m.ConversationKey != key {
		return fmt.Errorf("%w: message does not belong to conversation", ErrInvalidInput)
	}
	return s.store.Reads.MarkRead(ctx, userID, key, messageID)
}

// ensureAccessible 会话可达性：键格式合法（400）→ 会话存在（404）→ 属于当前用户（403）。
func (s *messageService) ensureAccessible(ctx context.Context, userID int64, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	conv, err := s.store.Conversations.GetByKey(ctx, key)
	if err != nil {
		return err
	}
	if conv == nil {
		return ErrConversationNotFound
	}
	if conv.UserA != userID && conv.UserB != userID {
		return ErrConversationForbidden
	}
	return nil
}

// validateKey 会话键格式：min:max 两个正整数且 min < max（与生成规则一致）。
func validateKey(key string) error {
	if key == "" || utf8.RuneCountInString(key) > maxKeyRunes {
		return fmt.Errorf("%w: invalid conversation key", ErrInvalidInput)
	}
	parts := strings.Split(key, ":")
	if len(parts) != 2 {
		return fmt.Errorf("%w: invalid conversation key", ErrInvalidInput)
	}
	a, errA := strconv.ParseInt(parts[0], 10, 64)
	b, errB := strconv.ParseInt(parts[1], 10, 64)
	if errA != nil || errB != nil || a <= 0 || b <= 0 || a >= b {
		return fmt.Errorf("%w: invalid conversation key", ErrInvalidInput)
	}
	return nil
}

// usernames 批量取用户名：去重后逐个跨模块查询（用户不存在兜底空串）。
func usernames(ctx context.Context, users UserService, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		u, err := users.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, usersvc.ErrUserNotFound) {
				out[id] = ""
				continue
			}
			return nil, err
		}
		if u == nil {
			out[id] = ""
			continue
		}
		out[id] = u.Username
	}
	return out, nil
}

// reverse 原地反转切片（倒序分页结果转正序）。
func reverse(msgs []model.Message) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}
