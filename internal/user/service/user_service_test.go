package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	updateErr  error
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

func (f *fakeUsers) UsernameRateLimitKey(_ context.Context, username string) (string, error) {
	return strings.ToLower(strings.TrimSpace(username)), nil
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*model.User, error) {
	return f.byID[id], nil
}

func (f *fakeUsers) UpdateProfile(_ context.Context, userID int64, patch repository.ProfilePatch) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	stored, ok := f.byID[userID]
	if !ok {
		return repository.ErrUserNotFound
	}
	if patch.Nickname != nil {
		stored.Nickname = *patch.Nickname
	}
	if patch.AvatarURL != nil {
		stored.AvatarURL = *patch.AvatarURL
	}
	return nil
}

func (f *fakeUsers) HasAvatarURL(_ context.Context, reference string) (bool, error) {
	for _, u := range f.byID {
		if u.AvatarURL == reference {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeUsers) SearchByUsername(_ context.Context, prefix string, limit int) ([]model.PublicUser, error) {
	users := make([]model.PublicUser, 0, limit)
	for _, u := range f.byID {
		if strings.HasPrefix(u.Username, prefix) {
			users = append(users, model.PublicUser{ID: u.ID, Username: u.Username})
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	if len(users) > limit {
		users = users[:limit]
	}
	return users, nil
}

func (f *fakeUsers) GetPublicByIDs(_ context.Context, ids []int64) ([]model.PublicUser, error) {
	users := make([]model.PublicUser, 0, len(ids))
	for _, id := range ids {
		if u := f.byID[id]; u != nil {
			users = append(users, model.PublicUser{ID: u.ID, Username: u.Username})
		}
	}
	return users, nil
}

type fakeIssuer struct {
	token string
	err   error
}

func (f *fakeIssuer) Issue(_ int64, _ string) (string, error) { return f.token, f.err }

type fakeMedia struct{}

func (fakeMedia) IsOwned(_ context.Context, ownerID int64, reference, kind string) (bool, error) {
	if kind != "image" || reference != fmt.Sprintf("/files/user-%d-image", ownerID) {
		return false, nil
	}
	return true, nil
}

func newTestService(users *fakeUsers, addresses *fakeAddresses, issuer TokenIssuer) Service {
	return NewWithMedia(repository.Store{Users: users, Addresses: addresses}, issuer, fakeMedia{})
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

func TestUnknownLoginDummyHashUsesDefaultBcryptCost(t *testing.T) {
	cost, err := bcrypt.Cost([]byte(dummyPasswordHash))
	require.NoError(t, err)
	require.Equal(t, bcrypt.DefaultCost, cost)
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

// ---- 个人资料（UpdateProfile）----

func strPtr(s string) *string { return &s }

func TestUpdateProfileNicknameAndAvatar(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{token: "t"})

	u, err := svc.Register(context.Background(), "erin", "secret123")
	require.NoError(t, err)

	avatar := fmt.Sprintf("/files/user-%d-image", u.ID)
	got, err := svc.UpdateProfile(context.Background(), u.ID, ProfileParams{
		Nickname:  strPtr("  小艾  "),
		AvatarURL: strPtr(avatar),
	})
	require.NoError(t, err)
	assert.Equal(t, "小艾", got.Nickname, "昵称应 trim 后落库")
	assert.Equal(t, avatar, got.AvatarURL)

	// fake 与返回一致（落库生效）。
	stored, err := svc.GetByID(context.Background(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, "小艾", stored.Nickname)
	assert.Equal(t, avatar, stored.AvatarURL)
}

func TestUpdateProfilePartialAndClear(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	u, err := svc.Register(context.Background(), "frank", "secret123")
	require.NoError(t, err)

	avatar := fmt.Sprintf("/files/user-%d-image", u.ID)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{
		Nickname:  strPtr("老王"),
		AvatarURL: strPtr(avatar),
	})
	require.NoError(t, err)

	// 只改昵称：头像不动（PATCH 语义）。
	got, err := svc.UpdateProfile(context.Background(), u.ID, ProfileParams{Nickname: strPtr("王老")})
	require.NoError(t, err)
	assert.Equal(t, "王老", got.Nickname)
	assert.Equal(t, avatar, got.AvatarURL)

	// 空串清空头像：昵称不动。
	got, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("")})
	require.NoError(t, err)
	assert.Equal(t, "王老", got.Nickname)
	assert.Empty(t, got.AvatarURL)
}

type interleavingUsers struct {
	*fakeUsers
	mu            sync.Mutex
	nicknameReady chan struct{}
	avatarDone    chan struct{}
}

func (f *interleavingUsers) GetByID(_ context.Context, id int64) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.byID[id]
	if u == nil {
		return nil, nil
	}
	copy := *u
	return &copy, nil
}

func (f *interleavingUsers) UpdateProfile(_ context.Context, userID int64, patch repository.ProfilePatch) error {
	if patch.Nickname != nil && *patch.Nickname == "新昵称" {
		close(f.nicknameReady)
		<-f.avatarDone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := f.byID[userID]
	if patch.Nickname != nil {
		stored.Nickname = *patch.Nickname
	}
	if patch.AvatarURL != nil {
		stored.AvatarURL = *patch.AvatarURL
		close(f.avatarDone)
	}
	return nil
}

func TestConcurrentPartialProfileUpdatesPreserveBothFields(t *testing.T) {
	base := newFakeUsers()
	u := &model.User{ID: 1, Username: "parallel", Nickname: "旧昵称"}
	base.byID[u.ID] = u
	base.byUsername[u.Username] = u
	repo := &interleavingUsers{
		fakeUsers: base, nicknameReady: make(chan struct{}), avatarDone: make(chan struct{}),
	}
	svc := NewWithMedia(repository.Store{Users: repo, Addresses: newFakeAddresses()}, &fakeIssuer{}, fakeMedia{})

	nicknameErr := make(chan error, 1)
	go func() {
		_, err := svc.UpdateProfile(context.Background(), u.ID, ProfileParams{Nickname: strPtr("新昵称")})
		nicknameErr <- err
	}()
	<-repo.nicknameReady

	avatar := fmt.Sprintf("/files/user-%d-image", u.ID)
	_, err := svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: &avatar})
	require.NoError(t, err)
	require.NoError(t, <-nicknameErr)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, "新昵称", repo.byID[u.ID].Nickname)
	require.Equal(t, avatar, repo.byID[u.ID].AvatarURL)
}

