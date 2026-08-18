// T02 个人资料集成测试（主 seam）：PATCH /api/users/me 修改昵称/头像 + 回读、
// 非法输入 400、归属校验（token 即本人）；头像走真实 MinIO 上传 e2e
// （复用 platform/file 的 POST /api/files），MinIO 未就绪时本地跳过、CI 失败。
package user_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// png1x1 一张合法 1×1 像素 PNG（与 platform/file 测试同源样例）。
var png1x1 = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x62, 0x60, 0x01, 0x00, 0x00,
	0x00, 0xFF, 0xFF, 0x03, 0x00, 0x00, 0x06, 0x00,
	0x05, 0x57, 0xBF, 0xAB, 0xD4, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

// PATCH 修改昵称/头像 → GET /users/me 回读；PATCH 语义：未提交字段不动、空串清空。
func TestUpdateProfileAndMe(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("prof_%d", time.Now().UnixNano())
	registerUser(t, env, username, "secret123")
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	// 初始：昵称/头像为空。
	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "", me["nickname"])
	require.Equal(t, "", me["avatar_url"])

	// 修改昵称；头像完整闭环由真实 MinIO 用例覆盖。
	w, updated := doJSON(t, env, http.MethodPatch, "/api/users/me",
		`{"nickname":"  小艾  "}`, token)
	require.Equal(t, http.StatusOK, w.Code, "PATCH 失败: %v", updated)
	require.Equal(t, "小艾", updated["nickname"], "昵称应 trim 后返回")
	require.Equal(t, "", updated["avatar_url"])

	// 只改昵称：头像保持（PATCH 部分更新语义）。
	w, updated = doJSON(t, env, http.MethodPatch, "/api/users/me", `{"nickname":"阿艾"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "阿艾", updated["nickname"])
	require.Equal(t, "", updated["avatar_url"])

	// 空串清空头像。
	w, updated = doJSON(t, env, http.MethodPatch, "/api/users/me", `{"avatar_url":""}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "阿艾", updated["nickname"])
	require.Equal(t, "", updated["avatar_url"])

	// GET /users/me 回读与库内一致。
	w, me = doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "阿艾", me["nickname"])
	require.Equal(t, "", me["avatar_url"])
}

// 归属校验：B 的 token 只能改 B；A 的资料不受影响（userID 取自令牌而非请求体）。
func TestUpdateProfileOwnership(t *testing.T) {
	env := requireEnv(t)
	uid := time.Now().UnixNano()
	userA := fmt.Sprintf("owna_%d", uid)
	userB := fmt.Sprintf("ownb_%d", uid)
	registerUser(t, env, userA, "secret123")
	registerUser(t, env, userB, "secret123")
	_, loginA := login(t, env, userA, "secret123")
	tokenA := tokenOf(t, loginA)
	_, loginB := login(t, env, userB, "secret123")
	tokenB := tokenOf(t, loginB)

	w, _ := doJSON(t, env, http.MethodPatch, "/api/users/me", `{"nickname":"我是B"}`, tokenB)
	require.Equal(t, http.StatusOK, w.Code)

	// A 的昵称不受 B 的 PATCH 影响。
	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "", me["nickname"])
}

