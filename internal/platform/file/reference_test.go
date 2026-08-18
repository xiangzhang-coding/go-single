package file

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagedReferenceRoundTrip(t *testing.T) {
	key := "users/42/image/20260818/0123456789abcdef0123456789abcdef.png"
	reference := referenceForKey(key)
	require.NotContains(t, reference, "users/42")
	require.Contains(t, reference, "/files/")

	got, err := parseReference(reference)
	require.NoError(t, err)
	require.Equal(t, key, got.Key)
	require.Equal(t, int64(42), got.OwnerID)
	require.Equal(t, KindImage, got.Kind)
}

func TestManagedReferenceRejectsExternalAndMalformedValues(t *testing.T) {
	for _, reference := range []string{
		"https://cdn.example.com/a.png",
		"/files/not-base64!",
		referenceForKey("users/42/image/../../secret.png"),
		referenceForKey("users/0/image/20260818/0123456789abcdef0123456789abcdef.png"),
		referenceForKey("users/42/video/20260818/0123456789abcdef0123456789abcdef.png"),
		referenceForKey("users/42/image/20260818/short.png"),
	} {
		_, err := parseReference(reference)
		require.ErrorIs(t, err, ErrInvalidReference, reference)
	}
}
