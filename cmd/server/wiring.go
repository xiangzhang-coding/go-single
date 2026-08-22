package main

import (
	"context"
	"errors"

	chatmodel "github.com/xiangzhang-coding/go-single/internal/chat/model"
	chatsvc "github.com/xiangzhang-coding/go-single/internal/chat/service"
	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
)

type orderCancellationCoordinator struct {
	orders  ordersvc.Service
	seckill flashsalesvc.SeckillCancellation
}

func (c orderCancellationCoordinator) Cancel(ctx context.Context, userID int64, orderNo string) error {
	err := c.orders.Cancel(ctx, userID, orderNo)
	if errors.Is(err, ordersvc.ErrSeckillCancellationRequired) {
		return c.seckill.Cancel(ctx, userID, orderNo)
	}
	return err
}

// wsMessageNotifier 将 chat 服务"消息已落库"事件接入 WS Hub（T18）：
// 向在线接收方推送 new_message 事件；离线为无操作（落库 + 上线 REST 补拉兜底）。
type wsMessageNotifier struct {
	hub *ws.Hub
}

func (n wsMessageNotifier) NotifyMessageSent(_ context.Context, msg *chatmodel.Message) {
	n.hub.PushToUser(msg.RecipientID, ws.EventNewMessage, msg)
}

var _ chatsvc.MessageNotifier = wsMessageNotifier{}

// mediaAccessAuthorizer 聚合各业务模块的最小授权查询：已绑定头像对登录用户
// 可见，动态图片跟随好友关系，聊天媒体仅会话双方可读。
type mediaAccessAuthorizer struct {
	users avatarMediaAccess
	posts postMediaAccess
	chat  chatMediaAccess
}

type avatarMediaAccess interface {
	CanReadAvatar(ctx context.Context, reference string) (bool, error)
}

type postMediaAccess interface {
	CanReadImage(ctx context.Context, userID int64, reference string) (bool, error)
}

type chatMediaAccess interface {
	CanReadMedia(ctx context.Context, userID int64, reference string) (bool, error)
}

func (a mediaAccessAuthorizer) CanRead(ctx context.Context, userID int64, reference string) (bool, error) {
	allowed, err := a.users.CanReadAvatar(ctx, reference)
	if err != nil || allowed {
		return allowed, err
	}
	allowed, err = a.posts.CanReadImage(ctx, userID, reference)
	if err != nil || allowed {
		return allowed, err
	}
	return a.chat.CanReadMedia(ctx, userID, reference)
}

var _ file.AccessAuthorizer = mediaAccessAuthorizer{}
