package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
	"github.com/xiangzhang-coding/go-single/internal/user/repository"
)

// fakeUsers 是仓储 seam 的测试替身。
type fakeUsers struct {
	byUsername map[string]*model.User
	byID       map[int64]*model.User
	nextID     int64
	createErr  error
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byUsername: map[string]*model.User{}, byID: map[int64]*model.User{}, nextID: 1}
}

func (f *fakeUsers) Create(_ context.Context, u *model.User) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, exists := f.byUsername[u.Username]; exists {
		return repository.ErrUsernameExists
	}
	u.ID = f.nextID
	f.nextID++
	f.byUsername[u.Username] = u
	f.byID[u.ID] = u
	return nil
}

func (f *fakeUsers) GetByUsername(_ context.Context, username string) (*model.User, error) {
	return f.byUsername[username], nil
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*model.User, error) {
	return f.byID[id], nil
}

type fakeIssuer struct {
	token string
	err   error
}

func (f *fakeIssuer) Issue(_ int64, _ string) (string, error) { return f.token, f.err }

func newTestService(users *fakeUsers, addresses *fakeAddresses, issuer TokenIssuer) Service {
	return New(repository.Store{Users: users, Addresses: addresses}, issuer)
}

func TestRegisterSuccess(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{token: "t"})

	u, err := svc.Register(context.Background(), "alice", "secret123")
	require.NoError(t, err)
	assert.Equal(t, "alice", u.Username)
	assert.Equal(t, model.RoleUser, u.Role)
	assert.NotEmpty(t, u.PasswordHash)
	assert.NotEqual(t, "secret123", u.PasswordHash)
	// 密码已 bcrypt 加密且可校验。
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("secret123")))
}

func TestRegisterDuplicateUsername(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	_, err := svc.Register(context.Background(), "alice", "secret123")
	require.NoError(t, err)
	_, err = svc.Register(context.Background(), "alice", "secret456")
	require.ErrorIs(t, err, ErrUsernameTaken)
}

func TestRegisterInvalidInput(t *testing.T) {
	svc := newTestService(newFakeUsers(), newFakeAddresses(), &fakeIssuer{})

	_, err := svc.Register(context.Background(), "ab", "secret123")
	require.ErrorIs(t, err, ErrInvalidUsername)

	_, err = svc.Register(context.Background(), "alice", "12345")
	require.ErrorIs(t, err, ErrInvalidPassword)

	_, err = svc.Register(context.Background(), "alice", "x")
	require.ErrorIs(t, err, ErrInvalidPassword)
}

func TestLoginSuccess(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{token: "jwt-token"})

	_, err := svc.Register(context.Background(), "bob", "secret123")
	require.NoError(t, err)

	u, token, err := svc.Login(context.Background(), "bob", "secret123")
	require.NoError(t, err)
	assert.Equal(t, "bob", u.Username)
	assert.Equal(t, "jwt-token", token)
}

func TestLoginWrongPassword(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{token: "t"})

	_, err := svc.Register(context.Background(), "bob", "secret123")
	require.NoError(t, err)

	_, _, err = svc.Login(context.Background(), "bob", "wrong-pass")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLoginUnknownUser(t *testing.T) {
	svc := newTestService(newFakeUsers(), newFakeAddresses(), &fakeIssuer{token: "t"})

	_, _, err := svc.Login(context.Background(), "ghost", "secret123")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestGetByID(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	u, err := svc.Register(context.Background(), "carol", "secret123")
	require.NoError(t, err)

	got, err := svc.GetByID(context.Background(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)

	_, err = svc.GetByID(context.Background(), 999)
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestRegisterRepoErrorPropagates(t *testing.T) {
	repo := newFakeUsers()
	repo.createErr = fmt.Errorf("db down: %w", errors.New("connection refused"))
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	_, err := svc.Register(context.Background(), "dave", "secret123")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUsernameTaken)
}
