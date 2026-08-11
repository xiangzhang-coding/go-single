// service 层单元测试（中间 seam）：fake 仓储 + fake 用户/好友服务
// （复用 ADR-0003 仓储接口 seam），覆盖发送校验（类型/长度/自加）、
// 跨模块校验（接收方存在、好友关系）、幂等重放、会话键有序、
// 会话列表（未读数/预览/对方用户名）、消息游标分页（after/before/缺省 + has_more）、
// 会话 owner 校验（防 IDOR）、已读游标只进不退、T18 实时推送端口
// （落库成功推给接收方、幂等重放不重复推、notifier 为 nil 不推送）。
package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/chat/model"
	"github.com/xiangzhang-coding/go-single/internal/chat/repository"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// ---- fake 用户服务 ----

type fakeUsers struct {
	byID map[int64]*usermodel.User
	fail map[int64]error // 模拟基础设施错误（透传测试）
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byID: map[int64]*usermodel.User{}, fail: map[int64]error{}}
}

func (f *fakeUsers) add(id int64, username string) {
	f.byID[id] = &usermodel.User{ID: id, Username: username}
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*usermodel.User, error) {
	if err := f.fail[id]; err != nil {
		return nil, err
	}
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, usersvc.ErrUserNotFound
}

// ---- fake 好友服务 ----

type fakeSocial struct {
	friends map[[2]int64]bool
}

func newFakeSocial() *fakeSocial { return &fakeSocial{friends: map[[2]int64]bool{}} }

func (f *fakeSocial) befriend(a, b int64) {
	f.friends[[2]int64{a, b}] = true
	f.friends[[2]int64{b, a}] = true
}

func (f *fakeSocial) AreFriends(_ context.Context, userID, friendID int64) (bool, error) {
	return f.friends[[2]int64{userID, friendID}], nil
}

// ---- fake 会话仓储 ----

type fakeConversations struct {
	mu   sync.Mutex
	byID map[string]*model.Conversation
}

func newFakeConversations() *fakeConversations {
	return &fakeConversations{byID: map[string]*model.Conversation{}}
}

func (f *fakeConversations) Ensure(_ context.Context, _ *gorm.DB, c *model.Conversation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[c.ConversationKey]; !ok {
		cp := *c
		f.byID[cp.ConversationKey] = &cp
	}
	return nil
}

