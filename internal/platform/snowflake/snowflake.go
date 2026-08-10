// Package snowflake 手写雪花 ID 生成器（学习点）：
// 41bit 毫秒时间戳（相对 2024-01-01 纪元）+ 10bit 机器/工作 ID + 12bit 序列号，
// 合计 63bit 落在 int64 内。单实例单调递增；时钟回拨拒绝生成（返回错误）；
// 同一毫秒序列号耗尽时自旋等待下一毫秒。
package snowflake

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// epochStart 纪元：2024-01-01T00:00:00Z（41bit ≈ 69 年）。
	epochStart int64 = 1704067200000
	// workerBits 机器/工作 ID 位数（0~1023）。
	workerBits int64 = 10
	// sequenceBits 同一毫秒序列号位数（0~4095）。
	sequenceBits int64 = 12
	// maxWorkerID 工作 ID 上限。
	maxWorkerID int64 = -1 ^ (-1 << workerBits)
	// maxSequence 序列号上限。
	maxSequence int64 = -1 ^ (-1 << sequenceBits)
	// workerShift 序列号在最终 ID 中的位移。
	workerShift = sequenceBits
	// timestampShift 时间戳在最终 ID 中的位移。
	timestampShift = workerBits + sequenceBits
)

// ErrClockBackward 系统时钟回拨，拒绝生成（等待时钟恢复）。
var ErrClockBackward = errors.New("snowflake: clock moved backwards")

// IDGenerator 雪花 ID 生成器；并发安全（互斥保护时间戳与序列号）。
type IDGenerator struct {
	mu       sync.Mutex
	workerID int64
	lastTS   int64
	sequence int64
}

// New 构造生成器；workerID 超出 [0, 1023] 返回错误。
func New(workerID int64) (*IDGenerator, error) {
	if workerID < 0 || workerID > maxWorkerID {
		return nil, fmt.Errorf("snowflake: worker id %d out of range [0, %d]", workerID, maxWorkerID)
	}
	return &IDGenerator{workerID: workerID, lastTS: -1}, nil
}

// Next 生成下一个 ID：时间戳取毫秒；同一毫秒内序列号递增，
// 耗尽后自旋等待下一毫秒；时钟回拨返回 ErrClockBackward。
func (g *IDGenerator) Next() (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UnixMilli() - epochStart
	if now < g.lastTS {
		return 0, fmt.Errorf("%w (last %d, now %d)", ErrClockBackward, g.lastTS, now)
	}

	if now == g.lastTS {
		g.sequence++
		if g.sequence > maxSequence {
			// 序列号耗尽：自旋等待下一毫秒。
			for now <= g.lastTS {
				now = time.Now().UnixMilli() - epochStart
				if now < g.lastTS {
					return 0, fmt.Errorf("%w (last %d, now %d)", ErrClockBackward, g.lastTS, now)
				}
				time.Sleep(time.Millisecond)
			}
			g.sequence = 0
		}
	} else {
		g.sequence = 0
	}

	g.lastTS = now
	return (now << timestampShift) | (g.workerID << workerShift) | g.sequence, nil
}
