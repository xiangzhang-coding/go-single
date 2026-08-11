// service 层单元测试（中间 seam）：fake 动态仓储 + fake 订单端口（复用
// ADR-0003 仓储接口 seam），覆盖 分享购买校验（未购买被拒）、时间线
// 仅好友可见与分页、删除 owner 校验（防 IDOR）。
package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
	"github.com/xiangzhang-coding/go-single/internal/social/repository"
)

// ---- fake 订单端口（购买校验） ----

type fakeOrders struct {
	purchased map[int64]bool
}

func (f *fakeOrders) HasPurchasedSKU(_ context.Context, _ int64, skuID int64) (bool, error) {
	return f.purchased[skuID], nil
}

// ---- fake 动态仓储 ----

type fakePosts struct {
	mu    sync.Mutex
	byID  map[int64]*model.Post
	order int64
}

func newFakePosts() *fakePosts {
	return &fakePosts{byID: map[int64]*model.Post{}}
}

func (f *fakePosts) Create(_ context.Context, post *model.Post) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order++
	post.ID = f.order
	// 时间随 id 单调递增：倒序断言可稳定使用 id。
	post.CreatedAt = time.Unix(1_700_000_000+int64(f.order), 0)
	f.byID[post.ID] = post
	return nil
}

func (f *fakePosts) GetByID(_ context.Context, id int64) (*model.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.byID[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}

func (f *fakePosts) ListByUsers(_ context.Context, userIDs []int64, offset, limit int) ([]model.Post, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inSet := make(map[int64]bool, len(userIDs))
	for _, id := range userIDs {
		inSet[id] = true
	}
	var all []model.Post
	for _, p := range f.byID {
		if inSet[p.UserID] {
			all = append(all, *p)
		}
	}
	// 时间倒序（created_at DESC, id DESC：id 单调即时间单调）。
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	total := int64(len(all))
	if offset >= len(all) {
		return []model.Post{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (f *fakePosts) Delete(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[id]; !ok {
		return false, nil
	}
	delete(f.byID, id)
	return true, nil
}

// ---- 测试夹具 ----

type postFixture struct {
	svc    PostService
	users  *fakeUsers
	fs     *fakeFriendships
	posts  *fakePosts
	orders *fakeOrders
}

func newPostFixture() *postFixture {
	users := newFakeUsers()
	users.add(1, "alice")
	users.add(2, "bob")
	users.add(3, "carol")
	fs := newFakeFriendships()
	posts := newFakePosts()
	orders := &fakeOrders{purchased: map[int64]bool{}}
	store := repository.Store{Requests: newFakeRequests(), Friendships: fs, Posts: posts}
	svc := NewPosts(store, users, orders)
	return &postFixture{svc: svc, users: users, fs: fs, posts: posts, orders: orders}
}

// share 夹具：alice(1) 分享 sku，断言成功并返回动态。
func (fx *postFixture) share(t *testing.T, userID, skuID int64, content string) *model.Post {
	t.Helper()
	post, err := fx.svc.Share(context.Background(), userID, ShareParams{SKUID: skuID, Content: content})
	require.NoError(t, err)
	return post
}

// ---- 分享：购买校验 ----

func TestShareRequiresPurchase(t *testing.T) {
	fx := newPostFixture()

	// 未购买 → 被拒，且不落库。
	fx.orders.purchased[10] = false
	_, err := fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, Content: "种草"})
	require.ErrorIs(t, err, ErrNotPurchased)
	require.Empty(t, fx.posts.byID)

	// 已购买（已支付/已发货/已完成订单）→ 分享成功。
	fx.orders.purchased[10] = true
	post, err := fx.svc.Share(context.Background(), 1, ShareParams{
		SKUID:    10,
		Content:  "好好用",
		ImageURL: "https://minio.example/x.png",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), post.UserID)
	require.Equal(t, int64(10), post.SKUID)
	require.Equal(t, "好好用", post.Content)
	require.Equal(t, "https://minio.example/x.png", post.ImageURL)
}

func TestShareValidation(t *testing.T) {
	fx := newPostFixture()
	fx.orders.purchased[10] = true

	// 非法 SKU → 400 类错误。
	_, err := fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 0})
	require.ErrorIs(t, err, ErrInvalidInput)

	// 文案/图片超长 → 400 类错误；长度按字符计（中文 1 字 = 1 字符）。
	_, err = fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, Content: strings.Repeat("好", maxPostContent+1)})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, ImageURL: strings.Repeat("a", maxImageURL+1)})
	require.ErrorIs(t, err, ErrInvalidInput)

	// 边界长度恰好放行（200 个中文字符 = 600 字节 > 500 字节，仍应通过）。
	_, err = fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, Content: strings.Repeat("好", maxPostContent)})
	require.NoError(t, err)

	// 可选图片须为 http/https URL。
	_, err = fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, ImageURL: "javascript:alert(1)"})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.Share(context.Background(), 1, ShareParams{SKUID: 10, ImageURL: "https://minio.example/x.png"})
	require.NoError(t, err)
}

