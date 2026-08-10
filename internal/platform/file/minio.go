package file

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig 对象存储连接配置（与 auth.JWTConfig 同为先例：适配器自带配置结构）。
type MinIOConfig struct {
	// Endpoint 服务地址，如 127.0.0.1:19000。
	Endpoint string
	// AccessKey / SecretKey 管理员凭据。
	AccessKey string
	SecretKey string
	// Bucket 私有桶名。
	Bucket string
	// UseSSL 是否启用 TLS。
	UseSSL bool
	// PublicURL 可引用地址基址（如 http://127.0.0.1:19000），拼接上传返回的 URL。
	PublicURL string
}

// sniffSize 魔数嗅探读取的头部字节数（覆盖常见图片签名）。
const sniffSize = 512

// probeKey 匿名探测桶私有性用的不存在对象 key。
const probeKey = ".privacy-probe-ensure-private"

// MinIO 文件存储实现：私有桶 + 类型/大小校验 + 代理上传。
type MinIO struct {
	client     *minio.Client
	bucket     string
	publicBase string
}

// NewMinIO 构造 MinIO 实现：建连、确保桶存在且私有（失败快速退出，与 cache/mq 一致）。
// 桶私有性校验：匿名客户端探测不存在的对象——私有桶返回 AccessDenied（拒绝暴露存在性），
// 公开桶返回 NoSuchKey。已存在但公开的桶直接拒绝使用。
func NewMinIO(cfg MinIOConfig) (*MinIO, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("构造 MinIO 客户端: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("检查桶 %s: %w", cfg.Bucket, err)
	}
	if !exists {
		// 默认私有：不设置任何 policy。
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("创建桶 %s: %w", cfg.Bucket, err)
		}
	}

	if err := ensurePrivate(ctx, client, cfg.Endpoint, cfg.Bucket); err != nil {
		return nil, err
	}

	return &MinIO{
		client:     client,
		bucket:     cfg.Bucket,
		publicBase: strings.TrimRight(cfg.PublicURL, "/"),
	}, nil
}

// Upload 嗅探魔数校验类型与大小 → 上传私有桶 → 返回可引用 URL。
// 桶为私有，URL 仅经业务接口引用（匿名不可直读）。
func (s *MinIO) Upload(ctx context.Context, r io.Reader, size int64) (string, error) {
	br := bufio.NewReader(r)
	header, _ := br.Peek(sniffSize) // 短文件返回实际长度，空文件嗅探自然失败

	ext, mime, err := validate(header, size)
	if err != nil {
		return "", err
	}

	key := fmt.Sprintf("%s/%s.%s", time.Now().Format("20060102"), randHex(16), ext)
	_, err = s.client.PutObject(ctx, s.bucket, key, br, size, minio.PutObjectOptions{
		ContentType: mime,
	})
	if err != nil {
		return "", fmt.Errorf("上传对象 %s: %w", key, err)
	}
	return fmt.Sprintf("%s/%s/%s", s.publicBase, s.bucket, key), nil
}

// ensurePrivate 匿名探测桶私有性：私有桶对不存在的对象也拒绝（AccessDenied），
// 公开桶会先暴露存在性（NoSuchKey）。
func ensurePrivate(ctx context.Context, client *minio.Client, endpoint, bucket string) error {
	anon, err := minio.New(endpoint, &minio.Options{})
	if err != nil {
		return fmt.Errorf("构造匿名客户端: %w", err)
	}
	_, err = anon.StatObject(ctx, bucket, probeKey, minio.StatObjectOptions{})
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Code == "AccessDenied" {
		return nil // 私有：未授权读取被拒
	}
	return fmt.Errorf("桶 %s 非私有（匿名可探测），请配置为私有或换桶", bucket)
}

// randHex 生成 16 位随机十六进制串（对象名唯一性，避免标准库额外依赖）。
func randHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属系统级异常；退化为时间戳保证非空唯一性。
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
