// ws Hub 单元测试（无外部依赖）：httptest + gorilla 真实连接，
// 覆盖注册/注销、按用户推送（多连接、离线无操作）、慢消费者剔除、
// 心跳保活与死连接剔除、并发推送（-race）、Close 全量断开。
//
// 说明：gorilla 客户端仅在读取时处理控制帧（Ping 自动回 Pong），
// 故测试客户端带后台读循环（wsClient），与真实浏览器客户端行为一致。
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

const (
	heartbeat       = 30 * time.Millisecond
	writeWait       = 2 * time.Second
	receiveDeadline = 3 * time.Second
)

// fakeVerifier 接受测试 token，并为短令牌返回固定截止时间。
type fakeVerifier struct{ shortExpiry time.Time }

func (v fakeVerifier) Verify(_ context.Context, token string) (*auth.Claims, error) {
	switch token {
	case "valid-token":
		return &auth.Claims{UserID: 42, ExpiresAt: time.Now().Add(time.Hour)}, nil
	case "user-43-token":
		return &auth.Claims{UserID: 43, ExpiresAt: time.Now().Add(time.Hour)}, nil
	case "user-44-token":
		return &auth.Claims{UserID: 44, ExpiresAt: time.Now().Add(time.Hour)}, nil
	case "short-token":
		return &auth.Claims{UserID: 42, ExpiresAt: v.shortExpiry}, nil
	case "no-expiration-token":
		return &auth.Claims{UserID: 42}, nil
	default:
		return nil, auth.ErrInvalidToken
	}
}

// newTestHub 构造带短心跳的 Hub 与路由。
func newTestHub(t *testing.T) (*Hub, http.Handler) {
	t.Helper()
	return newTestHubWithConfig(t, Config{})
}

func newTestHubWithConfig(t *testing.T, cfg Config) (*Hub, http.Handler) {
	t.Helper()
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = heartbeat
	}
	if cfg.WriteWait == 0 {
		cfg.WriteWait = writeWait
	}
	hub := New(cfg, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies([]string{"127.0.0.1"}))
	r.GET("/ws", hub.Handler(fakeVerifier{shortExpiry: time.Now().Add(500 * time.Millisecond)}))
	return hub, r
}

func dialHub(t *testing.T, handler http.Handler, token, sourceIP string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	protocols := []string{authSubprotocol}
	if token != "" {
		protocols = append(protocols, token)
	}
	header := http.Header{}
	if sourceIP != "" {
		header.Set("X-Forwarded-For", sourceIP)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: protocols}
	return dialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws", header)
}

