package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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

func (f *fakeUsers) SearchByUsername(_ context.Context, prefix string, limit int) ([]model.User, error) {
	users := make([]model.User, 0, limit)
	for _, u := range f.byID {
		if strings.HasPrefix(u.Username, prefix) {
			users = append(users, *u)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	if len(users) > limit {
		users = users[:limit]
	}
	return users, nil
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

func TestSearchUsersByPrefix(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})
	// 注册一批用户，让 fake 仓储按 id 升序返回。
	names := []string{"alice", "alex", "bob", "alina"}
	for _, n := range names {
		_, err := svc.Register(context.Background(), n, "secret123")
		require.NoError(t, err)
	}

	got, err := svc.Search(context.Background(), "al", 10)
	require.NoError(t, err)
	usernames := make([]string, 0, len(got))
	for _, u := range got {
		usernames = append(usernames, u.Username)
	}
	// fake 按注册顺序（id 升序）返回：alice(1) → alex(2) → alina(4)。
	assert.Equal(t, []string{"alice", "alex", "alina"}, usernames)
}

func TestSearchUsersLimit(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})
	for _, n := range []string{"u11", "u22", "u33", "u44", "u55"} {
		_, err := svc.Register(context.Background(), n, "secret123")
		require.NoError(t, err)
	}

	// 非法 limit（0/负）收敛为默认 10；超上限截断为 20。
	got, err := svc.Search(context.Background(), "u", 0)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	got, err = svc.Search(context.Background(), "u", 999)
	require.NoError(t, err)
	assert.Len(t, got, 5)
}

func TestSearchUsersValidation(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	// 空前缀被拒；超长前缀（>32 字节）被拒。
	_, err := svc.Search(context.Background(), "", 10)
	require.ErrorIs(t, err, ErrInvalidUsername)
	_, err = svc.Search(context.Background(), "abcdefghijklmnopqrstuvwxyz123456789", 10)
	require.ErrorIs(t, err, ErrInvalidUsername)

	// 无匹配：空列表而非报错。
	got, err := svc.Search(context.Background(), "zzz", 10)
	require.NoError(t, err)
	assert.Empty(t, got)
}
