// Package service 承载 social 模块好友业务：发起申请→对方通过/拒绝→成为好友；
// 好友操作强制 owner 校验；被申请人用户名经跨模块进程内调用 user 服务补齐。
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
	"github.com/xiangzhang-coding/go-single/internal/social/repository"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrSelfRequest        = errors.New("cannot add yourself as friend")
	ErrTargetUserNotFound = errors.New("target user not found")
	ErrAlreadyFriends     = errors.New("already friends")
	ErrDuplicateRequest   = errors.New("friend request already pending")
	ErrRequestNotFound    = errors.New("friend request not found")
	ErrRequestForbidden   = errors.New("friend request does not belong to user")
	ErrRequestNotPending  = errors.New("friend request is not pending")
	ErrInvalidInput       = errors.New("invalid input")
)

// UserService user 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP；
// userSvc 天然满足，未来拆模块时换实现即可）。
type UserService interface {
	GetByID(ctx context.Context, id int64) (*usermodel.User, error)
	GetPublicByIDs(ctx context.Context, ids []int64) (map[int64]usermodel.PublicUser, error)
}

// 申请列表范围（scope）。
const (
	ScopeIncoming = "incoming"
	ScopeOutgoing = "outgoing"

	defaultRequestPageSize = 20
	maxRequestPageSize     = 50
)

// Service social 模块好友业务接口。
type Service interface {
	// SendRequest 发起申请：不可加自己；已是好友或已有待处理申请被拒。
	SendRequest(ctx context.Context, fromUserID, toUserID int64) (*model.FriendRequest, error)
	// Accept 通过申请：仅被申请人可操作（owner 校验）；建立双向好友关系。
	Accept(ctx context.Context, userID, requestID int64) error
	// Reject 拒绝申请：仅被申请人可操作（owner 校验）。
	Reject(ctx context.Context, userID, requestID int64) error
	// ListRequests 我的申请：scope=incoming/outgoing；status 空 = 全部。
	ListRequests(ctx context.Context, userID int64, scope, status string, page, pageSize int) ([]model.FriendRequestView, int64, error)
	// ListFriends 我的好友列表（双向：双方互为好友）。
	ListFriends(ctx context.Context, userID int64) ([]model.FriendView, error)
	// AreFriends 两人是否已是好友（chat 等跨模块校验用）。
	AreFriends(ctx context.Context, userID, friendID int64) (bool, error)
}

type friendService struct {
	store repository.Store
	users UserService
}

// New 构造好友服务。
func New(store repository.Store, users UserService) Service {
	return &friendService{store: store, users: users}
}

