// 消息与会话集成测试（主 seam）：真实 MySQL + httptest 起完整路由
// （user + social + chat + /ws），覆盖 T17 四项验收：
// ① A→B 发送消息成功（text/image/file）；
// ② 会话列表与消息列表正确（游标分页）；
// ③ 离线后上线可拉取到未读消息（未读数 + after_id 拉新 + 已读推进）；
// ④ 非会话双方不可访问（owner 校验，防 IDOR）。
// 另覆盖：幂等重放（同 client_request_id 返回原消息）、非好友发送被拒、
// 自加被拒、鉴权与参数校验；T18 WS 通道见 ws_integration_test.go（同 env）。
package chat_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	chathandler "github.com/xiangzhang-coding/go-single/internal/chat/handler"
	chatmodel "github.com/xiangzhang-coding/go-single/internal/chat/model"
	chatrepo "github.com/xiangzhang-coding/go-single/internal/chat/repository"
	chatsvc "github.com/xiangzhang-coding/go-single/internal/chat/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
	socialhandler "github.com/xiangzhang-coding/go-single/internal/social/handler"
	socialrepo "github.com/xiangzhang-coding/go-single/internal/social/repository"
	socialsvc "github.com/xiangzhang-coding/go-single/internal/social/service"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
	// wsHeartbeat WS 集成测试心跳间隔（短间隔使保活/死连接检测快速收敛）。
	wsHeartbeat = 40 * time.Millisecond
)

// testEnv 每个测试包只构建一次；MySQL 不可达时本地跳过、CI 失败。
type testEnv struct {
	router   http.Handler
	verifier auth.TokenVerifier
	hub      *ws.Hub
}

var (
	envOnce sync.Once
	env     *testEnv
	envErr  error
)

func requireEnv(t *testing.T) *testEnv {
	t.Helper()
	envOnce.Do(func() { env, envErr = buildEnv() })
	testsupport.RequireDependency(t, "MySQL", envErr)
	return env
}

func buildEnv() (*testEnv, error) {
	conn, err := sql.Open("mysql", testDSN(""))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	rootDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/", envOr("GO_SINGLE_MYSQL_ROOT_USER", "root"), envOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"), envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"), envOr("GO_SINGLE_MYSQL_PORT", "3306"))
	rootConn, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return nil, err
	}
	defer rootConn.Close()
	if _, err := rootConn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+testDBName); err != nil {
		return nil, fmt.Errorf("创建测试库: %w", err)
	}
	if _, err := rootConn.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+testDBName+".* TO '"+envOr("GO_SINGLE_MYSQL_USER", "shop")+"'@'%'"); err != nil {
		return nil, fmt.Errorf("授权测试库: %w", err)
	}

	m, err := migrate.New("file://"+migrationsDir, "mysql://"+testDSN(testDBName))
	if err != nil {
		return nil, err
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return nil, fmt.Errorf("执行迁移: %w", err)
	}

	gdb, err := gorm.Open(mysql.Open(testDSN(testDBName)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})
	fileSvc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint: envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"), AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"), Bucket: envOr("GO_SINGLE_MINIO_BUCKET", "go-shop-test"),
	})
	if err != nil {
		return nil, err
	}
	userSvc := usersvc.NewWithMedia(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier, fileSvc)

	socialStore := socialrepo.Store{
		Requests:    socialrepo.NewGORMRequest(gdb),
		Friendships: socialrepo.NewGORMFriendship(gdb),
		Posts:       socialrepo.NewGORMPost(gdb),
	}
	socialSvc := socialsvc.New(socialStore, userSvc)
	postSvc := socialsvc.NewPostsWithMedia(socialStore, userSvc, stubOrders{}, fileSvc)

	chatConversationRepo := chatrepo.NewGORMConversation(gdb)
	wsHub := ws.New(ws.Config{HeartbeatInterval: wsHeartbeat, WriteWait: 2 * time.Second}, zap.NewNop())
	chatSvc := chatsvc.NewWithMedia(chatrepo.Store{
		Conversations: chatConversationRepo,
		Messages:      chatrepo.NewGORMMessage(gdb),
		Reads:         chatrepo.NewGORMReadState(gdb),
		Tx:            chatConversationRepo,
	}, userSvc, socialSvc, wsNotifier{hub: wsHub}, fileSvc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws", wsHub.Handler(verifier))
	api := r.Group("/api")
	userhandler.New(userSvc, verifier).RegisterRoutes(api)
	socialhandler.New(socialSvc, postSvc, verifier).RegisterRoutes(api)
	chathandler.New(chatSvc, verifier).RegisterRoutes(api)
	file.NewHandler(fileSvc, verifier, chatMediaAuthorizer{chat: chatSvc}).RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, hub: wsHub}, nil
}