func dialRejected(t *testing.T, handler http.Handler, token string, status int) string {
	t.Helper()
	conn, resp, err := dialHub(t, handler, token, "")
	if conn != nil {
		conn.Close()
	}
	require.Error(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	require.Equal(t, status, resp.StatusCode)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	return string(body)
}

// wsClient 测试客户端：后台读循环（控制帧自动处理，Ping→Pong 维持保活），
// 数据消息进 msgs、连接错误进 errs。
type wsClient struct {
	conn *websocket.Conn
	msgs chan []byte
	errs chan error
}

// dialClient 拨号测试路由；token 非法时断言握手 401 并返回 nil。
// setup 在读循环启动前执行（如覆盖 PingHandler 模拟死连接）。
func dialClient(t *testing.T, handler http.Handler, token string, setup ...func(*websocket.Conn)) *wsClient {
	t.Helper()
	return dialClientFromIP(t, handler, token, "", setup...)
}

func dialClientFromIP(t *testing.T, handler http.Handler, token, sourceIP string, setup ...func(*websocket.Conn)) *wsClient {
	t.Helper()
	conn, resp, err := dialHub(t, handler, token, sourceIP)
	if err != nil {
		if resp != nil {
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "握手失败: %v", err)
		}
		return nil
	}
	for _, fn := range setup {
		fn(conn)
	}

	c := &wsClient{conn: conn, msgs: make(chan []byte, 256), errs: make(chan error, 1)}
	go func() {
		for {
			_, p, err := conn.ReadMessage()
			if err != nil {
				c.errs <- err
				return
			}
			c.msgs <- p
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return c
}

// nextEvent 读取一条推送并解码为事件信封（数据保持原类型）。
func (c *wsClient) nextEvent(t *testing.T) (string, any) {
	t.Helper()
	select {
	case p := <-c.msgs:
		var ev Event
		require.NoError(t, json.Unmarshal(p, &ev))
		return ev.Event, ev.Data
	case <-time.After(receiveDeadline):
		t.Fatal("等待推送超时")
		return "", nil
	}
}

// expectNoData 断言一段时间内不出现业务推送。
func (c *wsClient) expectNoData(t *testing.T, dur time.Duration) {
	t.Helper()
	select {
	case p := <-c.msgs:
		t.Fatalf("不应收到推送: %s", p)
	case <-time.After(dur):
	}
}

func TestHubHandshakeRequiresToken(t *testing.T) {
	_, handler := newTestHub(t)
	require.Nil(t, dialClient(t, handler, ""), "缺 token 握手应被拒")
	require.Nil(t, dialClient(t, handler, "invalid-token"), "非法 token 握手应被拒")
	require.Nil(t, dialClient(t, handler, "no-expiration-token"), "缺过期时间的 token 握手应被拒")

	srv := httptest.NewServer(handler)
	defer srv.Close()
	_, resp, err := websocket.DefaultDialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws?token=valid-token", nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "query token 不再作为凭据")
}

func TestHubClosesConnectionWhenAuthorizationExpires(t *testing.T) {
	hub, handler := newTestHub(t)
	client := dialClient(t, handler, "short-token")
	require.NotNil(t, client)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, time.Second, 10*time.Millisecond)
	pushStop := make(chan struct{})
	pushDone := make(chan struct{})
	defer func() {
		close(pushStop)
		<-pushDone
	}()
	go func() {
		defer close(pushDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				hub.PushToUser(42, EventNewMessage, "until-expiry")
			case <-pushStop:
				return
			}
		}
	}()

	select {
	case err := <-client.errs:
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		require.Equal(t, closeCodeTokenExpired, closeErr.Code)
	case <-time.After(time.Second):
		t.Fatal("token 到期后服务端未主动关闭连接")
	}
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 0 }, time.Second, 10*time.Millisecond)
	require.Nil(t, dialClient(t, handler, "short-token"), "重新连接必须重新鉴权")
}

func TestHubRegisterAndPush(t *testing.T) {
	hub, handler := newTestHub(t)
	client := dialClient(t, handler, "valid-token")
	require.NotNil(t, client)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	hub.PushToUser(42, EventNewMessage, map[string]any{"content": "hi"})
	event, data := client.nextEvent(t)
	require.Equal(t, EventNewMessage, event)
	require.Equal(t, map[string]any{"content": "hi"}, data)
}

func TestHubPushToAllConnsOfUser(t *testing.T) {
	hub, handler := newTestHub(t)
	c1 := dialClient(t, handler, "valid-token")
	c2 := dialClient(t, handler, "valid-token")
	require.NotNil(t, c1)
	require.NotNil(t, c2)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 2 }, 2*time.Second, 10*time.Millisecond)

	hub.PushToUser(42, EventNewMessage, "payload")
	for i, c := range []*wsClient{c1, c2} {
		event, data := c.nextEvent(t)
		require.Equal(t, EventNewMessage, event, "第 %d 个连接", i)
		require.Equal(t, "payload", data)
	}
}

