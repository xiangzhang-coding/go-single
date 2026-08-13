// Q3 SETNX 幂等键：防重复提交。
// 运行：go run ./interview/ch05_redis/q03_setnx_idem
package main

import (
	"errors"
	"fmt"
	"sync"
)

// 内存版 SETNX + EXPIRE（对应项目 idemScript）。
type idemStore struct {
	mu   sync.Mutex
	keys map[string]bool
	ttl  map[string]int
}

func (s *idemStore) setnx(key string, ttl int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys[key] {
		return false // 已存在：重复请求
	}
	s.keys[key] = true
	s.ttl[key] = ttl
	return true
}

func (s *idemStore) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, key)
	delete(s.ttl, key)
}

func main() {
	idem := &idemStore{keys: map[string]bool{}, ttl: map[string]int{}}
	user, activity := int64(7), int64(1001)
	key := fmt.Sprintf("flashsale:idem:%d:%d", activity, user)

	// 第一次抢占成功；重复提交被挡（409）。
	got := idem.setnx(key, 1800)
	fmt.Println("首次抢占:", got)
	if !idem.setnx(key, 1800) {
		fmt.Println("重复提交 → 409 重复请求（ErrDuplicateRequest）")
	}
	// 业务拒绝（抢光/限购）后释放幂等键，允许窗口内重试。
	idem.del(key)
	fmt.Println("业务拒绝释放后再次抢占:", idem.setnx(key, 1800))
	_ = errors.New("unused")
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 idemScript（SETNX+EX 30min）
// 与 isBusinessReject——业务拒绝释放、基础设施失败保留（防瞬时故障重复预扣）；
// 订单创建幂等键同理（order:idem:{user}:{client_request_id}，TTL 15min）。