type chatMediaAuthorizer struct{ chat chatsvc.Service }

func (a chatMediaAuthorizer) CanRead(ctx context.Context, userID int64, reference string) (bool, error) {
	return a.chat.CanReadMedia(ctx, userID, reference)
}

var _ file.AccessAuthorizer = chatMediaAuthorizer{}

// wsNotifier 测试侧的消息实时推送适配器（与 cmd/server 组装一致）。
type wsNotifier struct{ hub *ws.Hub }

func (n wsNotifier) NotifyMessageSent(_ context.Context, msg *chatmodel.Message) {
	n.hub.PushToUser(msg.RecipientID, ws.EventNewMessage, msg)
}

var _ chatsvc.MessageNotifier = wsNotifier{}

// stubOrders 好友圈动态的购买校验端口替身（本包不触达分享功能）。
type stubOrders struct{}

func (stubOrders) HasPurchasedSKU(context.Context, int64, int64) (bool, error) { return false, nil }

func testDSN(dbName string) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		envOr("GO_SINGLE_MYSQL_USER", "shop"),
		envOr("GO_SINGLE_MYSQL_PASSWORD", "shop123"),
		envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"),
		envOr("GO_SINGLE_MYSQL_PORT", "3306"),
		dbName,
	)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func doJSON(t *testing.T, env *testEnv, method, path, body string, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, r)

	var parsed map[string]any
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &parsed))
	}
	return w, parsed
}

func uploadMedia(t *testing.T, env *testEnv, token, filename, kind string, content []byte) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("kind", kind))
	part, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	reference, _ := response["url"].(string)
	require.True(t, strings.HasPrefix(reference, "/files/"))
	return reference
}

func readMedia(t *testing.T, env *testEnv, token, reference string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api"+reference, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// register 注册并登录，返回 (userID, token)。
func register(t *testing.T, env *testEnv, prefix string) (int64, string) {
	t.Helper()
	username := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	w, body := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	userID := int64(body["id"].(float64))

	w, body = doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusOK, w.Code)
	token := body["token"].(string)
	require.NotEmpty(t, token)
	return userID, token
}

// befriend alice 发起申请 → bob 通过，建立好友关系（聊天前提）。
func befriend(t *testing.T, env *testEnv, aliceToken string, bobID int64, bobToken string) {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, bobID), aliceToken)
	require.Equal(t, http.StatusCreated, w.Code, "发起申请失败: %s", w.Body.String())
	reqID := int64(body["id"].(float64))
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", reqID), "", bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)
}

// sendMsg 发送消息并断言 201，返回消息体。
func sendMsg(t *testing.T, env *testEnv, token string, toUserID int64, msgType, content, url, rid string) map[string]any {
	t.Helper()
	bodyParts := []string{
		fmt.Sprintf(`"to_user_id":%d`, toUserID),
		fmt.Sprintf(`"type":%q`, msgType),
	}
	if content != "" {
		bodyParts = append(bodyParts, fmt.Sprintf(`"content":%q`, content))
	}
	if url != "" {
		bodyParts = append(bodyParts, fmt.Sprintf(`"url":%q`, url))
	}
	if rid != "" {
		bodyParts = append(bodyParts, fmt.Sprintf(`"client_request_id":%q`, rid))
	}
	w, body := doJSON(t, env, http.MethodPost, "/api/messages", "{"+strings.Join(bodyParts, ",")+"}", token)
	require.Equal(t, http.StatusCreated, w.Code, "发送失败: %s", w.Body.String())
	return body
}

