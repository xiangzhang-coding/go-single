// Q7 熔断器：消费者挂掉时的自我保护（gobreaker 简化版）。
// 运行：go run ./interview/ch06_mq/q07_breaker
package main

import (
	"errors"
	"fmt"
	"time"
)

type state int

const (
	stClosed   state = iota // 正常：全部放行
	stOpen                  // 熔断：直接拒绝
	stHalfOpen              // 试探：放行一个，成功即恢复
)

type breaker struct {
	state     state
	failures  int
	threshold int
	openedAt  time.Time
	cooldown  time.Duration
}

func (b *breaker) allow() bool {
	switch b.state {
	case stClosed:
		return true
	case stOpen:
		if time.Since(b.openedAt) > b.cooldown {
			b.state = stHalfOpen
			return true // 半开试探
		}
		return false
	case stHalfOpen:
		return true
	}
	return false
}

func (b *breaker) onSuccess() { b.failures = 0; b.state = stClosed }

func (b *breaker) onFailure() {
	if b.state == stHalfOpen {
		b.state = stOpen
		b.openedAt = time.Now()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = stOpen
		b.openedAt = time.Now()
	}
}

func main() {
	b := &breaker{threshold: 3, cooldown: 50 * time.Millisecond}
	var err error
	for i := 1; i <= 6; i++ {
		if !b.allow() {
			fmt.Printf("第 %d 次: 熔断打开，直接返回 ErrCircuitOpen（重投）\n", i)
			continue
		}
		if err = consumeErr(); err != nil {
			b.onFailure()
			fmt.Printf("第 %d 次: 消费失败，连续失败 %d 次\n", i, b.failures)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if b.allow() {
		fmt.Println("冷却期后半开试探：放行一个请求")
	}
}

func consumeErr() error { return errors.New("rabbitmq channel closed") }

// 项目位置：internal/platform/mq/breaker.go 的 WrapCircuitBreaker（gobreaker，
// 配置 mq.circuit.*：连续 3 次失败、30s 间隔、10s 超时）；只包 Consume，Publish/Ping 直通；
// ErrPermanent 不计入失败（不该让数据问题触发熔断）。
