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
)

// phoneRe 中国大陆手机号（学习点：字段级正则校验）。
var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

// TokenIssuer 签发登录令牌（自签 JWT 实现，见 platform/auth）。
type TokenIssuer interface {
	Issue(userID int64, role string) (string, error)
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

// Service user 模块的业务接口。
type Service interface {
	Register(ctx context.Context, username, password string) (*model.User, error)
	Login(ctx context.Context, username, password string) (*model.User, string, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)

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
}

// New 构造 user 服务。
func New(store repository.Store, tokens TokenIssuer) Service {
	return &userService{store: store, tokens: tokens}
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
	if u == nil {
		return nil, "", ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, "", ErrInvalidCredentials
	}

	token, err := s.tokens.Issue(u.ID, u.Role)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
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

// ---- 地址簿 ----

// CreateAddress 新增地址：首条自动设为默认；显式 IsDefault=true 同样设为默认。
func (s *userService) CreateAddress(ctx context.Context, userID int64, p AddressParams) (*model.Address, error) {
	cleaned, err := validateAddress(p)
	if err != nil {
		return nil, err
	}
	// 先计数后落库：首条判定避免并发下"两条都以为自己是首条"的竞态；
	// 即便并发，SetDefault 为单条 UPDATE 指针，最终仍恰好一个默认。
	n, err := s.store.Addresses.CountByUser(ctx, userID)
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
	if err := s.store.Addresses.Create(ctx, a); err != nil {
		return nil, err
	}
	if cleaned.IsDefault || n == 0 {
		if err := s.store.Addresses.SetDefault(ctx, userID, a.ID); err != nil {
			// 设默认失败回滚刚落库的行，避免"新增成功但无默认"的部分状态。
			_ = s.store.Addresses.Delete(ctx, a.ID)
			return nil, err
		}
		a.IsDefault = true
	}
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

// DeleteAddress 删除地址；若删的是默认地址，FK ON DELETE SET NULL 自动解除指向，
// 再由 EnsureDefaultExists 自愈：仍有余下地址时把最新一条提为默认。
func (s *userService) DeleteAddress(ctx context.Context, userID, id int64) error {
	if err := s.ensureOwned(ctx, userID, id); err != nil {
		return err
	}
	if err := s.store.Addresses.Delete(ctx, id); err != nil {
		return err
	}
	return s.store.Addresses.EnsureDefaultExists(ctx, userID)
}

func (s *userService) ListAddresses(ctx context.Context, userID int64) ([]model.Address, error) {
	return s.store.Addresses.ListByUser(ctx, userID)
}

// SetDefaultAddress 设为默认：owner 校验后单条 UPDATE 切换指针，旧默认自动失效。
func (s *userService) SetDefaultAddress(ctx context.Context, userID, id int64) error {
	if err := s.ensureOwned(ctx, userID, id); err != nil {
		return err
	}
	return s.store.Addresses.SetDefault(ctx, userID, id)
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
