// Package service 承载 user 模块业务：注册（bcrypt）与登录（签发 JWT）。
package service

import (
	"context"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
	"github.com/xiangzhang-coding/go-single/internal/user/repository"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrUsernameTaken       = errors.New("username already taken")
	ErrInvalidCredentials  = errors.New("invalid credentials")
	ErrUserNotFound        = errors.New("user not found")
	ErrInvalidUsername     = errors.New("invalid username")
	ErrInvalidPassword     = errors.New("invalid password")
)

// TokenIssuer 签发登录令牌（自签 JWT 实现，见 platform/auth）。
type TokenIssuer interface {
	Issue(userID int64, role string) (string, error)
}

// Service user 模块的业务接口。
type Service interface {
	Register(ctx context.Context, username, password string) (*model.User, error)
	Login(ctx context.Context, username, password string) (*model.User, string, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
}

type userService struct {
	users  repository.UserRepository
	tokens TokenIssuer
}

// New 构造 user 服务。
func New(users repository.UserRepository, tokens TokenIssuer) Service {
	return &userService{users: users, tokens: tokens}
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
	if err := s.users.Create(ctx, u); err != nil {
		if errors.Is(err, repository.ErrUsernameExists) {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	return u, nil
}

func (s *userService) Login(ctx context.Context, username, password string) (*model.User, string, error) {
	u, err := s.users.GetByUsername(ctx, username)
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
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrUserNotFound
	}
	return u, nil
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
