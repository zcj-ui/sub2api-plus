package service

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientAuthURLStub struct{}

func (s *openaiOAuthClientAuthURLStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_GenerateAuthURL_OpenAIKeepsCodexFlow(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI)
	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.ClientID, q.Get("client_id"))
	require.Equal(t, "true", q.Get("codex_cli_simplified_flow"))

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, openai.ClientID, session.ClientID)
}

func TestOpenAIOAuthService_GenerateAuthURLRejectsInactiveOrExpiredProxy(t *testing.T) {
	proxyID := int64(91)
	for _, tc := range []struct {
		name  string
		proxy *Proxy
		want  string
	}{
		{name: "disabled", proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: StatusDisabled}, want: "not active"},
		{name: "expired", proxy: func() *Proxy {
			expires := time.Now().Add(-time.Minute)
			return &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: StatusActive, ExpiresAt: &expires}
		}(), want: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewOpenAIOAuthService(&configuredProxyLookupStub{proxy: tc.proxy}, &openaiOAuthClientAuthURLStub{})
			defer svc.Stop()
			_, err := svc.GenerateAuthURL(context.Background(), &proxyID, "", PlatformOpenAI)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
