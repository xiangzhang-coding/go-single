package file

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig 对象存储连接配置（与 auth.JWTConfig 同为先例：适配器自带配置结构）。
type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

const (
	sniffSize = 512
	probeKey  = "users/.privacy-probe-ensure-private"
)

var (
	ErrObjectNotFound     = errors.New("managed file not found")
	ErrReferenceForbidden = errors.New("managed file does not belong to user")
)

// ObjectInfo 是业务层验证与 HTTP 读取所需的最小对象元数据。
type ObjectInfo struct {
	Reference   string
	OwnerID     int64
	Kind        string
	ContentType string
	Size        int64
	Filename    string
}

// StoredObject 是经 MinIO 读取的私有对象流。
type StoredObject struct {
	ObjectInfo
	io.ReadCloser
}

// MinIO 文件存储实现：私有桶 + 内容校验 + 托管引用 + 鉴权代理读取。
type MinIO struct {
	client   *minio.Client
	bucket   string
	usage    UsageStore
	quotaCfg QuotaConfig
}

// NewMinIO 构造 MinIO 实现：建连、确保桶存在且私有。
func NewMinIO(cfg MinIOConfig, usage UsageStore, quotaCfg QuotaConfig) (*MinIO, error) {
	if usage == nil || quotaCfg.MaxBytesPerUser < 1 || quotaCfg.MaxObjectsPerUser < 1 {
		return nil, fmt.Errorf("%w: bytes=%d objects=%d", ErrQuotaConfig, quotaCfg.MaxBytesPerUser, quotaCfg.MaxObjectsPerUser)
	}
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
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("创建桶 %s: %w", cfg.Bucket, err)
		}
	}
	if err := ensurePrivate(ctx, client, cfg.Endpoint, cfg.Bucket); err != nil {
		return nil, err
	}
	return &MinIO{client: client, bucket: cfg.Bucket, usage: usage, quotaCfg: quotaCfg}, nil
}

// Upload 按媒体类型校验内容后写入私有桶。对象 key 固化上传者和类型，
// 返回后端文件接口引用，不暴露 MinIO 地址。
func (s *MinIO) Upload(ctx context.Context, ownerID int64, kind string, r io.Reader, size int64, filename string) (*ObjectInfo, error) {
	if ownerID <= 0 {
		return nil, ErrReferenceForbidden
	}
	br := bufio.NewReader(r)
	header, _ := br.Peek(sniffSize)
	ext, contentType, err := validateUpload(kind, header, size, filename)
	if err != nil {
		return nil, err
	}
	if err := s.usage.Reserve(ctx, ownerID, size, s.quotaCfg.MaxBytesPerUser, s.quotaCfg.MaxObjectsPerUser); err != nil {
		return nil, err
	}

	key := fmt.Sprintf("users/%d/%s/%s/%s.%s", ownerID, kind, time.Now().Format("20060102"), randHex(32), ext)
	cleanName := sanitizeFilename(filename)
	_, err = s.client.PutObject(ctx, s.bucket, key, br, size, minio.PutObjectOptions{
		ContentType: contentType,
		UserMetadata: map[string]string{
			"filename-b64": base64.RawURLEncoding.EncodeToString([]byte(cleanName)),
		},
	})
	if err != nil {
		uploadErr := fmt.Errorf("上传对象 %s: %w", key, err)
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if releaseErr := s.usage.Release(releaseCtx, ownerID, size); releaseErr != nil {
			return nil, errors.Join(uploadErr, releaseErr)
		}
		return nil, uploadErr
	}
	return &ObjectInfo{
		Reference: referenceForKey(key), OwnerID: ownerID, Kind: kind,
		ContentType: contentType, Size: size, Filename: cleanName,
	}, nil
}

