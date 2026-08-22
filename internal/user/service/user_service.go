// Package service 承载 user 模块业务：注册（bcrypt）、登录（签发 JWT）与地址簿。
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
	"github.com/xiangzhang-coding/go-single/internal/user/repository"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrUsernameTaken      = errors.New("username already taken")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidPassword    = errors.New("invalid password")

	ErrAddressNotFound  = errors.New("address not found")
	ErrAddressForbidden = errors.New("address does not belong to user")
	ErrInvalidAddress   = errors.New("invalid address")

	ErrInvalidProfile = errors.New("invalid profile")
)

// phoneRe 中国大陆手机号（学习点：字段级正则校验）。
var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

// dummyPasswordHash keeps unknown-account failures on the same bcrypt cost as
// known-account failures, without generating a hash per request.
const dummyPasswordHash = "$2a$10$YDcE3V.LXJpDdAcovEV/D.ZLd2pWN66gelFHvaxI0IHxnCs2yEYRq"

// TokenIssuer 签发登录令牌（自签 JWT 实现，见 platform/auth）。
type TokenIssuer interface {
	Issue(userID int64, role string) (string, error)
}

// MediaValidator 是 user 模块消费的最小媒体端口：头像必须是当前用户上传的图片。
type MediaValidator interface {
	IsOwned(ctx context.Context, ownerID int64, reference, kind string) (bool, error)
}

// AddressParams 地址簿参数（新增/编辑共用）；IsDefault 仅新增时生效。
type AddressParams struct {
	Receiver  string
	Phone     string
	Province  string
	City      string
	District  string
	Detail    string
	IsDefault bool
}

// ProfileParams 个人资料参数（PATCH 语义）：nil 字段不改动，
// 指向空字符串表示清空（数据库列可空，模型以空串表示）。
type ProfileParams struct {
	Nickname  *string
	AvatarURL *string
}