func TestHubEnforcesConnectionLimitsAndReleasesSlots(t *testing.T) {
	t.Run("per user", func(t *testing.T) {
		hub, handler := newTestHubWithConfig(t, Config{
			MaxConnections:        10,
			MaxConnectionsPerUser: 2,
			MaxConnectionsPerIP:   10,
		})
		c1 := dialClient(t, handler, "valid-token")
		c2 := dialClient(t, handler, "valid-token")
		require.NotNil(t, c1)
		require.NotNil(t, c2)
		require.Eventually(t, func() bool { return hub.ConnectedCount() == 2 }, time.Second, 10*time.Millisecond)
		require.JSONEq(t, `{"error":"websocket connection limit exceeded","scope":"user"}`,
			dialRejected(t, handler, "valid-token", http.StatusTooManyRequests))

		require.NoError(t, c1.conn.Close())
		require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, time.Second, 10*time.Millisecond)
		replacement := dialClient(t, handler, "valid-token")
		require.NotNil(t, replacement, "连接关闭后必须释放用户配额")
	})

	t.Run("per source IP", func(t *testing.T) {
		_, handler := newTestHubWithConfig(t, Config{
			MaxConnections:        10,
			MaxConnectionsPerUser: 10,
			MaxConnectionsPerIP:   2,
		})
		require.NotNil(t, dialClient(t, handler, "valid-token"))
		require.NotNil(t, dialClient(t, handler, "user-43-token"))
		require.JSONEq(t, `{"error":"websocket connection limit exceeded","scope":"ip"}`,
			dialRejected(t, handler, "user-44-token", http.StatusTooManyRequests))

		_, independentHandler := newTestHubWithConfig(t, Config{
			MaxConnections:        10,
			MaxConnectionsPerUser: 10,
			MaxConnectionsPerIP:   1,
		})
		require.NotNil(t, dialClientFromIP(t, independentHandler, "valid-token", "198.51.100.1"))
		require.NotNil(t, dialClientFromIP(t, independentHandler, "user-43-token", "198.51.100.2"),
			"不同来源 IP 应各自拥有连接配额")
	})

	t.Run("global", func(t *testing.T) {
		_, handler := newTestHubWithConfig(t, Config{
			MaxConnections:        2,
			MaxConnectionsPerUser: 10,
			MaxConnectionsPerIP:   10,
		})
		require.NotNil(t, dialClient(t, handler, "valid-token"))
		require.NotNil(t, dialClient(t, handler, "user-43-token"))
		require.JSONEq(t, `{"error":"websocket connection limit exceeded","scope":"global"}`,
			dialRejected(t, handler, "user-44-token", http.StatusTooManyRequests))
	})
}

func TestHubConcurrentFloodCannotExceedGlobalLimit(t *testing.T) {
	const (
		limit    = 3
		attempts = 24
	)
	hub, handler := newTestHubWithConfig(t, Config{
		HeartbeatInterval:     2 * time.Second,
		MaxConnections:        limit,
		MaxConnectionsPerUser: attempts,
		MaxConnectionsPerIP:   attempts,
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	start := make(chan struct{})
	statuses := make(chan int, attempts)
	connections := make(chan *websocket.Conn, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{authSubprotocol, "valid-token"}}
			conn, resp, err := dialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws", nil)
			if err == nil {
				statuses <- http.StatusSwitchingProtocols
				connections <- conn
				return
			}
			if resp == nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	close(connections)

	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	require.Equal(t, limit, counts[http.StatusSwitchingProtocols])
	require.Equal(t, attempts-limit, counts[http.StatusTooManyRequests])
	require.LessOrEqual(t, hub.ConnectedCount(), limit)
	for conn := range connections {
		conn.Close()
	}
}

func TestHubPushToOfflineUserNoop(t *testing.T) {
	hub, handler := newTestHub(t)
	client := dialClient(t, handler, "valid-token")
	require.NotNil(t, client)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	// 离线用户（从未在线）：推送为无操作，不影响其他在线连接。
	hub.PushToUser(99, EventNewMessage, "x")
	hub.PushToUser(42, EventNewMessage, "still-online")
	event, data := client.nextEvent(t)
	require.Equal(t, EventNewMessage, event)
	require.Equal(t, "still-online", data)
}

func TestHubUnregisterOnClose(t *testing.T) {
	hub, handler := newTestHub(t)
	client := dialClient(t, handler, "valid-token")
	require.NotNil(t, client)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, client.conn.Close())
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 0 }, 2*time.Second, 10*time.Millisecond,
		"客户端断开后应注销")
}