func (f *fakeConversations) GetByKey(_ context.Context, key string) (*model.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.byID[key]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (f *fakeConversations) ListByUser(_ context.Context, userID int64, beforeLastMessageID int64, limit int) ([]model.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Conversation
	for _, c := range f.byID {
		if c.UserA == userID || c.UserB == userID {
			if beforeLastMessageID > 0 && c.LastMessageID >= beforeLastMessageID {
				continue
			}
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessageID > out[j].LastMessageID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeConversations) TouchLastMessage(_ context.Context, _ *gorm.DB, key string, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.byID[key]; ok {
		c.LastMessageID = messageID
	}
	return nil
}

var _ repository.ConversationRepository = (*fakeConversations)(nil)

// ---- fake 消息仓储 ----

type fakeMessages struct {
	mu    sync.Mutex
	byID  map[int64]*model.Message
	order int64
}

func newFakeMessages() *fakeMessages {
	return &fakeMessages{byID: map[int64]*model.Message{}}
}

func (f *fakeMessages) Create(_ context.Context, _ *gorm.DB, m *model.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ClientRequestID != nil {
		for _, existing := range f.byID {
			if existing.SenderID == m.SenderID &&
				existing.ClientRequestID != nil && *existing.ClientRequestID == *m.ClientRequestID {
				return repository.ErrMessageDuplicate
			}
		}
	}
	f.order++
	m.ID = f.order
	cp := *m
	f.byID[cp.ID] = &cp
	return nil
}

func (f *fakeMessages) GetByID(_ context.Context, id int64) (*model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (f *fakeMessages) GetByIdempotencyKey(_ context.Context, senderID int64, requestID string) (*model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.byID {
		if m.SenderID == senderID && m.ClientRequestID != nil && *m.ClientRequestID == requestID {
			cp := *m
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeMessages) GetByIDs(_ context.Context, ids []int64) (map[int64]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]model.Message, len(ids))
	for _, id := range ids {
		if m, ok := f.byID[id]; ok {
			out[id] = *m
		}
	}
	return out, nil
}

func (f *fakeMessages) ListAfter(_ context.Context, key string, afterID int64, limit int) ([]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Message
	for _, m := range f.byID {
		if m.ConversationKey == key && m.ID > afterID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeMessages) ListBefore(_ context.Context, key string, beforeID int64, limit int) ([]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Message
	for _, m := range f.byID {
		if m.ConversationKey == key && m.ID < beforeID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeMessages) ListLatest(_ context.Context, key string, limit int) ([]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Message
	for _, m := range f.byID {
		if m.ConversationKey == key {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ repository.MessageRepository = (*fakeMessages)(nil)

// ---- fake 已读游标仓储 ----

type fakeReads struct {
	mu   sync.Mutex
	byU  map[int64]map[string]int64
	msgs *fakeMessages
}

func newFakeReads(msgs *fakeMessages) *fakeReads {
	return &fakeReads{byU: map[int64]map[string]int64{}, msgs: msgs}
}

func (f *fakeReads) MarkRead(_ context.Context, userID int64, key string, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byU[userID] == nil {
		f.byU[userID] = map[string]int64{}
	}
	if f.byU[userID][key] < messageID {
		f.byU[userID][key] = messageID
	}
	return nil
}

func (f *fakeReads) ListUnreadCounts(_ context.Context, userID int64, keys []string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int64, len(keys))
	f.msgs.mu.Lock()
	defer f.msgs.mu.Unlock()
	for _, key := range keys {
		cnt := int64(0)
		for _, m := range f.msgs.byID {
			if m.ConversationKey == key && m.RecipientID == userID && m.ID > f.byU[userID][key] {
				cnt++
			}
		}
		out[key] = cnt
	}
	return out, nil
}

var _ repository.ReadStateRepository = (*fakeReads)(nil)

// ---- fake 事务运行器 ----

type fakeTx struct{}

func (fakeTx) WithinTx(_ context.Context, fn func(tx *gorm.DB) error) error { return fn(nil) }

var _ repository.TxRunner = fakeTx{}

// ---- fake 实时推送端口（T18）----

type fakeNotifier struct {
	mu   sync.Mutex
	sent []*model.Message
}

func (f *fakeNotifier) NotifyMessageSent(_ context.Context, msg *model.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
}

func (f *fakeNotifier) snapshot() []*model.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*model.Message(nil), f.sent...)
}

var _ MessageNotifier = (*fakeNotifier)(nil)

// ---- 测试环境 ----

type env struct {
	svc      Service
	users    *fakeUsers
	social   *fakeSocial
	conv     *fakeConversations
	msgs     *fakeMessages
	reads    *fakeReads
	notifier *fakeNotifier
}

func newEnv() *env {
	e := &env{users: newFakeUsers(), social: newFakeSocial(), conv: newFakeConversations(), msgs: newFakeMessages(), notifier: &fakeNotifier{}}
	e.reads = newFakeReads(e.msgs)
	e.svc = New(repository.Store{
		Conversations: e.conv,
		Messages:      e.msgs,
		Reads:         e.reads,
		Tx:            fakeTx{},
	}, e.users, e.social, e.notifier)
	return e
}

// seed 注册两个好友 alice(1)/bob(2)（bob 存在且互为好友）。
func (e *env) seed() {
	e.users.add(1, "alice")
	e.users.add(2, "bob")
	e.social.befriend(1, 2)
}

var (
	errBoom = errors.New("boom")
)

// ---- 发送：text/image/file 与跨模块校验 ----

func TestSendTextImageFile(t *testing.T) {
	e := newEnv()
	e.seed()

	text, err := e.svc.Send(context.Background(), 1, SendParams{ToUserID: 2, Type: "text", Content: "你好"})
	require.NoError(t, err)
	require.False(t, text.Idempotent)
	require.Equal(t, "text", text.Message.Type)
	require.Equal(t, "你好", text.Message.Content)
	require.Equal(t, "", text.Message.URL)
	require.Equal(t, model.ConversationKey(1, 2), text.Message.ConversationKey)
	require.Equal(t, int64(1), text.Message.SenderID)
	require.Equal(t, int64(2), text.Message.RecipientID)

	image, err := e.svc.Send(context.Background(), 1, SendParams{ToUserID: 2, Type: "image", URL: "http://minio.example/a.png"})
	require.NoError(t, err)
	require.Equal(t, "image", image.Message.Type)
	require.Equal(t, "http://minio.example/a.png", image.Message.URL)
	require.Equal(t, "", image.Message.Content)

	file, err := e.svc.Send(context.Background(), 2, SendParams{ToUserID: 1, Type: "file", URL: "https://minio.example/b.pdf"})
	require.NoError(t, err)
	require.Equal(t, "file", file.Message.Type)

	// 会话键与发送方向无关（bob 发 alice 也是 min:max）。
	require.Equal(t, "1:2", file.Message.ConversationKey)

	// 会话最近消息推进到最新一条。
	conv, err := e.conv.GetByKey(context.Background(), "1:2")
	require.NoError(t, err)
	require.Equal(t, file.Message.ID, conv.LastMessageID)
}

func TestSendConversationKeyOrdering(t *testing.T) {
	e := newEnv()
	e.seed()

	// 高 id 用户（bob=2）先发，会话键仍为 1:2，且 user_a < user_b。
	res, err := e.svc.Send(context.Background(), 2, SendParams{ToUserID: 1, Type: "text", Content: "hi"})
	require.NoError(t, err)
	require.Equal(t, "1:2", res.Message.ConversationKey)

	conv, err := e.conv.GetByKey(context.Background(), "1:2")
	require.NoError(t, err)
	require.Equal(t, int64(1), conv.UserA)
	require.Equal(t, int64(2), conv.UserB)
}

func TestSendValidation(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	cases := []struct {
		name string
		p    SendParams
		want error
	}{
		{"自加被拒", SendParams{ToUserID: 1, Type: "text", Content: "x"}, ErrSelfMessage},
		{"非法类型", SendParams{ToUserID: 2, Type: "voice", Content: "x"}, ErrInvalidInput},
		{"text 缺内容", SendParams{ToUserID: 2, Type: "text"}, ErrInvalidInput},
		{"text 内容超长", SendParams{ToUserID: 2, Type: "text", Content: str(maxContentRunes + 1)}, ErrInvalidInput},
		{"text 携带 url", SendParams{ToUserID: 2, Type: "text", Content: "x", URL: "http://minio.example/a.png"}, ErrInvalidInput},
		{"image 缺 url", SendParams{ToUserID: 2, Type: "image"}, ErrInvalidInput},
		{"image url 非法", SendParams{ToUserID: 2, Type: "image", URL: "ftp://x"}, ErrInvalidInput},
		{"image 携带内容", SendParams{ToUserID: 2, Type: "image", URL: "http://minio.example/a.png", Content: "x"}, ErrInvalidInput},
		{"file url 超长", SendParams{ToUserID: 2, Type: "file", URL: "http://x/" + str(maxURLRunes)}, ErrInvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.Send(ctx, 1, c.p)
			require.ErrorIs(t, err, c.want)
		})
	}
}

func str(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestSendRecipientAndFriendChecks(t *testing.T) {
	e := newEnv()
	e.users.add(1, "alice")
	e.users.add(2, "bob")
	e.social.befriend(1, 3) // alice 与 3 是好友
	e.users.add(3, "carol")
	ctx := context.Background()

	// 接收方不存在 → 404 语义。
	_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 999, Type: "text", Content: "x"})
	require.ErrorIs(t, err, ErrRecipientNotFound)

	// 非好友（alice→bob 未建关系）→ 403 语义。
	_, err = e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "x"})
	require.ErrorIs(t, err, ErrNotFriends)

	// 跨模块基础设施错误透传（不吞错）。
	e.users.fail[3] = errBoom
	_, err = e.svc.Send(ctx, 1, SendParams{ToUserID: 3, Type: "text", Content: "x"})
	require.ErrorIs(t, err, errBoom)
}

// ---- 发送：幂等重放 ----

func TestSendIdempotentReplay(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	first, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "你好", ClientRequestID: "req-1"})
	require.NoError(t, err)
	require.False(t, first.Idempotent)

	replay, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "你好", ClientRequestID: "req-1"})
	require.NoError(t, err)
	require.True(t, replay.Idempotent)
	require.Equal(t, first.Message.ID, replay.Message.ID, "幂等重放返回原消息")

	// 同一 client_request_id 但不同发送方 → 各算各的。
	_, err = e.svc.Send(ctx, 2, SendParams{ToUserID: 1, Type: "text", Content: "hi", ClientRequestID: "req-1"})
	require.NoError(t, err)

	// 无 client_request_id → 不幂等，可重复发。
	_, err = e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "again"})
	require.NoError(t, err)

	// 总消息数：3（幂等重放未新增）。
	var total int
	for _, m := range e.msgs.byID {
		if m.ConversationKey == "1:2" {
			total++
		}
	}
	require.Equal(t, 3, total)
}

