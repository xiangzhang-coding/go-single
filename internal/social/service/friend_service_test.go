// service 层单元测试（中间 seam）：fake 仓储 + fake 用户服务（复用 ADR-0003 仓储接口 seam），
// 覆盖好友申请状态机（pending→accepted/rejected、被拒后重新申请）、
// 自加/重复申请被拒、owner 校验（防 IDOR）、双向建关系、列表视图。
package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
	"github.com/xiangzhang-coding/go-single/internal/social/repository"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// ---- fake 用户服务 ----

type fakeUsers struct {
	byID       map[int64]*usermodel.User
	batchCalls int
	missing    []int64 // 模拟不存在的用户 id（返回 ErrUserNotFound）
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byID: map[int64]*usermodel.User{}}
}

func (f *fakeUsers) add(id int64, username string) {
	f.byID[id] = &usermodel.User{ID: id, Username: username}
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*usermodel.User, error) {
	for _, m := range f.missing {
		if m == id {
			return nil, usersvc.ErrUserNotFound
		}
	}
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, usersvc.ErrUserNotFound
}

func (f *fakeUsers) GetPublicByIDs(_ context.Context, ids []int64) (map[int64]usermodel.PublicUser, error) {
	f.batchCalls++
	out := make(map[int64]usermodel.PublicUser, len(ids))
	for _, id := range ids {
		if u := f.byID[id]; u != nil {
			out[id] = usermodel.PublicUser{ID: id, Username: u.Username}
		}
	}
	return out, nil
}

// ---- fake 好友申请仓储 ----

type fakeRequests struct {
	mu    sync.Mutex
	byID  map[int64]*model.FriendRequest
	order int64
}

func newFakeRequests() *fakeRequests {
	return &fakeRequests{byID: map[int64]*model.FriendRequest{}}
}

func (f *fakeRequests) Create(_ context.Context, r *model.FriendRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.findPair(r.FromUserID, r.ToUserID); ok {
		return repository.ErrRequestPairExists
	}
	f.order++
	r.ID = f.order
	f.byID[r.ID] = r
	return nil
}

func (f *fakeRequests) GetByID(_ context.Context, id int64) (*model.FriendRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byID[id], nil
}

func (f *fakeRequests) GetByPair(_ context.Context, fromUserID, toUserID int64) (*model.FriendRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, _ := f.findPair(fromUserID, toUserID)
	return r, nil
}

func (f *fakeRequests) LockPair(context.Context, int64, int64) error { return nil }

func (f *fakeRequests) TransitionStatus(_ context.Context, id int64, from, to string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.byID[id]; ok && r.Status == from {
		r.Status = to
		return true, nil
	}
	return false, nil
}