// SendRequest 申请流程：
// 自加被拒 → 目标用户存在校验（跨模块）→ 已有申请行按状态分流：
// pending 重复申请被拒 / accepted 已是好友被拒 / rejected 复用原行重新申请。
func (s *friendService) SendRequest(ctx context.Context, fromUserID, toUserID int64) (*model.FriendRequest, error) {
	if fromUserID == toUserID {
		return nil, ErrSelfRequest
	}
	u, err := s.users.GetByID(ctx, toUserID)
	if err != nil {
		if errors.Is(err, usersvc.ErrUserNotFound) {
			return nil, ErrTargetUserNotFound
		}
		return nil, err
	}
	if u == nil {
		return nil, ErrTargetUserNotFound
	}

	var result *model.FriendRequest
	err = s.store.Tx.WithinTx(ctx, func(tx repository.TxStore) error {
		forward, reverse, lockErr := lockUserPair(ctx, tx.Requests, fromUserID, toUserID)
		if lockErr != nil {
			return lockErr
		}
		friends, existsErr := tx.Friendships.Exists(ctx, fromUserID, toUserID)
		if existsErr != nil {
			return existsErr
		}
		if friends {
			return ErrAlreadyFriends
		}
		existing := forward
		if forward == nil || forward.FromUserID != fromUserID {
			existing = reverse
		}
		if existing != nil && existing.FromUserID == fromUserID {
			var resolveErr error
			result, resolveErr = resolveExisting(ctx, tx.Requests, existing)
			return resolveErr
		}

		result = &model.FriendRequest{
			FromUserID: fromUserID,
			ToUserID:   toUserID,
			Status:     model.RequestStatusPending,
		}
		if createErr := tx.Requests.Create(ctx, result); errors.Is(createErr, repository.ErrRequestPairExists) {
			return ErrDuplicateRequest
		} else {
			return createErr
		}
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// resolveExisting 已有申请行按状态分流：
// pending 重复申请被拒 / accepted 已是好友被拒 / rejected 复用原行重新申请（已是好友则不许重提）。
func resolveExisting(ctx context.Context, requests repository.FriendRequestRepository, req *model.FriendRequest) (*model.FriendRequest, error) {
	switch req.Status {
	case model.RequestStatusPending:
		return nil, ErrDuplicateRequest
	case model.RequestStatusAccepted:
		return nil, ErrAlreadyFriends
	case model.RequestStatusRejected:
		changed, err := requests.TransitionStatus(ctx, req.ID,
			model.RequestStatusRejected, model.RequestStatusPending)
		if err != nil {
			return nil, err
		}
		if !changed {
			current, getErr := requests.GetByID(ctx, req.ID)
			if getErr != nil {
				return nil, getErr
			}
			if current == nil {
				return nil, ErrRequestNotFound
			}
			switch current.Status {
			case model.RequestStatusPending:
				return nil, ErrDuplicateRequest
			case model.RequestStatusAccepted:
				return nil, ErrAlreadyFriends
			default:
				return nil, ErrRequestNotPending
			}
		}
		req.Status = model.RequestStatusPending
		return req, nil
	default:
		return nil, ErrRequestNotPending
	}
}

// Accept 通过申请：按稳定顺序锁定同一用户对的申请 → owner 校验 →
// 双向建关系 → 两个方向的申请一并收敛为通过。
// 已是好友（建关系撞唯一键）时仍收敛申请，自愈历史残留。
func (s *friendService) Accept(ctx context.Context, userID, requestID int64) error {
	seed, err := s.store.Requests.GetByID(ctx, requestID)
	if err != nil {
		return err
	}
	if seed == nil {
		return ErrRequestNotFound
	}
	return s.store.Tx.WithinTx(ctx, func(tx repository.TxStore) error {
		req, reverse, err := lockRequestPair(ctx, tx.Requests, requestID, seed.FromUserID, seed.ToUserID)
		if err != nil {
			return err
		}
		if req.ToUserID != userID {
			return ErrRequestForbidden
		}
		if req.Status != model.RequestStatusPending {
			return ErrRequestNotPending
		}
		changed, err := tx.Requests.TransitionStatus(ctx, req.ID,
			model.RequestStatusPending, model.RequestStatusAccepted)
		if err != nil {
			return err
		}
		if !changed {
			return ErrRequestNotPending
		}
		if err := tx.Friendships.CreatePair(ctx, userID, req.FromUserID); err != nil &&
			!errors.Is(err, repository.ErrFriendshipExists) {
			return err
		}

		// 建立好友后，同一用户对不能残留 pending/rejected 申请。
		if reverse != nil && reverse.ID != req.ID && reverse.Status != model.RequestStatusAccepted {
			changed, err = tx.Requests.TransitionStatus(ctx, reverse.ID,
				reverse.Status, model.RequestStatusAccepted)
			if err != nil {
				return err
			}
			if !changed {
				return ErrRequestNotPending
			}
		}
		return nil
	})
}

// Reject 拒绝申请：仅被申请人可操作，且申请须处于待处理。
func (s *friendService) Reject(ctx context.Context, userID, requestID int64) error {
	seed, err := s.store.Requests.GetByID(ctx, requestID)
	if err != nil {
		return err
	}
	if seed == nil {
		return ErrRequestNotFound
	}
	return s.store.Tx.WithinTx(ctx, func(tx repository.TxStore) error {
		req, _, err := lockRequestPair(ctx, tx.Requests, requestID, seed.FromUserID, seed.ToUserID)
		if err != nil {
			return err
		}
		if req.ToUserID != userID {
			return ErrRequestForbidden
		}
		if req.Status != model.RequestStatusPending {
			return ErrRequestNotPending
		}
		friends, err := tx.Friendships.Exists(ctx, userID, req.FromUserID)
		if err != nil {
			return err
		}
		if friends {
			return ErrRequestNotPending
		}
		changed, err := tx.Requests.TransitionStatus(ctx, req.ID,
			model.RequestStatusPending, model.RequestStatusRejected)
		if err != nil {
			return err
		}
		if !changed {
			return ErrRequestNotPending
		}
		return nil
	})
}

// ListRequests 我的申请：先取申请行，再跨模块批量补对方用户名（去重后一次一查）。
func (s *friendService) ListRequests(ctx context.Context, userID int64, scope, status string, page, pageSize int) ([]model.FriendRequestView, int64, error) {
	if scope != ScopeIncoming && scope != ScopeOutgoing {
		return nil, 0, fmt.Errorf("%w: invalid scope", ErrInvalidInput)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultRequestPageSize
	}
	if pageSize > maxRequestPageSize {
		pageSize = maxRequestPageSize
	}
	requests, total, err := s.store.Requests.ListByUser(ctx, userID, scope, status, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	peers := make([]int64, 0, len(requests))
	for i := range requests {
		peerID := requests[i].ToUserID
		if scope == ScopeIncoming {
			peerID = requests[i].FromUserID
		}
		peers = append(peers, peerID)
	}
	usernames, err := usernames(ctx, s.users, peers)
	if err != nil {
		return nil, 0, err
	}
	views := make([]model.FriendRequestView, 0, len(requests))
	for i := range requests {
		peerID := requests[i].ToUserID
		if scope == ScopeIncoming {
			peerID = requests[i].FromUserID
		}
		views = append(views, model.FriendRequestView{
			FriendRequest: requests[i],
			PeerUsername:  usernames[peerID],
		})
	}
	return views, total, nil
}

// ListFriends 我的好友：关系行 → 跨模块批量补用户名（去重后一次一查）。
func (s *friendService) ListFriends(ctx context.Context, userID int64) ([]model.FriendView, error) {
	list, err := s.store.Friendships.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	friendIDs := make([]int64, 0, len(list))
	for i := range list {
		friendIDs = append(friendIDs, list[i].FriendID)
	}
	usernames, err := usernames(ctx, s.users, friendIDs)
	if err != nil {
		return nil, err
	}
	views := make([]model.FriendView, 0, len(list))
	for i := range list {
		views = append(views, model.FriendView{
			UserID:   list[i].FriendID,
			Username: usernames[list[i].FriendID],
			Since:    list[i].CreatedAt,
		})
	}
	return views, nil
}

// AreFriends 两人是否已是好友：好友关系按方向存两行，(userID, friendID) 任一行
// 存在即互为好友（建关系时双向同时写入，状态总对称）。
func (s *friendService) AreFriends(ctx context.Context, userID, friendID int64) (bool, error) {
	return s.store.Friendships.Exists(ctx, userID, friendID)
}

// usernames 批量取用户名；用户不存在兜底空串。
// 好友服务与动态服务共用（动态时间线补作者用户名）。
func usernames(ctx context.Context, users UserService, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		out[id] = ""
	}
	profiles, err := users.GetPublicByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, profile := range profiles {
		out[id] = profile.Username
	}
	return out, nil
}

// lockRequestPair 按较小用户 ID 的方向先锁定同一用户对的两条申请。
// 通过和拒绝都遵循相同锁顺序，避免反向申请并发决策形成死锁或分裂终态。
func lockRequestPair(ctx context.Context, requests repository.FriendRequestRepository, requestID, userID, peerID int64) (*model.FriendRequest, *model.FriendRequest, error) {
	forward, reverse, err := lockUserPair(ctx, requests, userID, peerID)
	if err != nil {
		return nil, nil, err
	}
	if forward != nil && forward.ID == requestID {
		return forward, reverse, nil
	}
	if reverse != nil && reverse.ID == requestID {
		return reverse, forward, nil
	}
	return nil, nil, ErrRequestNotFound
}

func lockUserPair(ctx context.Context, requests repository.FriendRequestRepository, userID, peerID int64) (*model.FriendRequest, *model.FriendRequest, error) {
	if err := requests.LockPair(ctx, userID, peerID); err != nil {
		return nil, nil, err
	}
	low, high := userID, peerID
	if low > high {
		low, high = high, low
	}
	forward, err := requests.GetByPair(ctx, low, high)
	if err != nil {
		return nil, nil, err
	}
	reverse, err := requests.GetByPair(ctx, high, low)
	if err != nil {
		return nil, nil, err
	}
	return forward, reverse, nil
}