func conversationsOf(t *testing.T, env *testEnv, token, query string) ([]any, bool) {
	t.Helper()
	w, body := doJSON(t, env, http.MethodGet, "/api/conversations"+query, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items, _ := body["items"].([]any)
	hasMore, _ := body["has_more"].(bool)
	return items, hasMore
}

func messagesOf(t *testing.T, env *testEnv, token, key, query string) ([]any, bool) {
	t.Helper()
	w, body := doJSON(t, env, http.MethodGet, "/api/conversations/"+key+"/messages"+query, "", token)
	require.Equal(t, http.StatusOK, w.Code, "拉取消息失败: %s", w.Body.String())
	items, _ := body["items"].([]any)
	hasMore, _ := body["has_more"].(bool)
	return items, hasMore
}

// ---- 验收 ①：A→B 发送消息成功（text/image/file） ----

func TestChatSendMessageTypes(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_ct")
	bobID, bobToken := register(t, env, "bob_ct")
	_, carolToken := register(t, env, "carol_ct")
	befriend(t, env, aliceToken, bobID, bobToken)

	key := conversationKeyOf(aliceID, bobID)

	// text：content 落库，url 为空。
	text := sendMsg(t, env, aliceToken, bobID, "text", "你好呀", "", "")
	require.Equal(t, "text", text["type"])
	require.Equal(t, "你好呀", text["content"])
	require.Equal(t, key, text["conversation_key"])
	require.Equal(t, float64(aliceID), text["sender_id"])
	require.Equal(t, float64(bobID), text["recipient_id"])

	// image：真实上传后保存托管引用；会话双方可读，第三人不可读。
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 0x49, 0x48, 0x44, 0x52}
	imageReference := uploadMedia(t, env, aliceToken, "chat.png", file.KindImage, imageBytes)
	image := sendMsg(t, env, aliceToken, bobID, "image", "", imageReference, "")
	require.Equal(t, "image", image["type"])
	require.Equal(t, imageReference, image["url"])
	read := readMedia(t, env, bobToken, imageReference)
	require.Equal(t, http.StatusOK, read.Code)
	require.Equal(t, imageBytes, read.Body.Bytes())
	require.Equal(t, http.StatusForbidden, readMedia(t, env, carolToken, imageReference).Code)
	require.Equal(t, http.StatusUnauthorized, readMedia(t, env, "", imageReference).Code)

	// file：明确的 PDF 策略，反向（bob→alice）发送并鉴权下载。
	pdf := []byte("%PDF-1.7\nchat file\n")
	fileReference := uploadMedia(t, env, bobToken, "manual.pdf", file.KindFile, pdf)
	fileMessage := sendMsg(t, env, bobToken, aliceID, "file", "", fileReference, "")
	require.Equal(t, "file", fileMessage["type"])
	require.Equal(t, key, fileMessage["conversation_key"])
	read = readMedia(t, env, aliceToken, fileReference)
	require.Equal(t, http.StatusOK, read.Code)
	require.Equal(t, pdf, read.Body.Bytes())
	require.Contains(t, read.Header().Get("Content-Disposition"), "attachment")
	require.Contains(t, read.Header().Get("Content-Disposition"), "manual.pdf")

	foreignReference := uploadMedia(t, env, carolToken, "foreign.png", file.KindImage, imageBytes)
	w, _ := doJSON(t, env, http.MethodPost, "/api/messages",
		fmt.Sprintf(`{"to_user_id":%d,"type":"image","url":%q}`, bobID, foreignReference), aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code, "不能发送他人的对象引用")
	w, _ = doJSON(t, env, http.MethodPost, "/api/messages",
		fmt.Sprintf(`{"to_user_id":%d,"type":"image","url":"https://cdn.example.com/a.png"}`, bobID), aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code, "任意外部 URL 必须被拒")
}

