package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type avatarAccessStub struct {
	allowed bool
	err     error
}

func (s avatarAccessStub) CanReadAvatar(context.Context, string) (bool, error) {
	return s.allowed, s.err
}

type postAccessStub struct {
	allowed bool
	err     error
}

func (s postAccessStub) CanReadImage(context.Context, int64, string) (bool, error) {
	return s.allowed, s.err
}

type chatAccessStub struct {
	allowed bool
	err     error
}

func (s chatAccessStub) CanReadMedia(context.Context, int64, string) (bool, error) {
	return s.allowed, s.err
}

func TestMediaAccessAuthorizerAcceptsAnyBusinessAuthorization(t *testing.T) {
	cases := []struct {
		name  string
		users bool
		posts bool
		chat  bool
		want  bool
	}{
		{name: "bound avatar", users: true, want: true},
		{name: "visible post image", posts: true, want: true},
		{name: "conversation media", chat: true, want: true},
		{name: "unbound reference", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := mediaAccessAuthorizer{
				users: avatarAccessStub{allowed: tc.users},
				posts: postAccessStub{allowed: tc.posts},
				chat:  chatAccessStub{allowed: tc.chat},
			}
			allowed, err := authorizer.CanRead(context.Background(), 7, "/files/ref")
			require.NoError(t, err)
			require.Equal(t, tc.want, allowed)
		})
	}
}

func TestMediaAccessAuthorizerPropagatesRepositoryFailure(t *testing.T) {
	wantErr := errors.New("database unavailable")
	authorizer := mediaAccessAuthorizer{
		users: avatarAccessStub{},
		posts: postAccessStub{err: wantErr},
		chat:  chatAccessStub{allowed: true},
	}
	allowed, err := authorizer.CanRead(context.Background(), 7, "/files/ref")
	require.False(t, allowed)
	require.ErrorIs(t, err, wantErr)
}
