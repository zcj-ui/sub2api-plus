//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type upstreamModelsEOFTransport struct{ err error }

func (u *upstreamModelsEOFTransport) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, u.err
}

func (u *upstreamModelsEOFTransport) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return nil, u.err
}

func newUpstreamModelsChromeFallbackService(upstream HTTPUpstream) *AccountTestService {
	return NewAccountTestService(
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		&config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		nil,
	)
}

func TestFetchUpstreamSupportedModels_RetriesOpenAIAPIKeyWithChromeAfterEOF(t *testing.T) {
	svc := newUpstreamModelsChromeFallbackService(&upstreamModelsEOFTransport{err: io.EOF})
	called := false
	svc.openAIModelSyncChromeRequester = func(req *http.Request, proxyURL string) (*http.Response, error) {
		called = true
		require.Empty(t, proxyURL)
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, "https://upstream.example/v1/models", req.URL.String())
		require.Equal(t, "Bearer test-key", req.Header.Get("Authorization"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-5.6"}]}`)),
		}, nil
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://upstream.example/v1"},
	})

	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, []string{"gpt-5.6"}, models)
}

func TestFetchUpstreamSupportedModels_ChromeFallbackRetriesTransientEOF(t *testing.T) {
	svc := newUpstreamModelsChromeFallbackService(&upstreamModelsEOFTransport{err: io.EOF})
	calls := 0
	svc.openAIModelSyncChromeRequester = func(*http.Request, string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-5.6"}]}`)),
		}, nil
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://upstream.example/v1"},
	})

	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"gpt-5.6"}, models)
}

func TestFetchUpstreamSupportedModels_DoesNotChromeRetryNonOpenAIAccounts(t *testing.T) {
	svc := newUpstreamModelsChromeFallbackService(&upstreamModelsEOFTransport{err: io.EOF})
	called := false
	svc.openAIModelSyncChromeRequester = func(*http.Request, string) (*http.Response, error) {
		called = true
		return nil, io.EOF
	}
	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID: 13, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://upstream.example"},
	})
	require.Error(t, err)
	require.False(t, called)
}
