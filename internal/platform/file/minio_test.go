// 文件上传集成测试（主 seam）：真实 MinIO + httptest 起完整路由，
// 覆盖 合法图片返回 URL、非法类型/超限被拒、未授权 401、私有桶匿名不可读。
// MinIO 未就绪时本地跳过、CI 失败（服务见 deploy/docker-compose.yml）。
package file_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
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
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
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

// testEnv 每个测试包只构建一次；MinIO 不可达时本地跳过、CI 失败。
type testEnv struct {
	router   http.Handler
	bucket   string
	endpoint string
	client   *minio.Client
	svc      *file.MinIO
	usage    *memoryUsage
}

type memoryUsage struct {
	mu           sync.Mutex
	bytes        map[int64]int64
	objects      map[int64]int64
	reservations map[string]file.UploadReservation
	commitErr    error
}

type contextCheckingUsage struct {
	released      bool
	releaseCtxErr error
}

func (*contextCheckingUsage) Reserve(context.Context, int64, string, string, int64, int64, int64) error {
	return nil
}

func (*contextCheckingUsage) Commit(context.Context, int64, string) error { return nil }

func (u *contextCheckingUsage) Release(ctx context.Context, _ int64, _ string, _ int64) error {
	u.released = true
	u.releaseCtxErr = ctx.Err()
	return nil
}

func (*contextCheckingUsage) ListPending(context.Context, time.Duration, int) ([]file.UploadReservation, error) {
	return nil, nil
}

func (*contextCheckingUsage) GetByRequestID(context.Context, int64, string) (*file.UploadReservation, error) {
	return nil, nil
}

func newMemoryUsage() *memoryUsage {
	return &memoryUsage{
		bytes: map[int64]int64{}, objects: map[int64]int64{}, reservations: map[string]file.UploadReservation{},
	}
}

func (u *memoryUsage) Reserve(_ context.Context, ownerID int64, requestID, objectKey string, size, maxBytes, maxObjects int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, reservation := range u.reservations {
		if reservation.OwnerID == ownerID && reservation.RequestID == requestID {
			return file.ErrUploadRequestExists
		}
	}
	if u.bytes[ownerID]+size > maxBytes || u.objects[ownerID]+1 > maxObjects {
		return file.ErrQuotaExceeded
	}
	u.bytes[ownerID] += size
	u.objects[ownerID]++
	u.reservations[objectKey] = file.UploadReservation{
		OwnerID: ownerID, RequestID: requestID, ObjectKey: objectKey, Size: size,
		Status: file.UploadStatusPending, CreatedAt: time.Now(),
	}
	return nil
}

func (u *memoryUsage) Commit(_ context.Context, _ int64, objectKey string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.commitErr != nil {
		return u.commitErr
	}
	reservation := u.reservations[objectKey]
	reservation.Status = file.UploadStatusCommitted
	u.reservations[objectKey] = reservation
	return nil
}

func (u *memoryUsage) Release(_ context.Context, ownerID int64, objectKey string, size int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.reservations[objectKey]; !ok {
		return nil
	}
	delete(u.reservations, objectKey)
	u.bytes[ownerID] -= size
	u.objects[ownerID]--
	return nil
}