// ---- 时间线：仅好友可见 ----

func TestFeedFriendsOnly(t *testing.T) {
	fx := newPostFixture()
	require.NoError(t, fx.fs.CreatePair(context.Background(), 1, 2)) // alice ↔ bob 好友
	fx.orders.purchased[10] = true
	fx.orders.purchased[11] = true
	fx.orders.purchased[12] = true

	// bob(2) 分享两条；carol(3)（非好友）分享一条；alice 自己分享一条。
	fx.share(t, 2, 10, "bob 第一条")
	fx.share(t, 2, 11, "bob 第二条")
	fx.share(t, 3, 12, "carol 的")
	fx.share(t, 1, 12, "alice 自己的")

	items, total, err := fx.svc.Feed(context.Background(), 1, 1, 20)
	require.NoError(t, err)
	require.Equal(t, int64(2), total, "时间线只含好友动态，不含自己与非好友")
	require.Len(t, items, 2)
	for _, v := range items {
		require.Equal(t, int64(2), v.UserID)
		require.Equal(t, "bob", v.AuthorUsername, "作者用户名经跨模块补齐")
	}
	require.Equal(t, "bob 第二条", items[0].Content, "时间倒序：最新在前")
	require.Equal(t, "bob 第一条", items[1].Content)
}

func TestFeedPagination(t *testing.T) {
	fx := newPostFixture()
	require.NoError(t, fx.fs.CreatePair(context.Background(), 1, 2))
	fx.orders.purchased[10] = true
	for i := 0; i < 5; i++ {
		fx.share(t, 2, 10, "第N条")
	}

	page1, total, err := fx.svc.Feed(context.Background(), 1, 1, 2)
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Len(t, page1, 2)
	require.Equal(t, int64(5), page1[0].ID, "第 1 页取最新两条")

	page3, _, err := fx.svc.Feed(context.Background(), 1, 3, 2)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	require.Equal(t, int64(1), page3[0].ID, "第 3 页剩最早一条")

	// 越界页返回空列表，total 不变。
	page4, total4, err := fx.svc.Feed(context.Background(), 1, 99, 2)
	require.NoError(t, err)
	require.Empty(t, page4)
	require.Equal(t, int64(5), total4)
}

func TestFeedNoFriends(t *testing.T) {
	fx := newPostFixture()
	fx.orders.purchased[10] = true
	fx.share(t, 2, 10, "bob 的") // 无好友关系

	items, total, err := fx.svc.Feed(context.Background(), 1, 1, 20)
	require.NoError(t, err)
	require.Empty(t, items)
	require.Zero(t, total)

	// page/page_size 越界自动收敛：非法页大小不报错。
	_, _, err = fx.svc.Feed(context.Background(), 1, 0, 0)
	require.NoError(t, err)
	items, _, err = fx.svc.Feed(context.Background(), 1, 1, 9999)
	require.NoError(t, err)
	require.Empty(t, items)
}

// ---- 删除自己的动态 ----

func TestDeleteOwnPost(t *testing.T) {
	fx := newPostFixture()
	fx.orders.purchased[10] = true
	post := fx.share(t, 1, 10, "alice 的")

	// 他人删除 → 403 类错误。
	require.ErrorIs(t, fx.svc.Delete(context.Background(), 2, post.ID), ErrPostForbidden)

	// 本人删除成功，动态消失。
	require.NoError(t, fx.svc.Delete(context.Background(), 1, post.ID))
	items, _, err := fx.svc.Feed(context.Background(), 2, 1, 20)
	require.NoError(t, err)
	require.Empty(t, items)

	// 删除不存在/已删除 → 404 类错误。
	require.ErrorIs(t, fx.svc.Delete(context.Background(), 1, post.ID), ErrPostNotFound)
	require.ErrorIs(t, fx.svc.Delete(context.Background(), 1, 999), ErrPostNotFound)
}