func conversationKeyOf(uidA, uidB int64) string {
	if uidA < uidB {
		return fmt.Sprintf("%d:%d", uidA, uidB)
	}
	return fmt.Sprintf("%d:%d", uidB, uidA)
}

// ---- 验收 ②：会话列表与消息列表正确（游标分页） ----

func TestChatConversationAndMessageList(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_cl")
	bobID, bobToken := register(t, env, "bob_cl")
	befriend(t, env, aliceToken, bobID, bobToken)
	key := conversationKeyOf(aliceID, bobID)

	for i := 0; i < 5; i++ {
		sendMsg(t, env, aliceToken, bobID, "text", fmt.Sprintf("m%d", i), "", "")
	}

	// 会话列表：双方可见同一会话；对方 id/用户名、最近消息预览、未读数正确。
	aliceConvs, _ := conversationsOf(t, env, aliceToken, "")
	require.Len(t, aliceConvs, 1)
	aConv := aliceConvs[0].(map[string]any)
	require.Equal(t, key, aConv["conversation_key"])
	require.Equal(t, float64(bobID), aConv["peer_user_id"])
	require.True(t, strings.HasPrefix(aConv["peer_username"].(string), "bob_"), "对方用户名经跨模块补齐")
	last := aConv["last_message"].(map[string]any)
	require.Equal(t, "m4", last["content"], "预览为最近一条消息")
	require.Equal(t, float64(0), aConv["unread_count"], "alice 发给自己没有未读")

	bobConvs, _ := conversationsOf(t, env, bobToken, "")
	require.Len(t, bobConvs, 1)
	bConv := bobConvs[0].(map[string]any)
	require.Equal(t, float64(aliceID), bConv["peer_user_id"])
	require.Equal(t, float64(5), bConv["unread_count"], "bob 有 5 条未读")

	// 消息列表游标分页：缺省取最近 limit 条（正序），after_id 拉新，has_more 正确。
	items, hasMore := messagesOf(t, env, bobToken, key, "?limit=2")
	require.True(t, hasMore)
	require.Len(t, items, 2)
	require.Equal(t, "m3", items[0].(map[string]any)["content"])
	require.Equal(t, "m4", items[1].(map[string]any)["content"])

	first := int64(items[0].(map[string]any)["id"].(float64))
	items, hasMore = messagesOf(t, env, bobToken, key, fmt.Sprintf("?after_id=%d&limit=2", first))
	require.False(t, hasMore, "after 游标翻到最后不再有更多")
	require.Len(t, items, 1)
	require.Equal(t, "m4", items[0].(map[string]any)["content"])

	// before_id 拉旧历史（倒序取回，返回仍正序；4 条更早消息 → has_more=true）。
	lastID := int64(items[0].(map[string]any)["id"].(float64))
	items, hasMore = messagesOf(t, env, bobToken, key, fmt.Sprintf("?before_id=%d&limit=3", lastID))
	require.True(t, hasMore, "还有更早的消息")
	require.Len(t, items, 3)
	require.Equal(t, "m1", items[0].(map[string]any)["content"])
	require.Equal(t, "m3", items[2].(map[string]any)["content"])

	// 继续向前翻到最早一条 → has_more=false。
	oldest := int64(items[0].(map[string]any)["id"].(float64))
	items, hasMore = messagesOf(t, env, bobToken, key, fmt.Sprintf("?before_id=%d&limit=3", oldest))
	require.False(t, hasMore)
	require.Len(t, items, 1)
	require.Equal(t, "m0", items[0].(map[string]any)["content"])
}

// ---- 验收 ②：会话列表游标分页 ----

