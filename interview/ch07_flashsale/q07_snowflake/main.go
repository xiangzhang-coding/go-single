// Q7 雪花 ID：全局唯一、趋势递增、无中心依赖。
// 运行：go run ./interview/ch07_flashsale/q07_snowflake
package main

import (
	"fmt"
	"sync"
	"time"
)

// 与项目 internal/platform/snowflake 同构的简化版：
// 41bit 毫秒时间戳 + 10bit 机器位 + 12bit 序号；同毫秒序号耗尽则自旋等下一毫秒。
const (
	epoch      = 1704067200000 // 2024-01-01
	machineBit = 10
	seqBit     = 12
)

type snowflake struct {
	mu      sync.Mutex
	machine int64
	lastMS  int64
	seq     int64
}

func (s *snowflake) Next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if now == s.lastMS {
		s.seq = (s.seq + 1) & (1<<seqBit - 1)
		for s.seq == 0 && time.Now().UnixMilli() == s.lastMS {
			time.Sleep(100 * time.Microsecond) // 序号耗尽自旋
		}
		now = time.Now().UnixMilli()
	} else {
		s.seq = 0
	}
	s.lastMS = now
	return (now-epoch)<<(machineBit+seqBit) | s.machine<<seqBit | s.seq
}

func main() {
	s := &snowflake{machine: 1}
	seen := map[int64]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ { // 并发 100 次生成
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := s.Next()
			mu.Lock()
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	fmt.Printf("生成 %d 个 ID，全部唯一: %v\n", len(seen), len(seen) == 100)
	fmt.Println("特性：趋势递增（利于索引）、可解码时间（取高 41bit）")
}

// 项目位置：internal/platform/snowflake/snowflake.go（41+10+12 位布局、时钟回拨
// ErrClockBackward 防御）；订单号/支付号由它生成（order/payment service）。