// ---- T18 实时推送端口 ----

func TestSendNotifierPushesToRecipient(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	// alice→bob：推送携带落库后的完整消息（含 id），接收方为 bob。
	res, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "在吗"})
	require.NoError(t, err)
	sent := e.notifier.snapshot()
	require.Len(t, sent, 1)
	require.Equal(t, res.Message.ID, sent[0].ID, "推送的是落库后同一消息")
	require.Equal(t, int64(2), sent[0].RecipientID)
	require.Equal(t, "在吗", sent[0].Content)

	// 反向（bob→alice）同样推送。
	_, err = e.svc.Send(ctx, 2, SendParams{ToUserID: 1, Type: "text", Content: "在"})
	require.NoError(t, err)
	require.Len(t, e.notifier.snapshot(), 2)
}

func TestSendNotifierSkipsIdempotentReplay(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "hi", ClientRequestID: "req-1"})
	require.NoError(t, err)
	replay, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "hi", ClientRequestID: "req-1"})
	require.NoError(t, err)
	require.True(t, replay.Idempotent)

	require.Len(t, e.notifier.snapshot(), 1, "幂等重放不重复推送")
}

func TestSendWithoutNotifier(t *testing.T) {
	e := newEnv()
	e.seed()
	// notifier 为 nil：发送不受影响、不推送。
	e.svc = New(repository.Store{
		Conversations: e.conv,
		Messages:      e.msgs,
		Reads:         e.reads,
		Tx:            fakeTx{},
	}, e.users, e.social, nil)

	res, err := e.svc.Send(context.Background(), 1, SendParams{ToUserID: 2, Type: "text", Content: "hi"})
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Message.SenderID)
}

