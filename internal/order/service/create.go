package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/order/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// Create 下单（幂等接口有限重试，T20）：client_request_id 幂等 + 单事务原子性，
// 基础设施瞬时故障（DB/Redis 抖动）重试 + 退避；业务拒绝（校验类错误）经
// retry.Stop 立即返回不重试。真实执行见 createOrder。
func (s *orderService) Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error) {
	var result *CreateResult
	err := retry.Do(ctx, s.retryCfg, func(ctx context.Context) error {
		var attemptErr error
		result, attemptErr = s.createOrder(ctx, userID, p)
		if attemptErr != nil && !isValidationError(attemptErr) {
			return attemptErr // 基础设施/未知错误：可重试
		}
		return retry.Stop(attemptErr) // 业务拒绝：重试无意义
	})
	return result, err
}

// createOrder 下单流程：
//  1. 查询持久请求身份；未命中再生成订单号并以 Redis SETNX 协调在途请求
//  2. 读取地址（固化为快照）→ 组装订单项（购物车/直购）
//  3. 单事务读取 SKU、锁定并核销券，以同一券事实计算金额
//  4. 条件扣减库存 → 建订单+订单项 → 删除已购购物车条目
//
// 任一校验失败（库存不足/券不可用等）删除幂等键，允许修正后重试。
// 幂等语义保证重试安全：事务原子回滚 + 幂等键重复请求返回同一订单号。
func (s *orderService) createOrder(ctx context.Context, userID int64, p CreateParams) (result *CreateResult, err error) {
	if userID <= 0 || p.AddressID <= 0 {
		return nil, fmt.Errorf("%w: invalid user or address id", ErrInvalidInput)
	}
	if p.ClientRequestID == "" || len(p.ClientRequestID) > 64 {
		return nil, fmt.Errorf("%w: invalid client_request_id", ErrInvalidInput)
	}
	if p.CouponID < 0 {
		return nil, fmt.Errorf("%w: invalid coupon id", ErrInvalidInput)
	}
	if err := validateItems(&p); err != nil {
		return nil, err
	}
	if existing, err := s.replayNormalRequest(ctx, userID, p.ClientRequestID); err != nil || existing != nil {
		return existing, err
	}

	orderNo, acquired, err := s.acquireIdempotency(ctx, userID, p.ClientRequestID)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return s.replayIdempotent(ctx, orderNo)
	}
	// 校验类失败释放幂等键，允许客户端修正后重试；基础设施类失败先查订单：
	// 已提交则保留幂等键返回同一订单号，确认未提交则释放，避免永久占用请求。
	defer func() {
		if err == nil {
			return
		}
		shouldRelease := isValidationError(err)
		if !shouldRelease {
			existing, lookupErr := s.store.Orders.GetByNo(ctx, orderNo)
			shouldRelease = lookupErr == nil && existing == nil
		}
		if shouldRelease {
			if delErr := s.cache.Del(ctx, idemKey(userID, p.ClientRequestID)); delErr != nil {
				err = fmt.Errorf("%w: release idempotency key: %v", err, delErr)
			}
		}
	}()

	address, err := s.users.GetAddress(ctx, userID, p.AddressID)
	if err != nil {
		if errors.Is(err, usersvc.ErrAddressNotFound) {
			return nil, ErrAddressNotFound
		}
		if errors.Is(err, usersvc.ErrAddressForbidden) {
			return nil, ErrAddressForbidden
		}
		return nil, err
	}
	if address == nil {
		return nil, ErrAddressNotFound
	}

	plannedProductIDs, err := s.productIDsForCreate(ctx, userID, &p)
	if err != nil {
		return nil, err
	}
	mutatingProducts, err := s.beginProductDetailMutations(ctx, plannedProductIDs)
	if err != nil {
		return nil, err
	}

	var (
		snapshots   []lineSnapshot
		total       int64
		coupon      *couponmodel.CouponRedemption
		order       *model.Order
		items       []model.OrderItem
		cartItemIDs []int64
	)
	err = s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		if p.FromCart {
			cartItems, err := s.cart.LockItems(ctx, tx, userID)
			if err != nil {
				return err
			}
			if len(cartItems) == 0 {
				return ErrCartEmpty
			}
			lines := make([]orderLine, 0, len(cartItems))
			cartItemIDs = make([]int64, 0, len(cartItems))
			for _, item := range cartItems {
				lines = append(lines, orderLine{skuID: item.SKUID, quantity: item.Quantity})
				cartItemIDs = append(cartItemIDs, item.ID)
			}
			snapshots, total, err = s.loadSnapshots(ctx, tx, lines)
			if err != nil {
				return err
			}
		} else {
			lines := directLines(&p)
			snapshots, total, err = s.loadSnapshots(ctx, tx, lines)
			if err != nil {
				return err
			}
		}
		if !mutationsCoverProducts(mutatingProducts, productIDsFromSnapshots(snapshots)) {
			return errCartChangedWhileFencing
		}
		if p.CouponID > 0 {
			redemption, err := s.coupons.RedeemForOrder(ctx, tx, userID, p.CouponID, total)
			if err != nil {
				return translateCouponError(err)
			}
			coupon = &redemption
		}

		discount, pay, err := calculateAmounts(total, coupon)
		if err != nil {
			return err
		}
		order, items = buildOrder(userID, orderNo, p.ClientRequestID, address, snapshots, total, discount, pay, coupon)
		return s.persistOrder(ctx, tx, userID, snapshots, order, items, cartItemIDs)
	})
	s.finishProductDetailMutations(ctx, mutatingProducts)
	if err != nil {
		if errors.Is(err, repository.ErrOrderDuplicate) {
			if existing, replayErr := s.replayNormalRequest(ctx, userID, p.ClientRequestID); replayErr != nil || existing != nil {
				return existing, replayErr
			}
		}
		return nil, err
	}
	// 订单创建/状态流转打点（T19c）：事务提交后计数，幂等命中不重复计数。
	if coupon != nil {
		s.metrics.CouponRedeemed()
	}
	s.metrics.OrderCreated(model.OrderTypeNormal)
	s.metrics.OrderStatusChanged(model.OrderStatusPendingPayment)
	return &CreateResult{Order: &model.OrderView{Order: *order, Items: items}}, nil
}

