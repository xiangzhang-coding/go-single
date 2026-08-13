// Q6 越权防护（IDOR）：所有资源访问都校验归属。
// 运行：go run ./interview/ch08_auth/q06_idor
package main

import (
	"errors"
	"fmt"
)

type order struct {
	ownerID int64
	no      string
}

var orders = map[string]order{"O1": {ownerID: 7, no: "O1"}, "O2": {ownerID: 8, no: "O2"}}

// 简化版 loadOwned：按订单号取订单，且必须属于当前用户。
func loadOwned(userID int64, orderNo string) (order, error) {
	o, ok := orders[orderNo]
	if !ok {
		return order{}, errors.New("404 订单不存在")
	}
	if o.ownerID != userID {
		return order{}, errors.New("403 越权：订单不属于你")
	}
	return o, nil
}

func main() {
	// 用户 7 访问自己的订单 OK；尝试访问用户 8 的订单被拒。
	if _, err := loadOwned(7, "O1"); err == nil {
		fmt.Println("访问自己的订单 → 200")
	}
	if _, err := loadOwned(7, "O2"); err != nil {
		fmt.Println("访问他人订单 →", err)
	}
}

// 项目位置：internal/order/service/order_service.go 的 loadOwned（1071-1083）；
// 同样的 owner 校验遍布 user（ensureOwned）、cart（ensureOwned）、friend
//（ensurePendingOwned）、chat（ensureAccessible 会话双方校验）。
