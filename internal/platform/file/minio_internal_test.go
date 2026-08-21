package file

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnonymousPrivacyProbeUsesConfiguredTLS(t *testing.T) {
	client, err := newAnonymousClient("storage.example.test", true)
	require.NoError(t, err)
	require.Equal(t, "https", client.EndpointURL().Scheme)
}
