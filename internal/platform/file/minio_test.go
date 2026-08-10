// 文件上传集成测试（主 seam）：真实 MinIO + httptest 起完整路由，
// 覆盖 合法图片返回 URL、非法类型/超限被拒、未授权 401、私有桶匿名不可读。
// MinIO 未就绪（deploy/docker-compose.yml）时整体跳过。
package file_test

import (
	"bytes"
	"context"
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
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
)

// png1x1 一张合法 1×1 像素 PNG。
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

// testEnv 每个测试包只构建一次；MinIO 不可达时测试整体跳过。
type testEnv struct {
	router   http.Handler
	bucket   string
	endpoint string
	client   *minio.Client
}

var (
	envOnce sync.Once
	env     *testEnv
	envErr  error
)

func requireEnv(t *testing.T) *testEnv {
	t.Helper()
	envOnce.Do(func() { env, envErr = buildEnv() })
	if envErr != nil {
		t.Skipf("MinIO 不可达，跳过集成测试（先 docker compose up -d minio）：%v", envErr)
	}
	return env
}

func buildEnv() (*testEnv, error) {
	cfg := file.MinIOConfig{
		Endpoint:  envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    envOr("GO_SINGLE_MINIO_BUCKET", "go-shop-test"),
		UseSSL:    false,
		PublicURL: envOr("GO_SINGLE_MINIO_PUBLIC_URL", "http://127.0.0.1:19000"),
	}
	svc, err := file.NewMinIO(cfg)
	if err != nil {
		return nil, err
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: "integration-test-secret", TTL: 2 * time.Hour})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	file.NewHandler(svc, verifier).RegisterRoutes(api)
	return &testEnv{router: r, bucket: cfg.Bucket, endpoint: cfg.Endpoint, client: client}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// uploadFile 以 multipart 上传一个文件，返回响应。
func uploadFile(t *testing.T, e *testEnv, token, filename string, content []byte) *httptest.ResponseRecorder {
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
	e.router.ServeHTTP(w, req)
	return w
}

func token(t *testing.T) string {
	t.Helper()
	j := auth.NewJWT(auth.JWTConfig{Secret: "integration-test-secret", TTL: 2 * time.Hour})
	tk, err := j.Issue(1, "user")
	require.NoError(t, err)
	return tk
}

// objectKey 从返回 URL 中提取对象 key（{public}/{bucket}/{key}）。
func objectKey(t *testing.T, e *testEnv, url string) string {
	t.Helper()
	prefix := fmt.Sprintf("http://%s/%s/", e.endpoint, e.bucket)
	require.True(t, strings.HasPrefix(url, prefix), "URL 应为可引用地址: %s", url)
	key := strings.TrimPrefix(url, prefix)
	require.NotEmpty(t, key)
	return key
}

// ---- 合法图片上传返回 URL ----

func TestUploadValidImageReturnsURL(t *testing.T) {
	e := requireEnv(t)
	w := uploadFile(t, e, token(t), "avatar.png", png1x1)
	require.Equal(t, http.StatusCreated, w.Code, "上传失败: %s", w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	url, ok := body["url"].(string)
	require.True(t, ok)
	require.NotEmpty(t, url)
	require.True(t, strings.HasSuffix(url, ".png"), "URL 应带正确扩展名: %s", url)

	// 对象确实落库且内容一致。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	obj, err := e.client.StatObject(ctx, e.bucket, objectKey(t, e, url), minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(len(png1x1)), obj.Size)
	require.Equal(t, "image/png", obj.ContentType)
}

// ---- 非法类型被拒 ----

func TestUploadInvalidTypeRejected(t *testing.T) {
	e := requireEnv(t)
	// 文本内容即使命名为 .png 也被拒（魔数嗅探，不信扩展名）。
	w := uploadFile(t, e, token(t), "avatar.png", []byte("plain text, definitely not an image"))
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 空文件被拒。
	w = uploadFile(t, e, token(t), "empty.png", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---- 超限文件被拒 ----

func TestUploadTooLargeRejected(t *testing.T) {
	e := requireEnv(t)
	// 5MB 合法 PNG 前缀 + 填充至超过上限 → 400。
	oversize := append(append([]byte{}, png1x1...), make([]byte, file.MaxFileSize)...)
	w := uploadFile(t, e, token(t), "big.png", oversize)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---- 未授权 401 ----

func TestUploadUnauthorized(t *testing.T) {
	e := requireEnv(t)
	w := uploadFile(t, e, "", "avatar.png", png1x1)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 缺少 file 字段 → 400。
	w = uploadFile(t, e, token(t), "", png1x1)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// ---- 桶私有：匿名不可读，URL 仅经业务接口引用 ----

func TestBucketPrivate(t *testing.T) {
	e := requireEnv(t)
	w := uploadFile(t, e, token(t), "private.png", png1x1)
	require.Equal(t, http.StatusCreated, w.Code)
	key := objectKey(t, e, mustURL(t, w))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 匿名客户端（无凭据）读取被拒。
	anon, err := minio.New(e.endpoint, &minio.Options{})
	require.NoError(t, err)
	_, err = anon.StatObject(ctx, e.bucket, key, minio.StatObjectOptions{})
	require.Error(t, err, "匿名访问私有桶对象应被拒")

	// 带凭据的客户端可读（后端引用场景）。
	_, err = e.client.StatObject(ctx, e.bucket, key, minio.StatObjectOptions{})
	require.NoError(t, err)
}

// ---- 已存在的公开桶被拒绝使用 ----

func TestExistingPublicBucketRejected(t *testing.T) {
	e := requireEnv(t)
	bucket := "go-shop-public-test"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if exists, _ := e.client.BucketExists(ctx, bucket); !exists {
		require.NoError(t, e.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	}
	// 显式设置公开读策略。
	publicPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},` +
		`"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + bucket + `/*"]}]}`
	require.NoError(t, e.client.SetBucketPolicy(ctx, bucket, publicPolicy))
	t.Cleanup(func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		_ = e.client.SetBucketPolicy(ctx2, bucket, `{"Version":"2012-10-17","Statement":[]}`)
		_ = e.client.RemoveBucket(ctx2, bucket)
	})

	_, err := file.NewMinIO(file.MinIOConfig{
		Endpoint:  envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    bucket,
		PublicURL: "http://127.0.0.1:19000",
	})
	require.Error(t, err, "公开桶应被拒绝")
	require.Contains(t, err.Error(), "非私有")
}

func mustURL(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	url, ok := body["url"].(string)
	require.True(t, ok)
	return url
}