func (f *fakeRequests) ListByUser(_ context.Context, userID int64, scope, status string, offset, limit int) ([]model.FriendRequest, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.FriendRequest
	for _, r := range f.byID {
		switch scope {
		case "incoming":
			if r.ToUserID != userID {
				continue
			}
		default:
			if r.FromUserID != userID {
				continue
			}
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	total := int64(len(out))
	if offset >= len(out) {
		return []model.FriendRequest{}, total, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

func (f *fakeRequests) findPair(from, to int64) (*model.FriendRequest, bool) {
	for _, r := range f.byID {
		if r.FromUserID == from && r.ToUserID == to {
			return r, true
		}
	}
	return nil, false
}

// ---- fake 好友关系仓储 ----

type fakeFriendships struct {
	rows []model.Friendship
}

func newFakeFriendships() *fakeFriendships {
	return &fakeFriendships{}
}

func (f *fakeFriendships) CreatePair(_ context.Context, userID, friendID int64) error {
	for _, row := range f.rows {
		if row.UserID == userID && row.FriendID == friendID {
			return repository.ErrFriendshipExists
		}
	}
	f.rows = append(f.rows,
		model.Friendship{ID: int64(len(f.rows) + 1), UserID: userID, FriendID: friendID},
		model.Friendship{ID: int64(len(f.rows) + 2), UserID: friendID, FriendID: userID},
	)
	return nil
}

type fakeFriendTx struct {
	requests    *fakeRequests
	friendships *fakeFriendships
}

func (f fakeFriendTx) WithinTx(_ context.Context, fn func(repository.TxStore) error) error {
	return fn(repository.TxStore{Requests: f.requests, Friendships: f.friendships})
}

func (f *fakeFriendships) Exists(_ context.Context, userID, friendID int64) (bool, error) {
	for _, row := range f.rows {
		if row.UserID == userID && row.FriendID == friendID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeFriendships) ListByUser(_ context.Context, userID int64) ([]model.Friendship, error) {
	var out []model.Friendship
	for _, row := range f.rows {
		if row.UserID == userID {
			out = append(out, row)
		}
	}
	return out, nil
}

// ---- 测试夹具 ----

type fixture struct {
	svc   Service
	users *fakeUsers
	reqs  *fakeRequests
	fs    *fakeFriendships
}

func newFixture() *fixture {
	users := newFakeUsers()
	users.add(1, "alice")
	users.add(2, "bob")
	users.add(3, "carol")
	reqs := newFakeRequests()
	fs := newFakeFriendships()
	svc := New(repository.Store{
		Requests: reqs, Friendships: fs,
		Tx: fakeFriendTx{requests: reqs, friendships: fs},
	}, users)
	return &fixture{svc: svc, users: users, reqs: reqs, fs: fs}
}

// send 夹具：alice(1) → bob(2) 发起申请，返回申请 id。
func (fx *fixture) send(t *testing.T, from, to int64) int64 {
	t.Helper()
	r, err := fx.svc.SendRequest(context.Background(), from, to)
	require.NoError(t, err)
	return r.ID
}

// ---- 状态机：申请→通过→好友（双向）----

func TestFriendRequestAcceptFlow(t *testing.T) {
	fx := newFixture()

	reqID := fx.send(t, 1, 2)
	req, err := fx.reqs.GetByID(context.Background(), reqID)
	require.NoError(t, err)
	require.Equal(t, model.RequestStatusPending, req.Status)

	// bob(2) 通过 → 建立好友。
	require.NoError(t, fx.svc.Accept(context.Background(), 2, reqID))

	// 双向好友列表都含对方。
	aFriends, err := fx.svc.ListFriends(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, aFriends, 1)
	require.Equal(t, int64(2), aFriends[0].UserID)
	require.Equal(t, "bob", aFriends[0].Username)

	bFriends, err := fx.svc.ListFriends(context.Background(), 2)
	require.NoError(t, err)
	require.Len(t, bFriends, 1)
	require.Equal(t, int64(1), bFriends[0].UserID)
	require.Equal(t, "alice", bFriends[0].Username)

	// 申请置为通过。
	req, _ = fx.reqs.GetByID(context.Background(), reqID)
	require.Equal(t, model.RequestStatusAccepted, req.Status)

	// 已是好友：再申请被拒；重复通过同一申请（已非待处理）→ 409。
	_, err = fx.svc.SendRequest(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrAlreadyFriends)
	require.ErrorIs(t, fx.svc.Accept(context.Background(), 2, reqID), ErrRequestNotPending)
}

// ---- 拒绝后不建立关系，可重新申请 ----

func TestFriendRequestRejectThenReapply(t *testing.T) {
	fx := newFixture()

	reqID := fx.send(t, 1, 2)
	require.NoError(t, fx.svc.Reject(context.Background(), 2, reqID))

	// 拒绝后不建立关系。
	aFriends, err := fx.svc.ListFriends(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, aFriends)
	req, _ := fx.reqs.GetByID(context.Background(), reqID)
	require.Equal(t, model.RequestStatusRejected, req.Status)

	// 被拒后重新申请：复用原行回到 pending，id 不变。
	r, err := fx.svc.SendRequest(context.Background(), 1, 2)
	require.NoError(t, err)
	require.Equal(t, reqID, r.ID)
	require.Equal(t, model.RequestStatusPending, r.Status)
}

// ---- 自加与重复申请被拒 ----

func TestFriendRequestSelfAndDuplicate(t *testing.T) {
	fx := newFixture()

	_, err := fx.svc.SendRequest(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrSelfRequest)

	fx.send(t, 1, 2)
	_, err = fx.svc.SendRequest(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrDuplicateRequest)

	// 目标用户不存在 → 404 类错误。
	fx.users.missing = []int64{999}
	_, err = fx.svc.SendRequest(context.Background(), 1, 999)
	require.ErrorIs(t, err, ErrTargetUserNotFound)
}

// ---- owner 校验（防 IDOR）----

func TestFriendRequestOwnerCheck(t *testing.T) {
	fx := newFixture()

	reqID := fx.send(t, 1, 2) // alice→bob

	// 申请人 alice 不能通过/拒绝自己的申请（只有被申请人 bob 可操作）。
	require.ErrorIs(t, fx.svc.Accept(context.Background(), 1, reqID), ErrRequestForbidden)
	require.ErrorIs(t, fx.svc.Reject(context.Background(), 1, reqID), ErrRequestForbidden)

	// 无关第三人 carol 同样 403。
	require.ErrorIs(t, fx.svc.Accept(context.Background(), 3, reqID), ErrRequestForbidden)

	// 不存在的申请 → 404。
	require.ErrorIs(t, fx.svc.Accept(context.Background(), 2, 999), ErrRequestNotFound)

	// 非待处理申请：拒绝后再通过 → 409。
	require.NoError(t, fx.svc.Reject(context.Background(), 2, reqID))
	require.ErrorIs(t, fx.svc.Accept(context.Background(), 2, reqID), ErrRequestNotPending)
	require.ErrorIs(t, fx.svc.Reject(context.Background(), 2, reqID), ErrRequestNotPending)
}

// ---- 反向待处理申请收敛 ----

// A→B 与 B→A 并存时，任一方通过后双方即为好友，另一条申请一并置为通过。
func TestFriendRequestReverseConvergence(t *testing.T) {
	fx := newFixture()

	ab := fx.send(t, 1, 2)
	ba := fx.send(t, 2, 1) // 互发申请，各自 pending

	require.NoError(t, fx.svc.Accept(context.Background(), 2, ab))

	for _, id := range []int64{ab, ba} {
		req, _ := fx.reqs.GetByID(context.Background(), id)
		require.Equal(t, model.RequestStatusAccepted, req.Status, "反向申请应随建关系一并收敛")
	}
	aFriends, err := fx.svc.ListFriends(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, aFriends, 1)
}

func TestFriendRequestRejectedReverseConvergesWhenPairBecomesFriends(t *testing.T) {
	fx := newFixture()
	ab := fx.send(t, 1, 2)
	ba := fx.send(t, 2, 1)
	require.NoError(t, fx.svc.Reject(context.Background(), 1, ba))

	require.NoError(t, fx.svc.Accept(context.Background(), 2, ab))
	for _, id := range []int64{ab, ba} {
		req, err := fx.reqs.GetByID(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, model.RequestStatusAccepted, req.Status)
	}
}

func TestFriendRequestCannotCreateMissingReverseAfterAcceptance(t *testing.T) {
	fx := newFixture()
	ab := fx.send(t, 1, 2)
	require.NoError(t, fx.svc.Accept(context.Background(), 2, ab))

	_, err := fx.svc.SendRequest(context.Background(), 2, 1)
	require.ErrorIs(t, err, ErrAlreadyFriends)
	reverse, getErr := fx.reqs.GetByPair(context.Background(), 2, 1)
	require.NoError(t, getErr)
	require.Nil(t, reverse)
}

// 历史残留自愈：两人已是好友但申请仍为 pending（如历史建关系后未收敛），
// 通过该申请应直接收敛为 accepted 而非报错，维持"好友间无待处理申请"不变式。
func TestFriendRequestAcceptSelfHeal(t *testing.T) {
	fx := newFixture()

	reqID := fx.send(t, 1, 2)
	// 模拟历史残留：直接写好友关系，跳过申请收敛。
	require.NoError(t, fx.fs.CreatePair(context.Background(), 1, 2))

	require.NoError(t, fx.svc.Accept(context.Background(), 2, reqID))
	req, _ := fx.reqs.GetByID(context.Background(), reqID)
	require.Equal(t, model.RequestStatusAccepted, req.Status, "已是好友时通过应自愈收敛残留申请")

	// 重新申请此时被拒（已是好友）。
	_, err := fx.svc.SendRequest(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrAlreadyFriends)
}

// ---- 列表视图 ----

func TestListRequestsScopes(t *testing.T) {
	fx := newFixture()

	ab := fx.send(t, 1, 2)
	_ = fx.send(t, 1, 3)

	incoming, total, err := fx.svc.ListRequests(context.Background(), 2, ScopeIncoming, "", 1, 20)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, incoming, 1)
	require.Equal(t, ab, incoming[0].ID)
	require.Equal(t, "alice", incoming[0].PeerUsername)

	outgoing, total, err := fx.svc.ListRequests(context.Background(), 1, ScopeOutgoing, "", 1, 20)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, outgoing, 2)
	require.Equal(t, "carol", outgoing[0].PeerUsername, "最新申请在前")
	require.Equal(t, "bob", outgoing[1].PeerUsername)

	// status 筛选：pending 两条，通过后筛选 accepted 一条。
	require.NoError(t, fx.svc.Accept(context.Background(), 2, ab))
	accepted, _, err := fx.svc.ListRequests(context.Background(), 1, ScopeOutgoing, "accepted", 1, 20)
	require.NoError(t, err)
	require.Len(t, accepted, 1)

	// 非法 scope → 400 类错误。
	_, _, err = fx.svc.ListRequests(context.Background(), 1, "sideways", "", 1, 20)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Equal(t, 3, fx.users.batchCalls, "每次列表补齐只应调用一次批量用户查询")
}

// ---- 并发申请只成功一条 ----

func TestSendRequestConcurrentSamePair(t *testing.T) {
	fx := newFixture()

	var wg sync.WaitGroup
	results := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = fx.svc.SendRequest(context.Background(), 1, 2)
		}(i)
	}
	wg.Wait()

	var ok, dup int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDuplicateRequest):
			dup++
		default:
			t.Fatalf("意外的错误: %v", err)
		}
	}
	require.Equal(t, 1, ok, "同一对并发申请只允许一条 pending")
	require.Equal(t, 19, dup)
}
