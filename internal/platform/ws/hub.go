// Package ws 提供 WebSocket 实时通道基础设施（T18）：
// Hub 管理在线连接（userID → 连接集合），PushToUser 向在线用户单向推送事件；
// 心跳保活（Ping/Pong，间隔配置化）；慢消费者关闭连接；
// 离线不丢消息由"消息落库 + 上线 REST 补拉"兜底（见 chat 模块，WS 仅实时推送）。
package ws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// 推送事件类型：客户端按 event 分发。data 由业务方给定（如 chat 模块的 Message）。
const (
	// EventNewMessage 新消息已落库，推送给在线接收方。
	EventNewMessage = "new_message"
	// closeCodeTokenExpired 表示当前连接的 JWT 授权寿命已结束。
	closeCodeTokenExpired = 4001
)

// Event 推送事件信封：{"event":"new_message","data":{...}}。
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// Config WS 通道配置。
type Config struct {
	// HeartbeatInterval 心跳 Ping 间隔（保活）；客户端两次 Pong 间隔内未响应即判定断开。
	HeartbeatInterval time.Duration
	// WriteWait 单条写（业务消息/Ping）超时。
	WriteWait time.Duration
	// AllowOrigins 握手允许的 Origin 白名单；空 = 允许所有（演示取舍，
	// 前端 VITE_WS_BASE 可跨源直连，生产应配置为前端域名）。
	AllowOrigins []string
	// MaxConnections 单进程允许的连接总数。
	MaxConnections int
	// MaxConnectionsPerUser 单用户允许的连接数，支持正常多设备登录。
	MaxConnectionsPerUser int
	// MaxConnectionsPerIP 单来源 IP 允许的连接数。
	MaxConnectionsPerIP int
}

const (
	// sendBuffer 每连接发送缓冲（慢消费者满即断开，客户端重连后 REST 补拉）。
	sendBuffer = 64
	// defaultHeartbeat 未配置时默认心跳间隔。
	defaultHeartbeat = 30 * time.Second
	// defaultWriteWait 未配置时默认写超时。
	defaultWriteWait             = 10 * time.Second
	defaultMaxConnections        = 1000
	defaultMaxConnectionsPerUser = 5
	defaultMaxConnectionsPerIP   = 50
	expiryCloseWait              = 100 * time.Millisecond
)

type rejectionScope string

const (
	rejectionScopeGlobal   rejectionScope = "global"
	rejectionScopeUser     rejectionScope = "user"
	rejectionScopeIP       rejectionScope = "ip"
	rejectionScopeShutdown rejectionScope = "shutdown"
)

// Hub 管理全部在线连接：注册/注销与按用户推送（并发安全）。
// 连接生命周期由 Handle 驱动（握手成功后阻塞至连接关闭）。
type Hub struct {
	cfg Config
	log *zap.Logger

	mu             sync.RWMutex
	clients        map[int64]map[*client]struct{}
	closed         bool
	reserved       int
	reservedByUser map[int64]int
	reservedByIP   map[string]int
	reservationsWg sync.WaitGroup
}

// New 构造 Hub；cfg 零值字段取默认（心跳 30s、写超时 10s）。
func New(cfg Config, log *zap.Logger) *Hub {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeat
	}
	if cfg.WriteWait <= 0 {
		cfg.WriteWait = defaultWriteWait
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = defaultMaxConnections
	}
	if cfg.MaxConnectionsPerUser <= 0 {
		cfg.MaxConnectionsPerUser = defaultMaxConnectionsPerUser
	}
	if cfg.MaxConnectionsPerIP <= 0 {
		cfg.MaxConnectionsPerIP = defaultMaxConnectionsPerIP
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Hub{
		cfg:            cfg,
		log:            log,
		clients:        make(map[int64]map[*client]struct{}),
		reservedByUser: make(map[int64]int),
		reservedByIP:   make(map[string]int),
	}
}

// PushToUser 向用户全部在线连接推送事件（非阻塞：发送缓冲满视为慢消费者，关闭连接）。
// 序列化失败仅记日志；离线用户为无操作（消息已落库，上线 REST 补拉）。
func (h *Hub) PushToUser(userID int64, event string, data any) {
	payload, err := json.Marshal(Event{Event: event, Data: data})
	if err != nil {
		h.log.Warn("WS 推送序列化失败", zap.Error(err))
		return
	}

	h.mu.RLock()
	conns := make([]*client, 0, len(h.clients[userID]))
	for c := range h.clients[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		select {
		case c.send <- payload:
		default:
			h.log.Warn("WS 慢消费者，关闭连接", zap.Int64("user_id", userID))
			c.close()
		}
	}
}

// ConnectedCount 当前在线连接数（指标/测试用）。
func (h *Hub) ConnectedCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	total := 0
	for _, conns := range h.clients {
		total += len(conns)
	}
	return total
}

