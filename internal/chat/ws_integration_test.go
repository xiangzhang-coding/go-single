// T18 消息实时通道（WS）集成测试（主 seam）：真实 MySQL + httptest 起完整路由
// （user + social + chat + /ws），覆盖三项验收：
// ① 双方在线实时互聊（发送走 REST，接收走 WS 推送 new_message）；
// ② 未授权连接被拒（缺 token / 非法 token 握手前 401）；
// ③ 心跳保活（Ping/Pong 周期维持连接）+ 断线重连消息不丢
//
//	（离线期间落库，重连后 REST after_id 补拉）。
//
// 另覆盖：仅接收方收到推送（发送方不收到自己消息的回推）、
// 同一用户多端在线全部收到、死连接（停止回应 Pong）被服务端剔除。
//
// 说明：gorilla 客户端仅在读取时处理控制帧（Ping 自动回 Pong），
// 测试客户端带后台读循环（wsClient），与真实浏览器客户端行为一致；
// 读循环不因业务消息积压而阻塞（缓冲 256），保证保活持续处理。
package chat_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
)

// wsClient 测试客户端：后台读循环，数据消息进 msgs、连接错误进 errs。
type wsClient struct {
	conn *websocket.Conn
	msgs chan []byte
	errs chan error
}

// dialWS 起真实 HTTP 服务并以指定 token 拨号 /ws；握手失败（401）返回 nil。
// setup 在读循环启动前执行。
func dialWS(t *testing.T, env *testEnv, token string, setup ...func(*websocket.Conn)) *wsClient {
	t.Helper()
	srv := httptest.NewServer(env.router)
	t.Cleanup(srv.Close)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws?token="+url.QueryEscape(token), nil)
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

// nextEvent 读取一条推送并解码为事件信封。
func (c *wsClient) nextEvent(t *testing.T) (string, map[string]any) {
	t.Helper()
	select {
	case p := <-c.msgs:
		var ev ws.Event
		require.NoError(t, json.Unmarshal(p, &ev))
		data, _ := ev.Data.(map[string]any)
		return ev.Event, data
	case <-time.After(3 * time.Second):
		t.Fatal("等待推送超时")
		return "", nil
	}
}

// expectNoPush 断言一段时间内不会收到任何业务推送。
func (c *wsClient) expectNoPush(t *testing.T) {
	t.Helper()
	select {
	case p := <-c.msgs:
		t.Fatalf("不应收到推送: %s", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// ---- 验收 ②：未授权连接被拒 ----

func TestWSHandshakeRejectsUnauthorized(t *testing.T) {
	env := requireEnv(t)

	require.Nil(t, dialWS(t, env, ""), "缺 token 的握手应被拒")
	require.Nil(t, dialWS(t, env, "not-a-jwt"), "非法 token 的握手应被拒")
}

// ---- 验收 ①：双方在线实时互聊 ----

func TestWSRealtimeChat(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_ws")
	bobID, bobToken := register(t, env, "bob_ws")
	befriend(t, env, aliceToken, bobID, bobToken)

	bobWS := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS)
	aliceWS := dialWS(t, env, aliceToken)
	require.NotNil(t, aliceWS)

	// alice 经 REST 发送 → bob 在线即时收到 new_message（消息内容与落库一致）。
	msg := sendMsg(t, env, aliceToken, bobID, "text", "实时消息", "", "")
	event, data := bobWS.nextEvent(t)
	require.Equal(t, ws.EventNewMessage, event)
	require.Equal(t, float64(aliceID), data["sender_id"])
	require.Equal(t, float64(bobID), data["recipient_id"])
	require.Equal(t, "实时消息", data["content"])
	require.Equal(t, msg["id"], data["id"], "推送与落库为同一消息")

	// 发送方（alice）不收到自己消息的回推。
	aliceWS.expectNoPush(t)

	// 反向同样实时：bob 回一条 → alice 收到。
	_ = sendMsg(t, env, bobToken, aliceID, "text", "收到", "", "")
	event, data = aliceWS.nextEvent(t)
	require.Equal(t, ws.EventNewMessage, event)
	require.Equal(t, float64(bobID), data["sender_id"])
	require.Equal(t, "收到", data["content"])
}

func TestWSPushToAllDevices(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_md")
	bobID, bobToken := register(t, env, "bob_md")
	befriend(t, env, aliceToken, bobID, bobToken)

	// bob 双端在线：两连接都收到同一条推送。
	bobWS1 := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS1)
	bobWS2 := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS2)

	_ = sendMsg(t, env, aliceToken, bobID, "text", "多端推送", "", "")
	for i, c := range []*wsClient{bobWS1, bobWS2} {
		event, data := c.nextEvent(t)
		require.Equal(t, ws.EventNewMessage, event, "第 %d 个连接", i)
		require.Equal(t, "多端推送", data["content"])
	}
}

// ---- 验收 ③：心跳保活 ----

func TestWSHeartbeatKeepAlive(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_hb")
	bobID, bobToken := register(t, env, "bob_hb")
	befriend(t, env, aliceToken, bobID, bobToken)

	bobWS := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS)

	// 读循环自动回 Pong：跨多个 Ping 周期（40ms × 8 = 320ms）连接存活。
	time.Sleep(320 * time.Millisecond)
	require.Eventually(t, func() bool { return env.hub.ConnectedCount() == 1 }, 2*time.Second, 20*time.Millisecond,
		"心跳保活下连接应保持在线")

	// 存活期间推送照常送达。
	_ = sendMsg(t, env, aliceToken, bobID, "text", "还活着吗", "", "")
	event, data := bobWS.nextEvent(t)
	require.Equal(t, ws.EventNewMessage, event)
	require.Equal(t, "还活着吗", data["content"])
}