// ---- 会话列表 ----

func TestListConversations(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	// alice 发 2 条、bob 回 1 条。
	for i := 0; i < 2; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "a"})
		require.NoError(t, err)
	}
	_, err := e.svc.Send(ctx, 2, SendParams{ToUserID: 1, Type: "text", Content: "b"})
	require.NoError(t, err)

	// bob 未读 2 条（alice 发的），alice 未读 1 条（bob 发的）。
	bobViews, _, err := e.svc.ListConversations(ctx, 2, 0, 20)
	require.NoError(t, err)
	require.Len(t, bobViews, 1)
	bobView := bobViews[0]
	require.Equal(t, "1:2", bobView.ConversationKey)
	require.Equal(t, int64(1), bobView.PeerUserID)
	require.Equal(t, "alice", bobView.PeerUsername)
	require.Equal(t, int64(2), bobView.UnreadCount)
	require.Equal(t, "b", bobView.LastMessage.Content, "预览为最近一条")

	aliceViews, _, err := e.svc.ListConversations(ctx, 1, 0, 20)
	require.NoError(t, err)
	require.Equal(t, int64(2), aliceViews[0].PeerUserID)
	require.Equal(t, "bob", aliceViews[0].PeerUsername)
	require.Equal(t, int64(1), aliceViews[0].UnreadCount)
}

