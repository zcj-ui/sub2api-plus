package service

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestValidateOpenAIImageDownloadURLRejectsUnsafeTargets(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "loopback", url: "http://127.0.0.1/image.png"},
		{name: "metadata", url: "http://169.254.169.254/latest/meta-data/image.png"},
		{name: "private IPv6", url: "http://[::1]/image.png"},
		{name: "multicast IPv6", url: "http://[ff02::1]/image.png"},
		{name: "localhost name", url: "http://localhost/image.png"},
		{name: "unsupported scheme", url: "file:///etc/passwd"},
		{name: "userinfo", url: "https://user:pass@example.com/image.png"},
		{name: "fragment", url: "https://example.com/image.png#fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateOpenAIImageDownloadURL(tt.url)
			require.Error(t, err)
		})
	}
}

func TestValidateOpenAIImageDownloadURLAcceptsPublicLiteral(t *testing.T) {
	got, err := validateOpenAIImageDownloadURL(" https://1.1.1.1/image.png?sig=keep ")
	require.NoError(t, err)
	require.Equal(t, "https://1.1.1.1/image.png?sig=keep", got)
}

func TestOpenAIImageDownloadRejectsUnsafeURLBeforeTransport(t *testing.T) {
	client := req.C()
	var calls int
	client.GetTransport().WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			calls++
			return next.RoundTrip(r)
		}
	})

	_, err := downloadOpenAIImageBytes(context.Background(), client, nil, "http://127.0.0.1/image.png", 1024)
	require.Error(t, err)
	require.Zero(t, calls, "unsafe URL must be rejected before any network round trip")
}

func TestOpenAIImageDownloadRedirectPolicyRejectsPrivateTarget(t *testing.T) {
	require.Error(t, openAIImageDownloadRedirectPolicy(&http.Request{
		URL: mustParseTestURL(t, "http://127.0.0.1/internal.png"),
	}, nil))
}

func mustParseTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestOpenAIImageDownloadURLErrorDoesNotEchoQuerySecrets(t *testing.T) {
	_, err := validateOpenAIImageDownloadURL("http://127.0.0.1/image.png?signature=" + strings.Repeat("x", 64))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "signature=")
}