// Service user 模块的业务接口。
type Service interface {
	Register(ctx context.Context, username, password string) (*model.User, error)
	Login(ctx context.Context, username, password string) (*model.User, string, error)
	AuthenticationAccountKey(ctx context.Context, username string) (string, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	// GetPublicByIDs 批量读取最小公开资料，供 social 等模块补齐列表展示。
	GetPublicByIDs(ctx context.Context, ids []int64) (map[int64]model.PublicUser, error)
	// UpdateProfile 更新当前用户个人资料（昵称/头像，PATCH 语义见 ProfileParams）；
	// userID 取自令牌声明，天然只有 owner 本人可改（防 IDOR）。
	UpdateProfile(ctx context.Context, userID int64, p ProfileParams) (*model.User, error)
	// CanReadAvatar 判断引用是否仍绑定为某个用户头像；供私有媒体读取授权使用。
	CanReadAvatar(ctx context.Context, reference string) (bool, error)
	// Search 按用户名前缀搜索用户（社交"加好友"发现入口）：
	// 前缀须非空且 ≤32 字符（与注册同名规则），limit 截断到 [1, maxSearchLimit]。
	Search(ctx context.Context, currentUserID int64, username string, limit int) ([]model.PublicUser, error)

	// ---- 地址簿 ----
	// CreateAddress 新增地址：首条自动设为默认；IsDefault=true 时显式设为默认。
	CreateAddress(ctx context.Context, userID int64, p AddressParams) (*model.Address, error)
	// UpdateAddress 编辑地址（不改动默认指向）。
	UpdateAddress(ctx context.Context, userID, id int64, p AddressParams) error
	// DeleteAddress 删除地址；若删的是默认地址，自动把最新余下地址提为默认。
	DeleteAddress(ctx context.Context, userID, id int64) error
	ListAddresses(ctx context.Context, userID int64) ([]model.Address, error)
	// SetDefaultAddress 设为默认：旧默认自动失效（单条 UPDATE 切换指针）。
	SetDefaultAddress(ctx context.Context, userID, id int64) error
	// GetAddress 读取单条地址（owner 校验）：供 order 模块下单固化为地址快照。
	GetAddress(ctx context.Context, userID, id int64) (*model.Address, error)
	// GetDefaultAddress 读取用户默认地址：供秒杀异步落单固化地址快照；
	// 无默认地址返回 (nil, nil)。
	GetDefaultAddress(ctx context.Context, userID int64) (*model.Address, error)
}

type userService struct {
	store  repository.Store
	tokens TokenIssuer
	media  MediaValidator
}

// New 构造 user 服务。
func New(store repository.Store, tokens TokenIssuer) Service {
	return &userService{store: store, tokens: tokens}
}

// NewWithMedia 构造启用托管头像校验的 user 服务。
func NewWithMedia(store repository.Store, tokens TokenIssuer, media MediaValidator) Service {
	return &userService{store: store, tokens: tokens, media: media}
}

func (s *userService) Register(ctx context.Context, username, password string) (*model.User, error) {
	if err := validateCredentials(username, password); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	u := &model.User{Username: username, PasswordHash: string(hash), Role: model.RoleUser}
	if err := s.store.Users.Create(ctx, u); err != nil {
		if errors.Is(err, repository.ErrUsernameExists) {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	return u, nil
}

func (s *userService) Login(ctx context.Context, username, password string) (*model.User, string, error) {
	u, err := s.store.Users.GetByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	hash := dummyPasswordHash
	if u != nil {
		hash = u.PasswordHash
	}
	compareErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if u == nil || compareErr != nil {
		return nil, "", ErrInvalidCredentials
	}

	token, err := s.tokens.Issue(u.ID, u.Role)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *userService) AuthenticationAccountKey(ctx context.Context, username string) (string, error) {
	return s.store.Users.UsernameRateLimitKey(ctx, username)
}

func (s *userService) GetByID(ctx context.Context, id int64) (*model.User, error) {
	u, err := s.store.Users.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (s *userService) GetPublicByIDs(ctx context.Context, ids []int64) (map[int64]model.PublicUser, error) {
	users, err := s.store.Users.GetPublicByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]model.PublicUser, len(users))
	for i := range users {
		byID[users[i].ID] = users[i]
	}
	return byID, nil
}

// UpdateProfile 更新当前用户个人资料：校验 → 读出本人 → 落库；nil 字段不动，空串清空。
func (s *userService) UpdateProfile(ctx context.Context, userID int64, p ProfileParams) (*model.User, error) {
	if p.Nickname == nil && p.AvatarURL == nil {
		return nil, fmt.Errorf("%w: no fields to update", ErrInvalidProfile)
	}
	cleaned, err := validateProfile(p)
	if err != nil {
		return nil, err
	}
	if cleaned.AvatarURL != nil && *cleaned.AvatarURL != "" {
		if s.media == nil {
			return nil, fmt.Errorf("%w: avatar_url must be an owned managed image", ErrInvalidProfile)
		}
		owned, mediaErr := s.media.IsOwned(ctx, userID, *cleaned.AvatarURL, "image")
		if mediaErr != nil {
			return nil, mediaErr
		}
		if !owned {
			return nil, fmt.Errorf("%w: avatar_url must be an owned managed image", ErrInvalidProfile)
		}
	}
	u, err := s.store.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	if cleaned.Nickname != nil {
		u.Nickname = *cleaned.Nickname
	}
	if cleaned.AvatarURL != nil {
		u.AvatarURL = *cleaned.AvatarURL
	}
	if err := s.store.Users.UpdateProfile(ctx, userID, repository.ProfilePatch{
		Nickname: cleaned.Nickname, AvatarURL: cleaned.AvatarURL,
	}); err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	updated, err := s.store.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, ErrUserNotFound
	}
	return updated, nil
}

func (s *userService) CanReadAvatar(ctx context.Context, reference string) (bool, error) {
	if reference == "" {
		return false, nil
	}
	return s.store.Users.HasAvatarURL(ctx, reference)
}

// 搜索默认条数与上限（演示页一次展示数量有限）。
const (
	defaultSearchLimit = 10
	maxSearchLimit     = 20
)

// Search 前缀搜索：校验 → 限量查询（≤maxSearchLimit，非法 limit 用默认值）。
func (s *userService) Search(ctx context.Context, currentUserID int64, username string, limit int) ([]model.PublicUser, error) {
	if username == "" || len(username) > 32 {
		return nil, ErrInvalidUsername
	}
	if limit < 1 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	users, err := s.store.Users.SearchByUsername(ctx, username, currentUserID, limit)
	if err != nil {
		return nil, err
	}
	return users, nil
}

// ---- 地址簿 ----

// CreateAddress 新增地址：仓储在一个事务内创建并决定首条/显式默认。
func (s *userService) CreateAddress(ctx context.Context, userID int64, p AddressParams) (*model.Address, error) {
	cleaned, err := validateAddress(p)
	if err != nil {
		return nil, err
	}
	a := &model.Address{
		UserID:   userID,
		Receiver: cleaned.Receiver,
		Phone:    cleaned.Phone,
		Province: cleaned.Province,
		City:     cleaned.City,
		District: cleaned.District,
		Detail:   cleaned.Detail,
	}
	isDefault, err := s.store.Addresses.CreateWithDefault(ctx, a, cleaned.IsDefault)
	if err != nil {
		return nil, err
	}
	a.IsDefault = isDefault
	return a, nil
}

// UpdateAddress 编辑地址：只改字段，不触碰默认指向。
func (s *userService) UpdateAddress(ctx context.Context, userID, id int64, p AddressParams) error {
	cleaned, err := validateAddress(p)
	if err != nil {
		return err
	}
	if err := s.ensureOwned(ctx, userID, id); err != nil {
		return err
	}
	return s.store.Addresses.Update(ctx, &model.Address{
		ID:       id,
		Receiver: cleaned.Receiver,
		Phone:    cleaned.Phone,
		Province: cleaned.Province,
		City:     cleaned.City,
		District: cleaned.District,
		Detail:   cleaned.Detail,
	})
}

// DeleteAddress 由仓储在一个事务内完成 owner 校验、删除和默认地址补位。
func (s *userService) DeleteAddress(ctx context.Context, userID, id int64) error {
	result, err := s.store.Addresses.DeleteAndEnsureDefault(ctx, userID, id)
	if err != nil {
		return err
	}
	switch result {
	case repository.DeleteAddressDeleted:
		return nil
	case repository.DeleteAddressForbidden:
		return ErrAddressForbidden
	default:
		return ErrAddressNotFound
	}
}

func (s *userService) ListAddresses(ctx context.Context, userID int64) ([]model.Address, error) {
	return s.store.Addresses.ListByUser(ctx, userID)
}

// SetDefaultAddress 由仓储在一个事务内串行 owner 校验与默认指针切换。
func (s *userService) SetDefaultAddress(ctx context.Context, userID, id int64) error {
	result, err := s.store.Addresses.SetDefaultOwned(ctx, userID, id)
	if err != nil {
		return err
	}
	switch result {
	case repository.SetDefaultAddressSet:
		return nil
	case repository.SetDefaultAddressForbidden:
		return ErrAddressForbidden
	default:
		return ErrAddressNotFound
	}
}

// ensureOwned 对象级授权（防 IDOR）：地址不存在 404；归属他人 403。
func (s *userService) ensureOwned(ctx context.Context, userID, id int64) error {
	a, err := s.store.Addresses.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrAddressNotFound
	}
	if a.UserID != userID {
		return ErrAddressForbidden
	}
	return nil
}