// CreateSeckillInTx 提供秒杀异步落单的 order 模块事务内能力：
//  1. 使用 PrepareSeckillOrder 已准备的地址与商品快照
//  2. 在调用方事务内创建秒杀订单（10min 超时）+ 订单项
//  3. 重复键（order_no 主键 / user_activity_key 唯一约束）返回 created=false，
//     由 flashsale 编排跳过活动库存扣减
func (s *orderService) CreateSeckillInTx(ctx context.Context, tx *transaction.Handle, p SeckillCreateParams) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("%w: transaction required", ErrInvalidInput)
	}
	if p.OrderNo == "" || p.UserID <= 0 || p.ActivityID <= 0 || p.PurchaseSlot <= 0 || p.SKUID <= 0 ||
		p.Price <= 0 || p.Price > productmodel.MaxPriceCents || p.Quantity < 1 || p.Quantity > 99 ||
		p.Snapshot == nil || p.Snapshot.SKUID != p.SKUID || p.Snapshot.ProductID <= 0 ||
		p.Snapshot.ProductTitle == "" || !json.Valid(p.Snapshot.Specs) {
		return false, fmt.Errorf("%w: invalid seckill order params", ErrInvalidInput)
	}
	amount, err := checkedAmountMul(p.Price, p.Quantity)
	if err != nil {
		return false, err
	}

	now := time.Now()
	activityID := p.ActivityID
	purchaseSlot := p.PurchaseSlot
	// 每次成功预扣占一个稳定购买槽位；唯一键只拦截同槽消息重投，不会把
	// 同一用户在限购范围内的第二次购买误判为第一单重放。
	dedupKey := fmt.Sprintf("%d:%d:%d", p.UserID, activityID, purchaseSlot)
	order := &model.Order{
		OrderNo:         p.OrderNo,
		UserID:          p.UserID,
		OrderType:       model.OrderTypeSeckill,
		Status:          model.OrderStatusPendingPayment,
		ActivityID:      &activityID,
		PurchaseSlot:    &purchaseSlot,
		TotalAmount:     amount,
		PayAmount:       amount,
		Receiver:        p.Snapshot.Address.Receiver,
		Phone:           p.Snapshot.Address.Phone,
		Province:        p.Snapshot.Address.Province,
		City:            p.Snapshot.Address.City,
		District:        p.Snapshot.Address.District,
		Detail:          p.Snapshot.Address.Detail,
		ExpireAt:        now.Add(seckillExpire),
		UserActivityKey: &dedupKey,
	}
	item := model.OrderItem{
		OrderNo:   p.OrderNo,
		SKUID:     p.SKUID,
		ProductID: p.Snapshot.ProductID,
		Title:     p.Snapshot.ProductTitle,
		Specs:     p.Snapshot.Specs,
		Price:     p.Price,
		Quantity:  p.Quantity,
		Subtotal:  amount,
	}
	if err := validateAmountConsistency(order, []model.OrderItem{item}); err != nil {
		return false, err
	}

	if err := s.store.Orders.Create(ctx, tx, order); err != nil {
		if errors.Is(err, repository.ErrOrderDuplicate) {
			existing, getErr := s.store.Orders.GetByNoInTx(ctx, tx, p.OrderNo)
			if getErr != nil {
				return false, getErr
			}
			if existing != nil && existing.UserID == p.UserID &&
				existing.ActivityID != nil && *existing.ActivityID == p.ActivityID &&
				existing.PurchaseSlot != nil && *existing.PurchaseSlot == p.PurchaseSlot &&
				existing.OrderType == model.OrderTypeSeckill {
				return false, nil
			}
			return false, ErrSeckillOrderConflict
		}
		return false, err
	}
	if err := s.store.Items.Create(ctx, tx, &item); err != nil {
		return false, err
	}
	return true, nil
}