// Close 关闭全部连接（服务优雅关闭时调用；连接断开自动注销）。
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	clients := make([]*client, 0)
	for _, conns := range h.clients {
		for c := range conns {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
	h.reservationsWg.Wait()
}

// Handle 接管一条已升级的连接直至关闭：注册 → 启动写泵 → 读泵阻塞（心跳维持与断开检测）。
func (h *Hub) Handle(userID int64, sourceIP string, expiresAt time.Time, conn *websocket.Conn) {
	c := newClient(h, userID, expiresAt, conn)
	if !h.register(c) {
		c.close()
		return
	}
	h.log.Info("WS 连接建立", zap.Int64("user_id", userID), zap.String("client_ip", sourceIP))

	go c.writePump()
	defer func() {
		c.close()
		<-c.stopped
		h.unregister(c)
		h.log.Info("WS 连接关闭", zap.Int64("user_id", userID), zap.String("client_ip", sourceIP))
	}()
	c.readPump()
}

func (h *Hub) register(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	if h.clients[c.userID] == nil {
		h.clients[c.userID] = make(map[*client]struct{})
	}
	h.clients[c.userID][c] = struct{}{}
	return true
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conns := h.clients[c.userID]; conns != nil {
		if _, ok := conns[c]; ok {
			delete(conns, c)
		}
		if len(conns) == 0 {
			delete(h.clients, c.userID)
		}
	}
}

// reserve 在升级前原子检查并占用连接配额，计数包含正在升级的请求。
// 返回的 release 必须在 handler 退出时调用一次。
func (h *Hub) reserve(userID int64, sourceIP string) (release func(), scope rejectionScope, ok bool) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, rejectionScopeShutdown, false
	}
	if h.reserved >= h.cfg.MaxConnections {
		h.mu.Unlock()
		return nil, rejectionScopeGlobal, false
	}
	if h.reservedByUser[userID] >= h.cfg.MaxConnectionsPerUser {
		h.mu.Unlock()
		return nil, rejectionScopeUser, false
	}
	if h.reservedByIP[sourceIP] >= h.cfg.MaxConnectionsPerIP {
		h.mu.Unlock()
		return nil, rejectionScopeIP, false
	}

	h.reserved++
	h.reservedByUser[userID]++
	h.reservedByIP[sourceIP]++
	h.reservationsWg.Add(1)
	h.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.reserved--
			h.reservedByUser[userID]--
			if h.reservedByUser[userID] == 0 {
				delete(h.reservedByUser, userID)
			}
			h.reservedByIP[sourceIP]--
			if h.reservedByIP[sourceIP] == 0 {
				delete(h.reservedByIP, sourceIP)
			}
			h.mu.Unlock()
			h.reservationsWg.Done()
		})
	}, "", true
}

// client 单条连接：写泵（业务推送 + 心跳 Ping）+ 读泵（Pong 维持读超时与断开检测）。
type client struct {
	hub       *Hub
	userID    int64
	expiresAt time.Time
	conn      *websocket.Conn
	send      chan []byte
	done      chan struct{}
	stopped   chan struct{}
	once      sync.Once
}

func newClient(hub *Hub, userID int64, expiresAt time.Time, conn *websocket.Conn) *client {
	return &client{
		hub:       hub,
		userID:    userID,
		expiresAt: expiresAt,
		conn:      conn,
		send:      make(chan []byte, sendBuffer),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

// close 幂等关闭：通知各泵退出并关闭底层连接。
func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

// writePump 写泵：串行发送业务消息、Ping 和到期关闭帧；退出时停止 ticker/timer。
func (c *client) writePump() {
	ticker := time.NewTicker(c.hub.cfg.HeartbeatInterval)
	expiry := time.NewTimer(time.Until(c.expiresAt))
	defer ticker.Stop()
	defer expiry.Stop()
	defer close(c.stopped)

	for {
		select {
		case payload := <-c.send:
			if !c.prepareWrite() {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.closeAfterWriteError()
				return
			}
		case <-ticker.C:
			if !c.prepareWrite() {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.closeAfterWriteError()
				return
			}
		case <-expiry.C:
			c.closeExpired()
			return
		case <-c.done:
			return
		}
	}
}

// prepareWrite 让业务写入最晚在授权截止时间结束，并在随机 select 先选中发送时
// 再次检查期限，避免到期后继续发送数据。
func (c *client) prepareWrite() bool {
	now := time.Now()
	if !now.Before(c.expiresAt) {
		c.closeExpired()
		return false
	}
	deadline := now.Add(c.hub.cfg.WriteWait)
	if c.expiresAt.Before(deadline) {
		deadline = c.expiresAt
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		c.close()
		return false
	}
	return true
}

func (c *client) closeAfterWriteError() {
	if !time.Now().Before(c.expiresAt) {
		c.closeExpired()
		return
	}
	c.close()
}

func (c *client) closeExpired() {
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(closeCodeTokenExpired, "token expired"),
		time.Now().Add(expiryCloseWait),
	)
	c.close()
}

// readPump 读泵：业务为单向推送，客户端消息全部忽略；Pong 重置读超时
// （两次心跳未收到任何帧即判定死连接）。读错误/超时 → 关闭连接。
func (c *client) readPump() {
	pongWait := 2 * c.hub.cfg.HeartbeatInterval
	c.conn.SetReadLimit(1024)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}