func TestChatConversationListPagination(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_cp")
	bobID, bobToken := register(t, env, "bob_cp")
	carolID, carolToken := register(t, env, "carol_cp")
	daveID, daveToken := register(t, env, "dave_cp")
	befriend(t, env, aliceToken, bobID, bobToken)
	befriend(t, env, aliceToken, carolID, carolToken)
	befriend(t, env, aliceToken, daveID, daveToken)

	// 活跃度递增：bob 会话 1 条 < carol 会话 2 条 < dave 会话 3 条。
	for i := 0; i < 1; i++ {
		sendMsg(t, env, aliceToken, bobID, "text", "b", "", "")
	}
	for i := 0; i < 2; i++ {
		sendMsg(t, env, aliceToken, carolID, "text", "c", "", "")
	}
	for i := 0; i < 3; i++ {
		sendMsg(t, env, aliceToken, daveID, "text", "d", "", "")
	}

	// 第 1 页 2 条（dave、carol 会话），has_more。
	page1, hasMore := conversationsOf(t, env, aliceToken, "?limit=2")
	require.True(t, hasMore)
	require.Len(t, page1, 2)
	require.Equal(t, conversationKeyOf(aliceID, daveID), page1[0].(map[string]any)["conversation_key"], "最近活跃在前")
	require.Equal(t, conversationKeyOf(aliceID, carolID), page1[1].(map[string]any)["conversation_key"])
	last := page1[1].(map[string]any)["last_message"].(map[string]any)
	beforeID := int64(last["id"].(float64))

	// 第 2 页：before_id = 上一页末位 last_message id → 剩 bob 会话。
	page2, hasMore := conversationsOf(t, env, aliceToken, fmt.Sprintf("?before_id=%d&limit=2", beforeID))
	require.False(t, hasMore)
	require.Len(t, page2, 1)
	require.Equal(t, conversationKeyOf(aliceID, bobID), page2[0].(map[string]any)["conversation_key"])

	// 越界：游标早于最早会话 → 无更多。
	page3, hasMore := conversationsOf(t, env, aliceToken, "?before_id=1&limit=2")
	require.False(t, hasMore)
	require.Empty(t, page3)

	// bob 视角：他的会话列表只含与 alice 的会话。
	bobConvs, _ := conversationsOf(t, env, bobToken, "")
	require.Len(t, bobConvs, 1)
}

// ---- 验收 ③：离线后上线可拉取到未读消息 ----

func TestChatOfflinePullUnread(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_ou")
	bobID, bobToken := register(t, env, "bob_ou")
	befriend(t, env, aliceToken, bobID, bobToken)
	key := conversationKeyOf(aliceID, bobID)

	// bob"离线"期间 alice 连发 3 条（消息落库，无人即时接收）。
	for i := 0; i < 3; i++ {
		sendMsg(t, env, aliceToken, bobID, "text", fmt.Sprintf("离线消息%d", i), "", "")
	}

	// bob 上线：会话列表可见未读数 3。
	convs, _ := conversationsOf(t, env, bobToken, "")
	require.Equal(t, float64(3), convs[0].(map[string]any)["unread_count"])

	// 按会话游标拉取全部未读（缺省取最近，limit 足够）。
	items, hasMore := messagesOf(t, env, bobToken, key, "?limit=20")
	require.False(t, hasMore)
	require.Len(t, items, 3)
	contents := []string{}
	for _, it := range items {
		contents = append(contents, it.(map[string]any)["content"].(string))
	}
	require.Equal(t, []string{"离线消息0", "离线消息1", "离线消息2"}, contents)

	// 已读推进到最后一条 → 未读清零（再上线不再视为未读）。
	lastID := int64(items[2].(map[string]any)["id"].(float64))
	w, _ := doJSON(t, env, http.MethodPost, "/api/conversations/"+key+"/read",
		fmt.Sprintf(`{"last_message_id":%d}`, lastID), bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)
	convs, _ = conversationsOf(t, env, bobToken, "")
	require.Equal(t, float64(0), convs[0].(map[string]any)["unread_count"])
}

// ---- 验收 ④：非会话双方不可访问（owner 校验） ----