func (s *orderService) PrepareSeckillOrder(ctx context.Context, userID, skuID int64) (*SeckillOrderSnapshot, error) {
	if userID <= 0 || skuID <= 0 {
		return nil, fmt.Errorf("%w: invalid seckill snapshot identity", ErrInvalidInput)
	}
	address, err := s.users.GetDefaultAddress(ctx, userID)
	if err != nil {
		if errors.Is(err, usersvc.ErrAddressNotFound) {
			return nil, ErrAddressNotFound
		}
		return nil, err
	}
	if address == nil {
		return nil, ErrAddressNotFound
	}
	sku, err := s.products.GetSKU(ctx, skuID)
	if err != nil {
		return nil, translateProductError(err)
	}
	if sku == nil {
		return nil, ErrSKUNotFound
	}
	product, err := s.products.GetProduct(ctx, sku.ProductID)
	if err != nil {
		return nil, translateProductError(err)
	}
	if product == nil {
		return nil, ErrSKUUnavailable
	}
	return &SeckillOrderSnapshot{
		SKUID: sku.ID, ProductID: sku.ProductID, ProductTitle: product.Title,
		Specs: append(json.RawMessage(nil), sku.Specs...), Address: *address,
	}, nil
}

// persistOrder 在同一事务内完成库存、券、订单、订单项与购物车清理。
func (s *orderService) persistOrder(ctx context.Context, tx *transaction.Handle, userID int64,
	snapshots []lineSnapshot, order *model.Order,
	items []model.OrderItem, cartItemIDs []int64) error {
	if err := validateAmountConsistency(order, items); err != nil {
		return err
	}
	for _, sn := range snapshots {
		ok, err := s.products.DeductStock(ctx, tx, sn.skuID, sn.quantity)
		if err != nil {
			return translateProductError(err)
		}
		if !ok {
			return ErrInsufficientStock
		}
	}
	if err := s.store.Orders.Create(ctx, tx, order); err != nil {
		return err
	}
	for i := range items {
		if err := s.store.Items.Create(ctx, tx, &items[i]); err != nil {
			return err
		}
	}
	if len(cartItemIDs) > 0 {
		if err := s.cart.DeletePurchased(ctx, tx, userID, cartItemIDs); err != nil {
			return err
		}
	}
	return nil
}

// acquireIdempotency 生成雪花订单号并原子抢占幂等键（SETNX+EX）；
// 返回 (订单号, 是否抢占成功)。未抢占成功时订单号为既有键中的值，
// 客户端据此查询/轮询同一订单（并发重复提交时订单可能尚未提交）。
// 注意：基础设施类失败（Redis 故障/时钟回拨）不包装为业务错误，
// 使调用方保持幂等键（防瞬时故障下重试生成第二单）。
func (s *orderService) acquireIdempotency(ctx context.Context, userID int64, clientRequestID string) (string, bool, error) {
	no, err := s.nos.Next()
	if err != nil {
		return "", false, fmt.Errorf("generate order no: %w", err)
	}
	orderNo := strconv.FormatInt(no, 10)

	key := idemKey(userID, clientRequestID)
	result, err := s.cache.AcquireIdempotency(ctx, key, orderNo, idemTTL)
	if err != nil {
		return "", false, fmt.Errorf("acquire idempotency key: %w", err)
	}
	if result == cache.IdempotencyAcquired {
		return orderNo, true, nil
	}
	if result != cache.IdempotencyExists {
		return "", false, fmt.Errorf("unexpected idempotency result: %d", result)
	}
	// 已存在：返回既有订单号（同一 client_request_id 复用同一订单）。
	raw, err := s.cache.Get(ctx, key)
	if err != nil {
		return "", false, err
	}
	if raw == "" {
		return "", false, fmt.Errorf("idempotency key %s empty", key)
	}
	return raw, false, nil
}