func TestListConversationsMultiOrdering(t *testing.T) {
	e := newEnv()
	e.seed()
	e.users.add(3, "carol")
	e.social.befriend(1, 3)
	ctx := context.Background()

	_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 3, Type: "text", Content: "to carol"})
	require.NoError(t, err)
	_, err = e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "to bob"})
	require.NoError(t, err)

	views, _, err := e.svc.ListConversations(ctx, 1, 0, 20)
	require.NoError(t, err)
	require.Len(t, views, 2)
	require.Equal(t, "1:2", views[0].ConversationKey, "最近活跃的会话在前")
	require.Equal(t, "1:3", views[1].ConversationKey)
}

// 会话列表游标分页：limit+1 探更多；before_id 翻页取更早会话。
func TestListConversationsPagination(t *testing.T) {
	e := newEnv()
	e.seed()
	e.users.add(3, "carol")
	e.social.befriend(1, 3)
	e.users.add(4, "dave")
	e.social.befriend(1, 4)
	ctx := context.Background()

	// 三个会话，活跃度递增：1:2（1 条）< 1:3（2 条）< 1:4（3 条）。
	for i := 0; i < 1; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "b"})
		require.NoError(t, err)
	}
	for i := 0; i < 2; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 3, Type: "text", Content: "c"})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 4, Type: "text", Content: "d"})
		require.NoError(t, err)
	}

	// 第 1 页 2 条（1:4、1:3），has_more；预览与未读正确。
	page1, hasMore, err := e.svc.ListConversations(ctx, 1, 0, 2)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Len(t, page1, 2)
	require.Equal(t, "1:4", page1[0].ConversationKey)
	require.Equal(t, "d", page1[0].LastMessage.Content)
	require.Zero(t, page1[0].UnreadCount, "发给别人不算未读")
	require.Equal(t, "1:3", page1[1].ConversationKey)

	// 第 2 页：before_id = 上一页末位的 last_message id → 剩 1:2。
	page2, hasMore, err := e.svc.ListConversations(ctx, 1, page1[1].LastMessage.ID, 2)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Len(t, page2, 1)
	require.Equal(t, "1:2", page2[0].ConversationKey)

	// 越界：无更多会话。
	page3, hasMore, err := e.svc.ListConversations(ctx, 1, page2[0].LastMessage.ID, 2)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Empty(t, page3)
}

// ---- 消息列表：游标分页 ----

func TestListMessagesCursorPagination(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "m"})
		require.NoError(t, err)
	}
	key := "1:2"

	// 缺省：最近 2 条，正序返回。
	items, hasMore, err := e.svc.ListMessages(ctx, 2, key, 0, 0, 2)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Len(t, items, 2)
	require.Equal(t, int64(4), items[0].ID)
	require.Equal(t, int64(5), items[1].ID)

	// after_id=2：id>2 的 2 条（3、4），has_more。
	items, hasMore, err = e.svc.ListMessages(ctx, 2, key, 2, 0, 2)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Equal(t, []int64{3, 4}, ids(items))

	// after_id=4：只剩 5，has_more=false。
	items, hasMore, err = e.svc.ListMessages(ctx, 2, key, 4, 0, 2)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, []int64{5}, ids(items))

	// before_id=3：id<3 的 2 条（1、2），正序。
	items, hasMore, err = e.svc.ListMessages(ctx, 2, key, 0, 3, 2)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, []int64{1, 2}, ids(items))

	// 游标互斥 → 400 语义。
	_, _, err = e.svc.ListMessages(ctx, 2, key, 1, 2, 2)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 非法键格式 / 不存在的会话。
	_, _, err = e.svc.ListMessages(ctx, 2, "abc", 0, 0, 2)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, _, err = e.svc.ListMessages(ctx, 2, "9:10", 0, 0, 2)
	require.ErrorIs(t, err, ErrConversationNotFound)
}