func (u *memoryUsage) ListPending(_ context.Context, minAge time.Duration, limit int) ([]file.UploadReservation, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]file.UploadReservation, 0, len(u.reservations))
	for _, reservation := range u.reservations {
		if reservation.Status != file.UploadStatusPending {
			continue
		}
		if time.Since(reservation.CreatedAt) < minAge {
			continue
		}
		out = append(out, reservation)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (u *memoryUsage) GetByRequestID(_ context.Context, ownerID int64, requestID string) (*file.UploadReservation, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, reservation := range u.reservations {
		if reservation.OwnerID == ownerID && reservation.RequestID == requestID {
			copy := reservation
			return &copy, nil
		}
	}
	return nil, nil
}

var (
	envOnce sync.Once
	env     *testEnv
	envErr  error
)

func requireEnv(t *testing.T) *testEnv {
	t.Helper()
	envOnce.Do(func() { env, envErr = buildEnv() })
	testsupport.RequireDependency(t, "MinIO", envErr)
	return env
}

func buildEnv() (*testEnv, error) {
	cfg := file.MinIOConfig{
		Endpoint:  envOr("GO_SINGLE_MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    envOr("GO_SINGLE_MINIO_BUCKET", "go-shop-test"),
		UseSSL:    false,
	}
	usage := newMemoryUsage()
	svc, err := file.NewMinIO(cfg, usage, file.QuotaConfig{
		MaxBytesPerUser: 256 << 20, MaxObjectsPerUser: 100,
	})
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
	file.NewHandler(svc, verifier, nil).RegisterRoutes(api)
	return &testEnv{router: r, bucket: cfg.Bucket, endpoint: cfg.Endpoint, client: client, svc: svc, usage: usage}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// uploadFile 以 multipart 上传一个文件，返回响应。
func uploadFile(t *testing.T, e *testEnv, token, filename string, content []byte) *httptest.ResponseRecorder {
	return uploadFileKind(t, e, token, filename, content, "")
}

func uploadFileKind(t *testing.T, e *testEnv, token, filename string, content []byte, kind string) *httptest.ResponseRecorder {
	return uploadFileKindWithRequestID(t, e, token, filename, content, kind, fmt.Sprintf("upload-%d", time.Now().UnixNano()))
}

func uploadFileKindWithRequestID(t *testing.T, e *testEnv, token, filename string, content []byte, kind, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if kind != "" {
		require.NoError(t, mw.WriteField("kind", kind))
	}
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Idempotency-Key", requestID)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func token(t *testing.T) string {
	return tokenFor(t, 1, 2*time.Hour)
}

func tokenFor(t *testing.T, userID int64, ttl time.Duration) string {
	t.Helper()
	j := auth.NewJWT(auth.JWTConfig{Secret: "integration-test-secret", TTL: ttl})
	tk, err := j.Issue(userID, "user")
	require.NoError(t, err)
	return tk
}

// objectKey 从后端托管引用中解出私有对象 key。
func objectKey(t *testing.T, _ *testEnv, reference string) string {
	t.Helper()
	require.True(t, strings.HasPrefix(reference, "/files/"), "应返回后端托管引用: %s", reference)
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(reference, "/files/"))
	require.NoError(t, err)
	require.NotEmpty(t, key)
	return string(key)
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
	require.True(t, strings.HasPrefix(url, "/files/"), "应返回后端托管引用: %s", url)
	require.NotContains(t, url, e.endpoint, "引用不得暴露 MinIO 地址")
	require.Equal(t, file.KindImage, body["kind"])
	require.Equal(t, "avatar.png", body["filename"])
	require.Equal(t, "image/png", body["content_type"])
	require.Equal(t, float64(len(png1x1)), body["size"])

	// 对象确实落库且内容一致。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	obj, err := e.client.StatObject(ctx, e.bucket, objectKey(t, e, url), minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(len(png1x1)), obj.Size)
	require.Equal(t, "image/png", obj.ContentType)
}

func TestUploadIdempotentRetryReturnsOriginalReference(t *testing.T) {
	e := requireEnv(t)
	requestID := fmt.Sprintf("retry-%d", time.Now().UnixNano())
	first := uploadFileKindWithRequestID(t, e, token(t), "same.png", png1x1, file.KindImage, requestID)
	require.Equal(t, http.StatusCreated, first.Code)
	second := uploadFileKindWithRequestID(t, e, token(t), "ignored.png", png1x1, file.KindImage, requestID)
	require.Equal(t, http.StatusOK, second.Code)
	var firstBody, secondBody map[string]any
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstBody))
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondBody))
	require.Equal(t, firstBody["url"], secondBody["url"])
}

func TestUploadAcceptsImagesFromOneToFiveMiB(t *testing.T) {
	e := requireEnv(t)
	for _, size := range []int{1 << 20, file.MaxImageSize} {
		t.Run(fmt.Sprintf("%d_MiB", size>>20), func(t *testing.T) {
			content := largePNG(size)
			w := uploadFile(t, e, tokenFor(t, int64(100+size), 2*time.Hour), "photo.png", content)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, float64(size), body["size"])
		})
	}
}