// replayIdempotent 命中幂等键：返回既有订单（同一订单号）。
// 订单号已落键但订单尚未提交（并发在途，或首提遇基础设施故障且幂等键保留）时，
// 返回仅含 order_no 的订单视图（status 为空即"未落库"标记），
// 客户端应轮询 GET /api/orders/{order_no} 直至状态非空。
func (s *orderService) replayIdempotent(ctx context.Context, orderNo string) (*CreateResult, error) {
	view, err := s.loadView(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return &CreateResult{
			Order:      &model.OrderView{Order: model.Order{OrderNo: orderNo}},
			Idempotent: true,
			Processing: true,
		}, nil
	}
	return &CreateResult{Order: view, Idempotent: true}, nil
}

func (s *orderService) replayNormalRequest(ctx context.Context, userID int64, clientRequestID string) (*CreateResult, error) {
	order, err := s.store.Orders.GetNormalByClientRequestID(ctx, userID, clientRequestID)
	if err != nil || order == nil {
		return nil, err
	}
	items, err := s.store.Items.ListByOrder(ctx, order.OrderNo)
	if err != nil {
		return nil, err
	}
	return &CreateResult{
		Order:      &model.OrderView{Order: *order, Items: items},
		Idempotent: true,
	}, nil
}

// translateCouponError 跨模块错误翻译为本模块业务错误（handler 据此映射 HTTP 状态码）。
func translateCouponError(err error) error {
	switch {
	case errors.Is(err, couponsvc.ErrCouponNotFound):
		return ErrCouponNotFound
	case errors.Is(err, couponsvc.ErrCouponUsed):
		return ErrCouponUsed
	case errors.Is(err, couponsvc.ErrCouponExpired):
		return ErrCouponExpired
	case errors.Is(err, couponsvc.ErrCouponThresholdNotMet):
		return ErrCouponThresholdNotMet
	case errors.Is(err, couponsvc.ErrCouponRollbackFailed):
		return fmt.Errorf("%w: %v", ErrCouponUsed, err)
	}
	return err
}

func validateItems(p *CreateParams) error {
	if p.FromCart {
		if len(p.Items) > 0 {
			return fmt.Errorf("%w: from_cart and items are mutually exclusive", ErrInvalidInput)
		}
		return nil
	}
	if len(p.Items) == 0 {
		return fmt.Errorf("%w: empty items", ErrInvalidInput)
	}
	// 直购同 SKU 多行合并为一行：同一 SKU 只扣一次库存、只生成一条订单项。
	merged := make(map[int64]int, len(p.Items))
	for _, it := range p.Items {
		if it.SKUID <= 0 {
			return fmt.Errorf("%w: invalid sku id", ErrInvalidInput)
		}
		if it.Quantity < 1 || it.Quantity > 99 {
			return fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
		}
		merged[it.SKUID] += it.Quantity
	}
	items := make([]ItemParams, 0, len(merged))
	for skuID, qty := range merged {
		// 合并后超过上限：明确拒绝（金额敏感路径不做静默裁剪）。
		if qty > 99 {
			return fmt.Errorf("%w: merged quantity exceeds 99", ErrInvalidInput)
		}
		items = append(items, ItemParams{SKUID: skuID, Quantity: qty})
	}
	p.Items = items
	return nil
}

// collectLines 购物车结算：读取购物车全部条目；直购：透传请求项。
// directLines 将直购请求转换为统一下单行；购物车行只在事务内经 LockItems 读取。
func directLines(p *CreateParams) []orderLine {
	lines := make([]orderLine, 0, len(p.Items))
	for _, it := range p.Items {
		lines = append(lines, orderLine{skuID: it.SKUID, quantity: it.Quantity})
	}
	return lines
}

func calculateAmounts(total int64, coupon *couponmodel.CouponRedemption) (int64, int64, error) {
	discount := int64(0)
	if coupon != nil {
		discount = coupon.Value
		if discount > total {
			discount = total
		}
	}
	return discount, total - discount, nil
}

// translateProductError 将事务内商品服务错误翻译为订单模块错误，避免 HTTP 500。
func translateProductError(err error) error {
	switch {
	case errors.Is(err, productsvc.ErrSKUNotFound):
		return ErrSKUNotFound
	case errors.Is(err, productsvc.ErrProductNotFound):
		return ErrSKUUnavailable
	}
	return err
}

