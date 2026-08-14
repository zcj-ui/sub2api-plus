package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveConfiguredProxyURLFailsClosedForConfiguredProxy(t *testing.T) {
	proxyID := int64(71)

	_, err := resolveConfiguredProxyURL(&Account{Platform: PlatformOpenAI, ProxyID: &proxyID})
	require.Error(t, err)

	mismatched := &Proxy{ID: proxyID + 1, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	_, err = resolveConfiguredProxyURL(&Account{Platform: PlatformOpenAI, ProxyID: &proxyID, Proxy: mismatched})
	require.Error(t, err)

	invalid := &Proxy{ID: proxyID}
	_, err = resolveConfiguredProxyURL(&Account{Platform: PlatformOpenAI, ProxyID: &proxyID, Proxy: invalid})
	require.Error(t, err)

	configured := &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	url, err := resolveConfiguredProxyURL(&Account{Platform: PlatformOpenAI, ProxyID: &proxyID, Proxy: configured})
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", url)
}

func TestResolveConfiguredProxyURLAllowsExplicitDirectAccount(t *testing.T) {
	url, err := resolveConfiguredProxyURL(&Account{Platform: PlatformOpenAI})
	require.NoError(t, err)
	require.Empty(t, url)
}

func TestResolveConfiguredProxyURLIgnoresStaleRelationWithoutProxyID(t *testing.T) {
	url, err := resolveConfiguredProxyURL(&Account{
		Proxy: &Proxy{ID: 12, Protocol: "http", Host: "127.0.0.1", Port: 1080},
	})
	require.NoError(t, err)
	require.Empty(t, url)
}

func TestResolveRequiredOpenAIProxyURLPreservesDirectAccount(t *testing.T) {
	url, err := resolveRequiredOpenAIProxyURL(&Account{Platform: PlatformOpenAI})
	require.NoError(t, err)
	require.Empty(t, url)
}

func TestResolveOpenAIAccountProxyURLPreservesUnconfiguredAccounts(t *testing.T) {
	url, err := resolveOpenAIAccountProxyURL(&Account{Platform: PlatformOpenAI})
	require.NoError(t, err)
	require.Empty(t, url)

	url, err = resolveOpenAIAccountProxyURL(&Account{Platform: PlatformGrok})
	require.NoError(t, err)
	require.Empty(t, url)
}
