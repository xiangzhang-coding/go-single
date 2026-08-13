// Q7 跨模块事务：tx 参数汇入同一事务。
// 运行：go run ./interview/ch09_engineering/q07_tx_boundary
package main

import (
	"errors"
	"fmt"
)

// 事务句柄（简化版 *gorm.DB 的 Transaction）。
type Tx struct {
	ops []string
	ok  bool
}

func (t *Tx) begin() *Tx { return &Tx{ok: true} }
func (t *Tx) commit()    { t.ok = true }
func (t *Tx) rollback()  { t.ok = false; t.ops = nil }

// 跨模块写都接收 tx：order 调 coupon.UseCoupon(tx)、product.DeductStock(tx)……
type couponModule struct{}

func (couponModule) UseCoupon(tx *Tx) error {
	if tx == nil {
		return errors.New("必须汇入调用方事务！")
	}
	tx.ops = append(tx.ops, "UPDATE user_coupons SET status=used")
	return nil
}

type productModule struct{}

func (productModule) DeductStock(tx *Tx) error {
	tx.ops = append(tx.ops, "UPDATE skus SET stock=stock-1 WHERE stock>=1")
	return nil
}

func main() {
	// 下单事务：订单 + 订单项 + 扣库存 + 地址快照 + 券核销 + 清购物车。
	tx := (&Tx{}).begin()
	coupon := couponModule{}
	product := productModule{}
	_ = coupon.UseCoupon(tx)
	_ = product.DeductStock(tx)
	tx.ops = append(tx.ops, "INSERT orders ...")
	tx.commit()
	fmt.Printf("事务提交，ops=%d：跨模块写全部原子生效\n", len(tx.ops))
}

// 项目位置：internal/order/repository/order_repository.go 的 TxRunner.WithinTx；
// 服务层接口带 tx 参数（productSvc.GetSKUForUpdate/DeductStock、couponSvc.UseCoupon、
// flashsaleSvc.DeductStock——即 order 侧端口），见 order_service.go createOrder 事务体。