func ids(msgs []model.Message) []int64 {
	out := make([]int64, 0, len(msgs))
	for i := range msgs {
		out = append(out, msgs[i].ID)
	}
	return out
}

// ---- 会话 owner 校验（防 IDOR） ----

func TestListMessagesOwnerCheck(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "私聊"})
	require.NoError(t, err)

	// 会话双方可访问。
	_, _, err = e.svc.ListMessages(ctx, 1, "1:2", 0, 0, 10)
	require.NoError(t, err)
	_, _, err = e.svc.ListMessages(ctx, 2, "1:2", 0, 0, 10)
	require.NoError(t, err)

	// 非会话双方 → 403 语义。
	e.users.add(3, "carol")
	_, _, err = e.svc.ListMessages(ctx, 3, "1:2", 0, 0, 10)
	require.ErrorIs(t, err, ErrConversationForbidden)

	// 会话列表不含他人会话。
	views, _, err := e.svc.ListConversations(ctx, 3, 0, 20)
	require.NoError(t, err)
	require.Empty(t, views)
}

// ---- 已读游标 ----

func TestMarkRead(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "m"})
		require.NoError(t, err)
	}

	// bob 读到第 2 条 → 未读剩 1。
	require.NoError(t, e.svc.MarkRead(ctx, 2, "1:2", 2))
	views, _, err := e.svc.ListConversations(ctx, 2, 0, 20)
	require.NoError(t, err)
	require.Equal(t, int64(1), views[0].UnreadCount)

	// 只进不退：再读到更小的 id 不生效。
	require.NoError(t, e.svc.MarkRead(ctx, 2, "1:2", 1))
	views, _, err = e.svc.ListConversations(ctx, 2, 0, 20)
	require.NoError(t, err)
	require.Equal(t, int64(1), views[0].UnreadCount)

	// 推进到最新 → 未读清零。
	require.NoError(t, e.svc.MarkRead(ctx, 2, "1:2", 3))
	views, _, err = e.svc.ListConversations(ctx, 2, 0, 20)
	require.NoError(t, err)
	require.Zero(t, views[0].UnreadCount)
}

func TestMarkReadValidationAndOwner(t *testing.T) {
	e := newEnv()
	e.seed()
	ctx := context.Background()

	_, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 2, Type: "text", Content: "m"})
	require.NoError(t, err)

	// 非法消息 id / 不存在的消息 → 400/404 语义。
	require.ErrorIs(t, e.svc.MarkRead(ctx, 2, "1:2", 0), ErrInvalidInput)
	require.ErrorIs(t, e.svc.MarkRead(ctx, 2, "1:2", 999), ErrMessageNotFound)

	// 非会话双方 → 403 语义。
	e.users.add(3, "carol")
	require.ErrorIs(t, e.svc.MarkRead(ctx, 3, "1:2", 1), ErrConversationForbidden)

	// 不存在的会话 → 404 语义。
	require.ErrorIs(t, e.svc.MarkRead(ctx, 2, "8:9", 1), ErrConversationNotFound)

	// 消息属于另一会话 → 400 语义。
	e.users.add(4, "dave")
	e.social.befriend(1, 4)
	other, err := e.svc.Send(ctx, 1, SendParams{ToUserID: 4, Type: "text", Content: "另一个会话"})
	require.NoError(t, err)
	require.ErrorIs(t, e.svc.MarkRead(ctx, 2, "1:2", other.Message.ID), ErrInvalidInput)
}