// 非法输入：空请求、超长昵称、非法 URL、坏 JSON → 400；未带 token → 401。
func TestUpdateProfileValidation(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("val_%d", time.Now().UnixNano())
	registerUser(t, env, username, "secret123")
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	cases := []struct {
		name string
		body string
	}{
		{"无字段", `{}`},
		{"昵称超长", fmt.Sprintf(`{"nickname":%q}`, strings.Repeat("昵", 33))},
		{"URL 非法协议", `{"avatar_url":"ftp://example.com/a.png"}`},
		{"URL 超长", `{"avatar_url":"http://e.com/` + strings.Repeat("a", 300) + `"}`},
		{"坏 JSON", `{nickname}`},
	}
	for _, tc := range cases {
		w, body := doJSON(t, env, http.MethodPatch, "/api/users/me", tc.body, token)
		require.Equal(t, http.StatusBadRequest, w.Code, "用例 %s 应 400: %v", tc.name, body)
	}

	// 未携带 token → 401。
	w, _ := doJSON(t, env, http.MethodPatch, "/api/users/me", `{"nickname":"x"}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 非法输入不落库：昵称仍为空。
	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "", me["nickname"])
}

// 头像上传 e2e：POST /api/files（真实 MinIO）取回 URL → PATCH 写入 avatar_url →
// GET /users/me 回读。MinIO 不可达时本地跳过、CI 失败。
func TestAvatarUploadThenSetProfile(t *testing.T) {
	env := requireEnv(t)

	fileSvc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint:  envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    envOr("GO_SINGLE_MINIO_BUCKET", "go-shop-test"),
		UseSSL:    false,
	})
	testsupport.RequireDependency(t, "MinIO", err)

	// 独立路由：user + file 同挂，走同库同 JWT。
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	issuer := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})
	userSvc := usersvc.NewWithMedia(userrepo.Store{Users: userrepo.NewGORM(env.gdb), Addresses: userrepo.NewGORMAddress(env.gdb)}, issuer, fileSvc)
	userhandler.New(userSvc, env.verifier).RegisterRoutes(api)
	file.NewHandler(fileSvc, env.verifier, avatarAuthorizer{users: userSvc}).RegisterRoutes(api)

	username := fmt.Sprintf("avatar_%d", time.Now().UnixNano())
	w := doReq(t, r, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusCreated, w.Code)
	w = doReq(t, r, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	var loginResp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	token, _ := loginResp["token"].(string)
	require.NotEmpty(t, token)

	otherUsername := fmt.Sprintf("avatar_other_%d", time.Now().UnixNano())
	w = doReq(t, r, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, otherUsername), "")
	require.Equal(t, http.StatusCreated, w.Code)
	w = doReq(t, r, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, otherUsername), "")
	var otherLogin map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &otherLogin))
	otherToken, _ := otherLogin["token"].(string)
	require.NotEmpty(t, otherToken)

	// 上传头像图片 → 201 + URL。
	up := uploadMultipart(t, r, token, "avatar.png", png1x1)
	require.Equal(t, http.StatusCreated, up.Code, "上传失败: %s", up.Body.String())
	var uploadResp map[string]any
	require.NoError(t, json.Unmarshal(up.Body.Bytes(), &uploadResp))
	url, ok := uploadResp["url"].(string)
	require.True(t, ok)
	require.True(t, strings.HasPrefix(url, "/files/"), "应返回后端托管引用: %s", url)
	require.NotContains(t, url, "19000", "不得暴露 MinIO 地址")

	// 非图片内容被拒（魔数嗅探）。
	bad := uploadMultipart(t, r, token, "avatar.png", []byte("plain text, not an image"))
	require.Equal(t, http.StatusBadRequest, bad.Code)

	// 未绑定前只有上传者可预览；他人不能盗用引用作为头像。
	w = doReq(t, r, http.MethodGet, "/api"+url, "", otherToken)
	require.Equal(t, http.StatusForbidden, w.Code)
	w = doReq(t, r, http.MethodPatch, "/api/users/me", fmt.Sprintf(`{"avatar_url":%q}`, url), otherToken)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w = doReq(t, r, http.MethodPatch, "/api/users/me", `{"avatar_url":"https://cdn.example.com/a.png"}`, token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 托管引用写入 avatar_url → 回读；上传者和已获头像引用的登录用户均可读取原始字节。
	w = doReq(t, r, http.MethodPatch, "/api/users/me", fmt.Sprintf(`{"avatar_url":%q}`, url), token)
	require.Equal(t, http.StatusOK, w.Code, "PATCH 失败: %s", w.Body.String())
	w = doReq(t, r, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	var me map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &me))
	require.Equal(t, url, me["avatar_url"])
	for _, readerToken := range []string{token, otherToken} {
		w = doReq(t, r, http.MethodGet, "/api"+url, "", readerToken)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, png1x1, w.Body.Bytes())
		require.Equal(t, "image/png", w.Header().Get("Content-Type"))
	}
	w = doReq(t, r, http.MethodGet, "/api"+url, "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

type avatarAuthorizer struct{ users usersvc.Service }

func (a avatarAuthorizer) CanRead(ctx context.Context, _ int64, reference string) (bool, error) {
	return a.users.CanReadAvatar(ctx, reference)
}

var _ file.AccessAuthorizer = avatarAuthorizer{}

// doReq 向指定 router 发 JSON 请求（独立路由的轻量辅助）。
func doReq(t *testing.T, r http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// uploadMultipart 以 multipart 字段 "file" 上传内容。
func uploadMultipart(t *testing.T, r http.Handler, token, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