func TestAuthorizedReadStreamsPrivateObject(t *testing.T) {
	e := requireEnv(t)
	ownerToken := tokenFor(t, 41, 2*time.Hour)
	w := uploadFile(t, e, ownerToken, "头像.png", png1x1)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	reference := mustURL(t, w)

	read := readFile(t, e, reference, ownerToken)
	require.Equal(t, http.StatusOK, read.Code)
	require.Equal(t, png1x1, read.Body.Bytes())
	require.Equal(t, "image/png", read.Header().Get("Content-Type"))
	disposition, params, err := mime.ParseMediaType(read.Header().Get("Content-Disposition"))
	require.NoError(t, err)
	require.Equal(t, "inline", disposition)
	require.Equal(t, "头像.png", params["filename"])
	require.Equal(t, "nosniff", read.Header().Get("X-Content-Type-Options"))

	require.Equal(t, http.StatusUnauthorized, readFile(t, e, reference, "").Code)
	require.Equal(t, http.StatusForbidden, readFile(t, e, reference, tokenFor(t, 42, 2*time.Hour)).Code)
	expired := tokenFor(t, 41, -time.Hour)
	require.Equal(t, http.StatusUnauthorized, readFile(t, e, reference, expired).Code)
}

func TestFileMessageUploadAndDownloadPolicy(t *testing.T) {
	e := requireEnv(t)
	ownerToken := tokenFor(t, 51, 2*time.Hour)
	pdf := []byte("%PDF-1.7\nprivate document\n")
	w := uploadFileKind(t, e, ownerToken, "manual.pdf", pdf, file.KindFile)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	read := readFile(t, e, mustURL(t, w), ownerToken)
	require.Equal(t, http.StatusOK, read.Code)
	require.Equal(t, pdf, read.Body.Bytes())
	require.Equal(t, "application/pdf", read.Header().Get("Content-Type"))
	require.Contains(t, read.Header().Get("Content-Disposition"), "attachment")
	require.Contains(t, read.Header().Get("Content-Disposition"), "manual.pdf")

	w = uploadFileKind(t, e, ownerToken, "attack.html", []byte("<script>alert(1)</script>"), file.KindFile)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w = uploadFileKind(t, e, ownerToken, "manual.pdf", pdf, "video")
	require.Equal(t, http.StatusBadRequest, w.Code)
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
	// 5 MiB 合法 PNG 前缀 + 填充至超过上限 → 413。
	oversize := append(append([]byte{}, png1x1...), make([]byte, file.MaxImageSize)...)
	w := uploadFile(t, e, token(t), "big.png", oversize)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestMultipartHardLimitRejectsBeforeParsing(t *testing.T) {
	e := requireEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader("not parsed"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=unused")
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.ContentLength = file.MaxMultipartBodySize + 1
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestMultipartHardLimitStopsChunkedStream(t *testing.T) {
	e := requireEnv(t)
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	contentType := multipartWriter.FormDataContentType()
	type writeResult struct {
		bytes int64
		err   error
	}
	done := make(chan writeResult, 1)
	go func() {
		part, err := multipartWriter.CreateFormFile("file", "oversize.bin")
		if err != nil {
			_ = writer.CloseWithError(err)
			done <- writeResult{err: err}
			return
		}
		n, copyErr := io.CopyN(part, zeroReader{}, file.MaxMultipartBodySize+1)
		if closeErr := multipartWriter.Close(); copyErr == nil {
			copyErr = closeErr
		}
		_ = writer.CloseWithError(copyErr)
		done <- writeResult{bytes: n, err: copyErr}
	}()

	req := httptest.NewRequest(http.MethodPost, "/api/files", reader)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.ContentLength = -1
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	_ = req.Body.Close()
	result := <-done

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Less(t, result.bytes, int64(file.MaxMultipartBodySize+1), "parser must stop before buffering the whole stream")
}

func TestExpiredMultipartRequestReturnsTimeoutContract(t *testing.T) {
	e := requireEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader("incomplete multipart"))
	ctx, cancel := context.WithDeadline(req.Context(), time.Now().Add(-time.Second))
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.Header.Set("Idempotency-Key", "expired-multipart")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusGatewayTimeout, w.Code)
	require.JSONEq(t, `{"error":"request timeout"}`, w.Body.String())
}

func TestUploadQuotaStopsRepeatedLegalUploads(t *testing.T) {
	e := requireEnv(t)
	cfg := file.MinIOConfig{
		Endpoint: e.endpoint, AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"), Bucket: e.bucket,
	}
	svc, err := file.NewMinIO(cfg, newMemoryUsage(), file.QuotaConfig{
		MaxBytesPerUser: int64(len(png1x1) * 2), MaxObjectsPerUser: 1,
	})
	require.NoError(t, err)
	verifier := auth.NewJWT(auth.JWTConfig{Secret: "integration-test-secret", TTL: 2 * time.Hour})
	r := gin.New()
	api := r.Group("/api")
	file.NewHandler(svc, verifier, nil).RegisterRoutes(api)
	limited := &testEnv{router: r}

	ownerToken := tokenFor(t, 9001, 2*time.Hour)
	require.Equal(t, http.StatusCreated, uploadFile(t, limited, ownerToken, "first.png", png1x1).Code)
	w := uploadFile(t, limited, ownerToken, "second.png", png1x1)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), "quota")
}

