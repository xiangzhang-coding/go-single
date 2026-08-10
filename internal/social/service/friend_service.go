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
}

// 申请列表范围（scope）。
const (
	ScopeIncoming = "incoming"
	ScopeOutgoing = "outgoing"
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
	ListRequests(ctx context.Context, userID int64, scope, status string) ([]model.FriendRequestView, error)
	// ListFriends 我的好友列表（双向：双方互为好友）。
	ListFriends(ctx context.Context, userID int64) ([]model.FriendView, error)
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

	if req, err := s.store.Requests.GetByPair(ctx, fromUserID, toUserID); err != nil {
		return nil, err
	} else if req != nil {
		return s.resolveExisting(ctx, req)
	}

	req := &model.FriendRequest{FromUserID: fromUserID, ToUserID: toUserID, Status: model.RequestStatusPending}
	if err := s.store.Requests.Create(ctx, req); err != nil {
		// 并发同对提交：唯一键冲突，按已有行状态再分流一次（与 resolveExisting 同一判定）。
		if errors.Is(err, repository.ErrRequestPairExists) {
			existing, getErr := s.store.Requests.GetByPair(ctx, fromUserID, toUserID)
			if getErr != nil {
				return nil, getErr
			}
			return s.resolveExisting(ctx, existing)
		}
		return nil, err
	}
	return req, nil
}

// resolveExisting 已有申请行按状态分流：
// pending 重复申请被拒 / accepted 已是好友被拒 / rejected 复用原行重新申请（已是好友则不许重提）。
func (s *friendService) resolveExisting(ctx context.Context, req *model.FriendRequest) (*model.FriendRequest, error) {
	switch req.Status {
	case model.RequestStatusPending:
		return nil, ErrDuplicateRequest
	case model.RequestStatusAccepted:
		return nil, ErrAlreadyFriends
	default: // rejected
		friends, err := s.store.Friendships.Exists(ctx, req.FromUserID, req.ToUserID)
		if err != nil {
			return nil, err
		}
		if friends {
			return nil, ErrAlreadyFriends
		}
		if err := s.store.Requests.UpdateStatus(ctx, req.ID, model.RequestStatusPending); err != nil {
			return nil, err
		}
		req.Status = model.RequestStatusPending
		return req, nil
	}
}

// Accept 通过申请：owner 校验 → 双向建关系 → 申请置为通过 →
// 收敛反向待处理申请（A→B 与 B→A 并存时一并通过，维持"好友间无待处理申请"不变式）。
// 已是好友（含建关系前检查与并发撞唯一键两处）时收敛本申请为通过，自愈历史残留。
func (s *friendService) Accept(ctx context.Context, userID, requestID int64) error {
	req, err := s.ensurePendingOwned(ctx, userID, requestID)
	if err != nil {
		return err
	}
	exists, err := s.store.Friendships.Exists(ctx, userID, req.FromUserID)
	if err != nil {
		return err
	}
	if exists {
		return s.markAccepted(ctx, req.ID)
	}
	if err := s.store.Friendships.CreatePair(ctx, userID, req.FromUserID); err != nil {
		if errors.Is(err, repository.ErrFriendshipExists) {
			return s.markAccepted(ctx, req.ID)
		}
		return err
	}
	if err := s.markAccepted(ctx, req.ID); err != nil {
		return err
	}

	// 反向待处理申请（B→A）随建关系一并收敛。
	reverse, err := s.store.Requests.GetByPair(ctx, req.ToUserID, req.FromUserID)
	if err != nil {
		return err
	}
	if reverse != nil && reverse.ID != req.ID && reverse.Status == model.RequestStatusPending {
		return s.store.Requests.UpdateStatus(ctx, reverse.ID, model.RequestStatusAccepted)
	}
	return nil
}

// markAccepted 将申请置为通过（幂等：已通过也不报错）。
func (s *friendService) markAccepted(ctx context.Context, requestID int64) error {
	return s.store.Requests.UpdateStatus(ctx, requestID, model.RequestStatusAccepted)
}

// Reject 拒绝申请：仅被申请人可操作，且申请须处于待处理。
func (s *friendService) Reject(ctx context.Context, userID, requestID int64) error {
	req, err := s.ensurePendingOwned(ctx, userID, requestID)
	if err != nil {
		return err
	}
	return s.store.Requests.UpdateStatus(ctx, req.ID, model.RequestStatusRejected)
}

// ListRequests 我的申请：先取申请行，再跨模块批量补对方用户名（去重后一次一查）。
func (s *friendService) ListRequests(ctx context.Context, userID int64, scope, status string) ([]model.FriendRequestView, error) {
	if scope != ScopeIncoming && scope != ScopeOutgoing {
		return nil, fmt.Errorf("%w: invalid scope", ErrInvalidInput)
	}
	requests, err := s.store.Requests.ListByUser(ctx, userID, scope, status)
	if err != nil {
		return nil, err
	}
	peers := make([]int64, 0, len(requests))
	for i := range requests {
		peerID := requests[i].ToUserID
		if scope == ScopeIncoming {
			peerID = requests[i].FromUserID
		}
		peers = append(peers, peerID)
	}
	usernames, err := s.usernames(ctx, peers)
	if err != nil {
		return nil, err
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
	return views, nil
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
	usernames, err := s.usernames(ctx, friendIDs)
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

// usernames 批量取用户名：去重后逐个跨模块查询（用户不存在兜底空串）。
func (s *friendService) usernames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		u, err := s.users.GetByID(ctx, id)
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

// ensurePendingOwned 申请存在（404）、归属本人（403）、处于待处理（409）。
func (s *friendService) ensurePendingOwned(ctx context.Context, userID, requestID int64) (*model.FriendRequest, error) {
	req, err := s.store.Requests.GetByID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, ErrRequestNotFound
	}
	if req.ToUserID != userID {
		return nil, ErrRequestForbidden
	}
	if req.Status != model.RequestStatusPending {
		return nil, ErrRequestNotPending
	}
	return req, nil
}