func TestHubSlowConsumerClosed(t *testing.T) {
	hub, handler := newTestHub(t)
	// 不带读循环：客户端不读取，作为慢消费者。
	srv := httptest.NewServer(handler)
	defer srv.Close()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{authSubprotocol, "valid-token"}}
	conn, _, err := dialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 1 }, 2*time.Second, 10*time.Millisecond)

	// 客户端不读取 → 发送缓冲（64）耗尽即视为慢消费者被剔除。
	for i := 0; i < 128; i++ {
		hub.PushToUser(42, EventNewMessage, fmt.Sprintf("m%d", i))
	}
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 0 }, 2*time.Second, 10*time.Millisecond,
		"慢消费者应被关闭")

	// 排空已缓冲帧后应读到连接关闭错误。
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func TestHubHeartbeatKeepsAlive(t *testing.T) {
	hub, handler := newTestHub(t)
	client := dialClient(t, handler, "valid-token")
	require.NotNil(t, client)

	// 读循环自动回 Pong：跨多个心跳周期（30ms × 20 = 600ms）连接存活。
	time.Sleep(600 * time.Millisecond)
	require.Equal(t, 1, hub.ConnectedCount(), "心跳保活下连接应保持在线")

	hub.PushToUser(42, EventNewMessage, "alive")
	event, _ := client.nextEvent(t)
	require.Equal(t, EventNewMessage, event)
}

func TestHubDeadClientRemoved(t *testing.T) {
	hub, handler := newTestHub(t)
	// 覆盖自动回 Pong 的默认 PingHandler 为静默：模拟死连接/网络分区
	// （须在读循环启动前设置，避免与读循环竞态）。
	client := dialClient(t, handler, "valid-token", func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error { return nil })
	})
	require.NotNil(t, client)

	require.Eventually(t, func() bool { return hub.ConnectedCount() == 0 }, 3*time.Second, 10*time.Millisecond,
		"停止回应 Pong 的连接应被剔除（pong_wait = 2×心跳）")
	select {
	case <-client.errs:
	case <-time.After(2 * time.Second):
		t.Fatal("服务端应主动关闭该连接")
	}
}

func TestHubConcurrentPushRaceFree(t *testing.T) {
	hub, handler := newTestHub(t)
	clients := make([]*wsClient, 3)
	for i := range clients {
		clients[i] = dialClient(t, handler, "valid-token")
		require.NotNil(t, clients[i])
	}
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 3 }, 2*time.Second, 10*time.Millisecond)

	// 负载低于发送缓冲（64）上限，避免触发慢消费者剔除；
	// 目的为校验并发推送与心跳/读泵并行时无 panic/死锁/数据竞争（-race）。
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				hub.PushToUser(42, EventNewMessage, fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	time.Sleep(300 * time.Millisecond)
	wg.Wait()
	require.Equal(t, 3, hub.ConnectedCount())
}

func TestHubCloseAll(t *testing.T) {
	hub, handler := newTestHub(t)
	c1 := dialClient(t, handler, "valid-token")
	c2 := dialClient(t, handler, "valid-token")
	require.NotNil(t, c1)
	require.NotNil(t, c2)
	require.Eventually(t, func() bool { return hub.ConnectedCount() == 2 }, 2*time.Second, 10*time.Millisecond)

	hub.Close()
	require.Zero(t, hub.ConnectedCount(), "Close 返回前应等待写泵退出并清理 Hub 记录")
}

func TestHubHandlerRejectsNonUpgradeRequest(t *testing.T) {
	_, handler := newTestHub(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// 普通 GET（无 Upgrade 头）：升级失败返回 400，绝不返回 101。
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/ws", nil)
	require.NoError(t, err)
	req.Header.Set("Sec-WebSocket-Protocol", authSubprotocol+", valid-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