func TestReconcilePendingUploadRemovesOrphanAndReleasesQuota(t *testing.T) {
	e := requireEnv(t)
	const ownerID int64 = 987654
	key := fmt.Sprintf("users/%d/file/20260822/%032x.txt", ownerID, time.Now().UnixNano())
	content := "orphaned upload"
	require.NoError(t, e.usage.Reserve(context.Background(), ownerID, "orphan-request", key, int64(len(content)), 1<<20, 10))
	e.usage.mu.Lock()
	reservation := e.usage.reservations[key]
	reservation.CreatedAt = time.Now().Add(-11 * time.Minute)
	e.usage.reservations[key] = reservation
	e.usage.mu.Unlock()
	_, err := e.client.PutObject(context.Background(), e.bucket, key, strings.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
	require.NoError(t, err)

	resolved, err := e.svc.ReconcilePendingUploads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, resolved)
	_, err = e.client.StatObject(context.Background(), e.bucket, key, minio.StatObjectOptions{})
	require.Error(t, err)
	e.usage.mu.Lock()
	defer e.usage.mu.Unlock()
	require.Zero(t, e.usage.bytes[ownerID])
	require.Zero(t, e.usage.objects[ownerID])
}

func TestUncertainQuotaCommitDoesNotDeletePossiblyCommittedObject(t *testing.T) {
	e := requireEnv(t)
	usage := newMemoryUsage()
	usage.commitErr = file.ErrUploadCommitUncertain
	svc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint: e.endpoint, AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"), Bucket: e.bucket,
	}, usage, file.QuotaConfig{MaxBytesPerUser: 1 << 20, MaxObjectsPerUser: 10})
	require.NoError(t, err)

	_, err = svc.Upload(context.Background(), 123456, "uncertain-request", "file", strings.NewReader("uncertain"), 9, "uncertain.txt")
	require.ErrorIs(t, err, file.ErrUploadCommitUncertain)
	usage.mu.Lock()
	var reservation file.UploadReservation
	for _, candidate := range usage.reservations {
		reservation = candidate
	}
	usage.mu.Unlock()
	require.NotEmpty(t, reservation.ObjectKey)
	_, err = e.client.StatObject(context.Background(), e.bucket, reservation.ObjectKey, minio.StatObjectOptions{})
	require.NoError(t, err, "提交结果未知时必须保留对象，等待独立确认或对账")
	require.NoError(t, e.client.RemoveObject(context.Background(), e.bucket, reservation.ObjectKey, minio.RemoveObjectOptions{}))
}

func TestUploadFailureReleasesQuotaAfterRequestCancellation(t *testing.T) {
	e := requireEnv(t)
	usage := &contextCheckingUsage{}
	svc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint: e.endpoint, AccessKey: envOr("GO_SINGLE_MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: envOr("GO_SINGLE_MINIO_SECRET_KEY", "minioadmin"), Bucket: e.bucket,
	}, usage, file.QuotaConfig{MaxBytesPerUser: 1 << 20, MaxObjectsPerUser: 10})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.Upload(ctx, 1, "cancelled-request", file.KindImage, bytes.NewReader(png1x1), int64(len(png1x1)), "avatar.png")
	require.Error(t, err)
	require.True(t, usage.released)
	require.NoError(t, usage.releaseCtxErr)
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
	// 即使只公开真实上传前缀 users/*，也必须在启动时被拒绝。
	publicPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},` +
		`"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + bucket + `/users/*"]}]}`
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
	}, newMemoryUsage(), file.QuotaConfig{MaxBytesPerUser: 1 << 20, MaxObjectsPerUser: 1})
	require.Error(t, err, "公开桶应被拒绝")
	require.Contains(t, err.Error(), "访问策略")
}

func readFile(t *testing.T, e *testEnv, reference, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api"+reference, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func mustURL(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	url, ok := body["url"].(string)
	require.True(t, ok)
	return url
}

func largePNG(size int) []byte {
	content := make([]byte, size)
	copy(content, png1x1)
	return content
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