func TestChatOwnerCheck(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_oc")
	bobID, bobToken := register(t, env, "bob_oc")
	_, carolToken := register(t, env, "carol_oc")
	befriend(t, env, aliceToken, bobID, bobToken)
	key := conversationKeyOf(aliceID, bobID)

	sendMsg(t, env, aliceToken, bobID, "text", "私聊内容", "", "")

	// 第三人（非会话双方）拉取消息 / 推进已读 → 403。
	w, _ := doJSON(t, env, http.MethodGet, "/api/conversations/"+key+"/messages", "", carolToken)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/conversations/"+key+"/read", `{"last_message_id":1}`, carolToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 第三人会话列表不含该会话。
	carolConvs, _ := conversationsOf(t, env, carolToken, "")
	require.Empty(t, carolConvs)

	// 会话双方均可访问。
	w, _ = doJSON(t, env, http.MethodGet, "/api/conversations/"+key+"/messages", "", aliceToken)
	require.Equal(t, http.StatusOK, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/conversations/"+key+"/messages", "", bobToken)
	require.Equal(t, http.StatusOK, w.Code)

	// 不存在的会话 → 404；非法键格式 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/conversations/99:100/messages", "", aliceToken)
	require.Equal(t, http.StatusNotFound, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/conversations/abc/messages", "", aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---- 幂等重放、非好友、鉴权与参数校验 ----

func TestChatIdempotentReplay(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_id")
	bobID, bobToken := register(t, env, "bob_id")
	befriend(t, env, aliceToken, bobID, bobToken)

	rid := fmt.Sprintf("req_%d", time.Now().UnixNano())
	first := sendMsg(t, env, aliceToken, bobID, "text", "幂等消息", "", rid)
	firstID := first["id"]

	// 同一 client_request_id 重放 → 200 + 原消息（同 id），不新增。
	w, body := doJSON(t, env, http.MethodPost, "/api/messages",
		fmt.Sprintf(`{"to_user_id":%d,"type":"text","content":"幂等消息","client_request_id":%q}`, bobID, rid), aliceToken)
	require.Equal(t, http.StatusOK, w.Code, "幂等重放应返回 200: %s", w.Body.String())
	require.Equal(t, firstID, body["id"])

	items, _ := messagesOf(t, env, bobToken, conversationKeyOf(aliceID, bobID), "")
	require.Len(t, items, 1, "幂等重放不产生新消息")
}

func TestChatSendRejections(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_sr")
	bobID, _ := register(t, env, "bob_sr")
	_, carolToken := register(t, env, "carol_sr")

	// 未带 token → 401。
	w, _ := doJSON(t, env, http.MethodPost, "/api/messages", `{"to_user_id":1,"type":"text","content":"x"}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 自加（给自己发消息）→ 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/messages",
		fmt.Sprintf(`{"to_user_id":%d,"type":"text","content":"x"}`, aliceID), aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 非好友 → 403（T14 好友关系为前置条件）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/messages",
		fmt.Sprintf(`{"to_user_id":%d,"type":"text","content":"x"}`, bobID), aliceToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 接收方不存在 → 404。
	w, _ = doJSON(t, env, http.MethodPost, "/api/messages", `{"to_user_id":999999,"type":"text","content":"x"}`, aliceToken)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 参数校验：缺 type / 非法类型 / text 缺内容 / image 缺 url / url 非 http。
	w, _ = doJSON(t, env, http.MethodPost, "/api/messages", fmt.Sprintf(`{"to_user_id":%d,"content":"x"}`, bobID), carolToken)
	require.Equal(t, http.StatusBadRequest, w.Code)
	for _, body := range []string{
		fmt.Sprintf(`{"to_user_id":%d,"type":"voice","content":"x"}`, bobID),
		fmt.Sprintf(`{"to_user_id":%d,"type":"text"}`, bobID),
		fmt.Sprintf(`{"to_user_id":%d,"type":"image"}`, bobID),
		fmt.Sprintf(`{"to_user_id":%d,"type":"file","url":"ftp://x"}`, bobID),
	} {
		w, _ = doJSON(t, env, http.MethodPost, "/api/messages", body, carolToken)
		require.Equal(t, http.StatusBadRequest, w.Code, "非法请求应被拒: %s", body)
	}

	// 未产生任何会话（校验失败不落库）。
	carolConvs, _ := conversationsOf(t, env, carolToken, "")
	require.Empty(t, carolConvs)
}