// IsOwned 校验引用由本系统管理、对象真实存在，且上传者和类型匹配。
// 无效、越权或对象不存在返回 (false, nil)，存储故障原样返回供业务映射 5xx。
func (s *MinIO) IsOwned(ctx context.Context, ownerID int64, reference, kind string) (bool, error) {
	managed, err := parseReference(reference)
	if err != nil {
		return false, nil
	}
	if managed.OwnerID != ownerID {
		return false, nil
	}
	if managed.Kind != kind {
		return false, nil
	}
	_, err = s.stat(ctx, managed)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Open 打开一个托管对象，并从私有桶元数据恢复安全文件名与响应信息。
func (s *MinIO) Open(ctx context.Context, reference string) (*StoredObject, error) {
	managed, err := parseReference(reference)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, managed.Key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("读取对象 %s: %w", managed.Key, err)
	}
	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, mapObjectError(managed.Key, err)
	}
	return &StoredObject{ObjectInfo: objectInfo(managed, stat), ReadCloser: obj}, nil
}

func (s *MinIO) stat(ctx context.Context, managed managedReference) (*ObjectInfo, error) {
	stat, err := s.client.StatObject(ctx, s.bucket, managed.Key, minio.StatObjectOptions{})
	if err != nil {
		return nil, mapObjectError(managed.Key, err)
	}
	info := objectInfo(managed, stat)
	return &info, nil
}

func objectInfo(managed managedReference, stat minio.ObjectInfo) ObjectInfo {
	return ObjectInfo{
		Reference: referenceForKey(managed.Key), OwnerID: managed.OwnerID, Kind: managed.Kind,
		ContentType: stat.ContentType, Size: stat.Size, Filename: filenameFromMetadata(stat, managed.Key),
	}
}

func filenameFromMetadata(stat minio.ObjectInfo, key string) string {
	var encoded string
	for name, value := range stat.UserMetadata {
		if strings.EqualFold(name, "filename-b64") {
			encoded = value
			break
		}
	}
	if encoded == "" {
		encoded = stat.Metadata.Get("X-Amz-Meta-Filename-B64")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(encoded); err == nil && len(decoded) > 0 {
		return sanitizeFilename(string(decoded))
	}
	return filepath.Base(key)
}

func sanitizeFilename(filename string) string {
	filename = strings.ReplaceAll(filename, "\\", "/")
	filename = filepath.Base(filename)
	filename = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(filename))
	if filename == "" || filename == "." {
		return "file"
	}
	if len(filename) > 255 {
		ext := filepath.Ext(filename)
		if len(ext) > 32 {
			ext = ""
		}
		base := strings.TrimSuffix(filename, ext)
		limit := 255 - len(ext)
		var truncated strings.Builder
		for _, r := range base {
			if truncated.Len()+utf8.RuneLen(r) > limit {
				break
			}
			truncated.WriteRune(r)
		}
		filename = truncated.String() + ext
	}
	return filename
}

func mapObjectError(key string, err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject" || resp.Code == "NotFound" {
		return fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return fmt.Errorf("读取对象 %s: %w", key, err)
}

func ensurePrivate(ctx context.Context, client *minio.Client, endpoint, bucket string) error {
	policy, err := client.GetBucketPolicy(ctx, bucket)
	if err == nil {
		if strings.TrimSpace(policy) != "" {
			return fmt.Errorf("桶 %s 配置了访问策略，请移除策略并保持私有", bucket)
		}
	} else if resp := minio.ToErrorResponse(err); resp.Code != "NoSuchBucketPolicy" {
		return fmt.Errorf("检查桶 %s 访问策略: %w", bucket, err)
	}

	anon, err := minio.New(endpoint, &minio.Options{})
	if err != nil {
		return fmt.Errorf("构造匿名客户端: %w", err)
	}
	_, err = anon.StatObject(ctx, bucket, probeKey, minio.StatObjectOptions{})
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Code == "AccessDenied" {
		return nil
	}
	return fmt.Errorf("桶 %s 非私有（匿名可探测），请配置为私有或换桶", bucket)
}

func randHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