func TestUpdateProfileValidation(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	u, err := svc.Register(context.Background(), "grace", "secret123")
	require.NoError(t, err)

	// 空请求（无任何字段）被拒。
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{})
	require.ErrorIs(t, err, ErrInvalidProfile)

	// 昵称超 32 个字符被拒（rune 计数，中文合法）。
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{Nickname: strPtr(strings.Repeat("昵", 33))})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{Nickname: strPtr(strings.Repeat("昵", 32))})
	require.NoError(t, err)

	// 头像必须是当前用户拥有的系统托管图片；外链和他人引用均被拒。
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("ftp://example.com/a.png")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("javascript:alert(1)")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("https://cdn.example.com/a.png")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("/files/user-2-image")})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("/files/" + strings.Repeat("a", 249))})
	require.ErrorIs(t, err, ErrInvalidProfile)

	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr(fmt.Sprintf("/files/user-%d-image", u.ID))})
	require.NoError(t, err)
}

func TestUpdateProfileUnknownUser(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	_, err := svc.UpdateProfile(context.Background(), 999, ProfileParams{Nickname: strPtr("幽灵")})
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestUpdateProfileRepoErrorPropagates(t *testing.T) {
	repo := newFakeUsers()
	svc := newTestService(repo, newFakeAddresses(), &fakeIssuer{})

	u, err := svc.Register(context.Background(), "heidi", "secret123")
	require.NoError(t, err)
	repo.updateErr = fmt.Errorf("db down: %w", errors.New("connection refused"))

	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{Nickname: strPtr("x")})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidProfile)
}

type failingMedia struct{ err error }

func (f failingMedia) IsOwned(context.Context, int64, string, string) (bool, error) {
	return false, f.err
}

func TestUpdateProfileMediaErrorPropagates(t *testing.T) {
	repo := newFakeUsers()
	base := newTestService(repo, newFakeAddresses(), &fakeIssuer{})
	u, err := base.Register(context.Background(), "mediaerr", "secret123")
	require.NoError(t, err)

	wantErr := errors.New("minio unavailable")
	svc := NewWithMedia(repository.Store{Users: repo, Addresses: newFakeAddresses()}, &fakeIssuer{}, failingMedia{err: wantErr})
	_, err = svc.UpdateProfile(context.Background(), u.ID, ProfileParams{AvatarURL: strPtr("/files/ref")})
	require.ErrorIs(t, err, wantErr)
}