// GetAddress 读取单条地址并校验归属；供 order 模块下单时固化为地址快照。
func (s *userService) GetAddress(ctx context.Context, userID, id int64) (*model.Address, error) {
	a, err := s.store.Addresses.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrAddressNotFound
	}
	if a.UserID != userID {
		return nil, ErrAddressForbidden
	}
	return a, nil
}

// GetDefaultAddress 读取用户默认地址；无默认地址返回 (nil, nil)（秒杀落单场景）。
func (s *userService) GetDefaultAddress(ctx context.Context, userID int64) (*model.Address, error) {
	return s.store.Addresses.GetDefaultAddress(ctx, userID)
}

func validateCredentials(username, password string) error {
	if len(username) < 3 || len(username) > 32 {
		return ErrInvalidUsername
	}
	if len(password) < 6 || len(password) > 72 {
		return ErrInvalidPassword
	}
	return nil
}

// validateProfile 校验并返回清理后（trim）的个人资料参数（同 validateAddress 风格）：
// 昵称非空时 ≤32 字符（rune 计数，支持中文）；头像引用为空（清空）或
// ≤255 字节（托管引用的归属与类型由 UpdateProfile 经媒体端口校验）。
func validateProfile(p ProfileParams) (ProfileParams, error) {
	if p.Nickname != nil {
		nickname := strings.TrimSpace(*p.Nickname)
		p.Nickname = &nickname
		if len([]rune(nickname)) > 32 {
			return p, fmt.Errorf("%w: invalid nickname", ErrInvalidProfile)
		}
	}
	if p.AvatarURL != nil {
		url := strings.TrimSpace(*p.AvatarURL)
		p.AvatarURL = &url
		if len(url) > 255 {
			return p, fmt.Errorf("%w: invalid avatar_url", ErrInvalidProfile)
		}
	}
	return p, nil
}

// validateAddress 校验并返回清理后（trim）的参数；字段级规则见各 case。
func validateAddress(p AddressParams) (AddressParams, error) {
	p.Receiver = strings.TrimSpace(p.Receiver)
	p.Province = strings.TrimSpace(p.Province)
	p.City = strings.TrimSpace(p.City)
	p.District = strings.TrimSpace(p.District)
	p.Detail = strings.TrimSpace(p.Detail)
	p.Phone = strings.TrimSpace(p.Phone)

	switch {
	case p.Receiver == "" || len([]rune(p.Receiver)) > 32:
		return p, fmt.Errorf("%w: invalid receiver", ErrInvalidAddress)
	case !phoneRe.MatchString(p.Phone):
		return p, fmt.Errorf("%w: invalid phone", ErrInvalidAddress)
	case p.Province == "" || len([]rune(p.Province)) > 16:
		return p, fmt.Errorf("%w: invalid province", ErrInvalidAddress)
	case p.City == "" || len([]rune(p.City)) > 16:
		return p, fmt.Errorf("%w: invalid city", ErrInvalidAddress)
	case p.District == "" || len([]rune(p.District)) > 16:
		return p, fmt.Errorf("%w: invalid district", ErrInvalidAddress)
	case p.Detail == "" || len([]rune(p.Detail)) > 255:
		return p, fmt.Errorf("%w: invalid detail", ErrInvalidAddress)
	}
	return p, nil
}
