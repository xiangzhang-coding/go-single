// 好友圈动态业务：购买成功后分享（引用已购 SKU + 可选文案 + 可选图片，
// 购买校验跨模块经 order 服务）；时间线仅好友可见（拉取式：好友列表
// join 动态表按时间倒序分页）；删除仅限自己的动态。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"unicode/utf8"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
	"github.com/xiangzhang-coding/go-single/internal/social/repository"
)

// 动态字段上限（与迁移列宽一致）。
const (
	maxPostContent = 500
	maxImageURL    = 500
	// 时间线分页：默认页大小与上限。
	defaultFeedPageSize = 20
	maxFeedPageSize     = 50
)

// 动态业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrNotPurchased  = errors.New("cannot share SKU not purchased")
	ErrPostNotFound  = errors.New("post not found")
	ErrPostForbidden = errors.New("post does not belong to user")
)

// OrderService order 模块暴露的最小查询接口（跨模块进程内调用，面向接口非
// HTTP；orderSvc 天然满足）：校验用户确已购买某 SKU（已支付/已发货/已完成订单）。
type OrderService interface {
	HasPurchasedSKU(ctx context.Context, userID, skuID int64) (bool, error)
}

// ShareParams 分享参数；Content / ImageURL 为空串表示未填写。
type ShareParams struct {
	SKUID    int64
	Content  string
	ImageURL string
}

// PostService 好友圈动态业务接口。
type PostService interface {
	// Share 分享动态：校验确已购买（未购买被拒）；引用 SKU + 可选文案 + 可选图片。
	Share(ctx context.Context, userID int64, p ShareParams) (*model.Post, error)
	// Feed 好友圈时间线：仅好友动态（不含自己），时间倒序分页，
	// 返回条目（含作者用户名）与总数。
	Feed(ctx context.Context, userID int64, page, pageSize int) ([]model.PostView, int64, error)
	// Delete 删除自己的动态（owner 校验，防 IDOR）。
	Delete(ctx context.Context, userID, postID int64) error
}

type postService struct {
	store  repository.Store
	users  UserService
	orders OrderService
}

// NewPosts 构造动态服务。
func NewPosts(store repository.Store, users UserService, orders OrderService) PostService {
	return &postService{store: store, users: users, orders: orders}
}

// Share 分享流程：参数校验 → 购买校验（跨模块 order 服务，未购买被拒）→ 落库。
// 长度按字符计（与 VARCHAR 列宽一致）：中文文案按字符而非字节。
func (s *postService) Share(ctx context.Context, userID int64, p ShareParams) (*model.Post, error) {
	if userID <= 0 || p.SKUID <= 0 {
		return nil, fmt.Errorf("%w: invalid user or sku id", ErrInvalidInput)
	}
	if utf8.RuneCountInString(p.Content) > maxPostContent {
		return nil, fmt.Errorf("%w: content too long", ErrInvalidInput)
	}
	if utf8.RuneCountInString(p.ImageURL) > maxImageURL {
		return nil, fmt.Errorf("%w: image url too long", ErrInvalidInput)
	}
	if p.ImageURL != "" && !validImageURL(p.ImageURL) {
		return nil, fmt.Errorf("%w: invalid image url", ErrInvalidInput)
	}
	purchased, err := s.orders.HasPurchasedSKU(ctx, userID, p.SKUID)
	if err != nil {
		return nil, err
	}
	if !purchased {
		return nil, ErrNotPurchased
	}
	post := &model.Post{UserID: userID, SKUID: p.SKUID, Content: p.Content, ImageURL: p.ImageURL}
	if err := s.store.Posts.Create(ctx, post); err != nil {
		return nil, err
	}
	return post, nil
}

// validImageURL 可选图片须为 http/https URL（图片经 platform/file 上传后引用 URL）。
func validImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Feed 时间线：好友列表 → 动态表按时间倒序分页 → 跨模块批量补作者用户名。
func (s *postService) Feed(ctx context.Context, userID int64, page, pageSize int) ([]model.PostView, int64, error) {
	if userID <= 0 {
		return nil, 0, fmt.Errorf("%w: invalid user id", ErrInvalidInput)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultFeedPageSize
	}
	if pageSize > maxFeedPageSize {
		pageSize = maxFeedPageSize
	}

	friends, err := s.store.Friendships.ListByUser(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	friendIDs := make([]int64, 0, len(friends))
	for i := range friends {
		friendIDs = append(friendIDs, friends[i].FriendID)
	}
	posts, total, err := s.store.Posts.ListByUsers(ctx, friendIDs, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	names, err := usernames(ctx, s.users, authorIDs(posts))
	if err != nil {
		return nil, 0, err
	}
	views := make([]model.PostView, 0, len(posts))
	for i := range posts {
		views = append(views, model.PostView{
			Post:           posts[i],
			AuthorUsername: names[posts[i].UserID],
		})
	}
	return views, total, nil
}

// Delete 删除自己的动态：存在校验（404）→ 归属校验（403）→ 删除。
// 删除经 RowsAffected 兜底：读取后已被他人/并发删除的，返回 404（不谎报成功）。
func (s *postService) Delete(ctx context.Context, userID, postID int64) error {
	post, err := s.store.Posts.GetByID(ctx, postID)
	if err != nil {
		return err
	}
	if post == nil {
		return ErrPostNotFound
	}
	if post.UserID != userID {
		return ErrPostForbidden
	}
	affected, err := s.store.Posts.Delete(ctx, postID)
	if err != nil {
		return err
	}
	if !affected {
		return ErrPostNotFound
	}
	return nil
}

// authorIDs 提取动态作者 id（usernames 内部去重，无需在调用方去重）。
func authorIDs(posts []model.Post) []int64 {
	ids := make([]int64, 0, len(posts))
	for i := range posts {
		ids = append(ids, posts[i].UserID)
	}
	return ids
}
