// snowflake 单元测试：结构合法性（时间戳/工作 ID/序列号三字段还原）、
// 严格单调递增、同一毫秒序列号进位、并发安全与非法工作 ID 拒绝。
package snowflake

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// 还原各字段：高 41bit 为毫秒时间戳，中 10bit 为工作 ID，低 12bit 为序列号。
func fields(id int64) (ts, worker, seq int64) {
	return id >> timestampShift, (id >> workerShift) & maxWorkerID, id & maxSequence
}

func TestNewRejectsBadWorkerID(t *testing.T) {
	_, err := New(-1)
	require.Error(t, err)
	_, err = New(1024)
	require.Error(t, err)
}

func TestNextMonotonicAndStructure(t *testing.T) {
	g, err := New(7)
	require.NoError(t, err)

	var prev int64 = -1
	for i := 0; i < 1000; i++ {
		id, err := g.Next()
		require.NoError(t, err)
		require.True(t, id > prev, "ID 必须严格递增")
		prev = id

		_, worker, seq := fields(id)
		require.Equal(t, int64(7), worker, "工作 ID 位应还原为 7")
		require.True(t, seq <= maxSequence)
		require.GreaterOrEqual(t, id, int64(0))
	}
}

// 同一毫秒内连续生成：序列号应随生成次数递增（至少覆盖一次序列号非零）。
func TestNextSequenceAdvances(t *testing.T) {
	g, err := New(1)
	require.NoError(t, err)

	first, err := g.Next()
	require.NoError(t, err)
	second, err := g.Next()
	require.NoError(t, err)

	_, _, seq1 := fields(first)
	_, _, seq2 := fields(second)
	require.Equal(t, seq1+1, seq2, "同一毫秒内序列号应递增")
}

// 不同工作 ID 生成器产生的 ID 互不相同（时间戳与序列号相同也不冲突）。
func TestWorkerBitsSeparate(t *testing.T) {
	ga, err := New(0)
	require.NoError(t, err)
	gb, err := New(1)
	require.NoError(t, err)

	a, _ := ga.Next()
	b, _ := gb.Next()
	require.NotEqual(t, a, b)
}

// 并发安全：多 goroutine 下所有 ID 唯一。
func TestNextConcurrentUnique(t *testing.T) {
	g, err := New(3)
	require.NoError(t, err)

	const n = 2000
	ids := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n/8; j++ {
				id, err := g.Next()
				require.NoError(t, err)
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]bool, n)
	for id := range ids {
		require.False(t, seen[id], "并发生成的 ID 必须唯一")
		seen[id] = true
	}
	require.Len(t, seen, n)
}