// ---- 死连接剔除：客户端停止回应 Pong → 服务端判定断开 ----

func TestWSDeadClientRemoved(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_dc")
	bobID, bobToken := register(t, env, "bob_dc")
	befriend(t, env, aliceToken, bobID, bobToken)

	// 覆盖自动回 Pong 的默认 PingHandler 为静默：模拟死连接/网络分区
	// （须在读循环启动前设置，避免与读循环竞态）。
	bobWS := dialWS(t, env, bobToken, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error { return nil })
	})
	require.NotNil(t, bobWS)

	// pong_wait = 2×40ms；服务端应剔除该连接。
	require.Eventually(t, func() bool { return env.hub.ConnectedCount() == 0 }, 3*time.Second, 20*time.Millisecond,
		"停止回应 Pong 的死连接应被服务端关闭")
	select {
	case <-bobWS.errs:
	case <-time.After(2 * time.Second):
		t.Fatal("服务端应主动关闭该连接")
	}

	// 剔除后推送不再投递（离线由 REST 补拉兜底，见下一测试）。
	_ = sendMsg(t, env, aliceToken, bobID, "text", "死连接后", "", "")
	bobWS.expectNoPush(t)
}

// ---- 验收 ③：断线重连后消息不丢（落库兜底 + 上线 REST 补拉） ----

func TestWSReconnectNoMessageLoss(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_rl")
	bobID, bobToken := register(t, env, "bob_rl")
	befriend(t, env, aliceToken, bobID, bobToken)
	key := conversationKeyOf(aliceID, bobID)

	// 第一段在线：bob 连接后主动断开（断线）。
	bobWS := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS)
	require.NoError(t, bobWS.conn.Close())
	require.Eventually(t, func() bool { return env.hub.ConnectedCount() == 0 }, 3*time.Second, 20*time.Millisecond,
		"断开后应注销")

	// 断线期间 alice 连发 2 条：落库、无 WS 可投递（不丢）。
	for i := 0; i < 2; i++ {
		sendMsg(t, env, aliceToken, bobID, "text", fmt.Sprintf("断线期间%d", i), "", "")
	}

	// 重连：新连接建立，历史消息不补推（推送仅实时），经 REST after_id 拉新补回。
	bobWS = dialWS(t, env, bobToken)
	require.NotNil(t, bobWS)
	bobWS.expectNoPush(t)

	items, _ := messagesOf(t, env, bobToken, key, "?limit=20")
	require.Len(t, items, 2, "断线期间消息已落库，重连后补拉不丢")
	lastID := int64(items[1].(map[string]any)["id"].(float64))
	items, hasMore := messagesOf(t, env, bobToken, key, fmt.Sprintf("?after_id=%d", lastID))
	require.False(t, hasMore)
	require.Empty(t, items)

	// 重连后实时通道恢复。
	_ = sendMsg(t, env, aliceToken, bobID, "text", "重连后", "", "")
	event, data := bobWS.nextEvent(t)
	require.Equal(t, ws.EventNewMessage, event)
	require.Equal(t, "重连后", data["content"])
}

// TestWSHubConcurrentPush 并发发送下推送与心跳/读泵并行，无丢失、无数据竞争（-race 覆盖）。
func TestWSHubConcurrentPush(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_cp2")
	bobID, bobToken := register(t, env, "bob_cp2")
	befriend(t, env, aliceToken, bobID, bobToken)

	bobWS := dialWS(t, env, bobToken)
	require.NotNil(t, bobWS)

	// 并发发送 20 条（推送/心跳/读泵并行），全部送达。
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			// 注意：子协程内不得使用 require（FailNow 仅限测试协程），错误经 channel 回传。
			body := fmt.Sprintf(`{"to_user_id":%d,"type":"text","content":"并发%d"}`, bobID, i)
			r := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+aliceToken)
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, r)
			if w.Code != http.StatusCreated {
				done <- fmt.Errorf("第 %d 条发送失败: %s", i, w.Body.String())
				return
			}
		}
		done <- nil
	}()

	for i := 0; i < 20; i++ {
		event, data := bobWS.nextEvent(t)
		require.Equal(t, ws.EventNewMessage, event, "第 %d 条", i)
		require.Equal(t, fmt.Sprintf("并发%d", i), data["content"])
	}
	require.NoError(t, <-done)
}