// loadSnapshots 读取 SKU 价格并校验存在/上架（GetDetail 仅上架可见，404 即下架），
// 累计商品总额，产出订单项快照。
func (s *orderService) loadSnapshots(ctx context.Context, tx *transaction.Handle, lines []orderLine) ([]lineSnapshot, int64, error) {
	type candidate struct {
		line      orderLine
		productID int64
	}
	candidates := make([]candidate, 0, len(lines))
	for _, line := range lines {
		sku, err := s.products.GetSKU(ctx, line.skuID)
		if err != nil {
			return nil, 0, translateProductError(err)
		}
		if sku == nil {
			return nil, 0, ErrSKUNotFound
		}
		candidates = append(candidates, candidate{line: line, productID: sku.ProductID})
	}
	// 先按商品再按 SKU 排序；GetSKUForUpdate 按 product → SKU 加锁，
	// 多商品订单之间使用全局一致的锁顺序，避免死锁。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].productID != candidates[j].productID {
			return candidates[i].productID < candidates[j].productID
		}
		return candidates[i].line.skuID < candidates[j].line.skuID
	})

	snapshots := make([]lineSnapshot, 0, len(candidates))
	var total int64
	for _, candidate := range candidates {
		l := candidate.line
		sku, err := s.products.GetSKUForUpdate(ctx, tx, l.skuID)
		if err != nil {
			if errors.Is(err, productsvc.ErrSKUNotFound) {
				return nil, 0, ErrSKUNotFound
			}
			if errors.Is(err, productsvc.ErrProductNotFound) {
				return nil, 0, ErrSKUUnavailable
			}
			return nil, 0, err
		}
		if sku == nil {
			return nil, 0, ErrSKUNotFound
		}
		if sku.Price < 0 || sku.Price > productmodel.MaxPriceCents {
			return nil, 0, fmt.Errorf("%w: invalid sku price", ErrInvalidInput)
		}
		subtotal, err := checkedAmountMul(sku.Price, l.quantity)
		if err != nil {
			return nil, 0, err
		}
		total, err = checkedAmountAdd(total, subtotal)
		if err != nil {
			return nil, 0, err
		}
		detail, err := s.products.GetDetail(ctx, sku.ProductID)
		if err != nil {
			if errors.Is(err, productsvc.ErrProductNotFound) {
				return nil, 0, ErrSKUUnavailable
			}
			return nil, 0, err
		}
		snapshots = append(snapshots, lineSnapshot{
			skuID:     sku.ID,
			productID: sku.ProductID,
			title:     detail.Product.Title,
			specs:     sku.Specs,
			price:     sku.Price,
			quantity:  l.quantity,
			subtotal:  subtotal,
		})
	}
	return snapshots, total, nil
}

// buildOrder 组装订单与订单项（含地址快照与金额）。
func buildOrder(userID int64, orderNo, clientRequestID string, address *usermodel.Address,
	snapshots []lineSnapshot, total, discount, pay int64, coupon *couponmodel.CouponRedemption) (*model.Order, []model.OrderItem) {

	order := &model.Order{
		OrderNo:         orderNo,
		UserID:          userID,
		ClientRequestID: &clientRequestID,
		OrderType:       model.OrderTypeNormal,
		Status:          model.OrderStatusPendingPayment,
		TotalAmount:     total,
		DiscountAmount:  discount,
		PayAmount:       pay,
		Receiver:        address.Receiver,
		Phone:           address.Phone,
		Province:        address.Province,
		City:            address.City,
		District:        address.District,
		Detail:          address.Detail,
		ExpireAt:        time.Now().Add(normalExpire),
	}
	if coupon != nil {
		order.CouponID = &coupon.CouponID
	}

	items := make([]model.OrderItem, 0, len(snapshots))
	for _, sn := range snapshots {
		items = append(items, model.OrderItem{
			OrderNo:   orderNo,
			SKUID:     sn.skuID,
			ProductID: sn.productID,
			Title:     sn.title,
			Specs:     sn.specs,
			Price:     sn.price,
			Quantity:  sn.quantity,
			Subtotal:  sn.subtotal,
		})
	}
	return order, items
}

// isValidationError 校验类业务错误：客户端修正输入后重试即可；
// 其余（基础设施/未知）错误保留幂等键，防重复下单。
func isValidationError(err error) bool {
	switch {
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrCartEmpty),
		errors.Is(err, ErrInsufficientStock), errors.Is(err, ErrSKUNotFound),
		errors.Is(err, ErrSKUUnavailable), errors.Is(err, ErrCouponNotFound),
		errors.Is(err, ErrCouponUsed), errors.Is(err, ErrCouponExpired),
		errors.Is(err, ErrCouponThresholdNotMet), errors.Is(err, ErrAddressNotFound),
		errors.Is(err, ErrAddressForbidden):
		return true
	}
	return false
}
