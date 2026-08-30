//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAICodexSpark429UsesModelScopedCooldown(t *testing.T) {
	repo := &stubAntigravityAccountRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 7301, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	resetAt := time.Now().Add(7 * 24 * time.Hour)
	headers := sparkQuotaHeaders()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, headers,
		[]byte(`{"error":{"code":"rate_limit_exceeded"}}`), "gpt-5.3-codex-spark",
	)

	require.False(t, shouldDisable)
	require.Empty(t, repo.rateCalls, "Spark 429 must not write account-level cooldown")
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.3-codex-spark", repo.modelRateLimitCalls[0].modelKey)
	require.WithinDuration(t, resetAt, repo.modelRateLimitCalls[0].resetAt, 2*time.Second)
}

func TestOpenAICodexSpark429DoesNotScopeOrdinaryModel(t *testing.T) {
	repo := &stubAntigravityAccountRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 7302, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(), account, http.StatusTooManyRequests, nil,
		[]byte(`{"error":{"code":"rate_limit_exceeded"}}`), "gpt-5.4",
	)

	require.False(t, shouldDisable, "the first ordinary OAuth 429 remains observational")
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAIStream429SparkKeepsQuotaHeadersForModelScope(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 7303, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := sparkQuotaHeaders()
	payload := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)

	status, shouldDisable := svc.handleOpenAIStreamTerminalAccountSideEffects(nil, account, payload, "quota exhausted", headers, "gpt-5.3-codex-spark")
	require.Equal(t, http.StatusTooManyRequests, status)
	require.False(t, shouldDisable)
	require.Equal(t, 1, repo.setModelRateLimitCalls)
	require.Equal(t, "gpt-5.3-codex-spark", repo.lastModelRateLimitKey)
	require.Greater(t, time.Until(repo.lastModelRateLimitedUntil), 6*24*time.Hour)
	require.Zero(t, repo.setRateLimitedCalls)
}

func TestOpenAIWSErrorEventOrdinary429IgnoresHandshakeQuotaHeaders(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 7304, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := sparkQuotaHeaders()
	payload := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)

	svc.persistOpenAIWSRateLimitSignalForModel(context.Background(), account, headers, payload, "rate_limit_exceeded", "rate_limit_error", "quota exhausted", "gpt-5.3-codex", http.StatusTooManyRequests)
	require.Zero(t, repo.setRateLimitedCalls)
	require.Zero(t, repo.setModelRateLimitCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIWSErrorEventSpark429UsesHandshakeQuotaHeaders(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 7305, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := sparkQuotaHeaders()
	payload := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)

	svc.persistOpenAIWSRateLimitSignalForModel(context.Background(), account, headers, payload, "rate_limit_exceeded", "rate_limit_error", "quota exhausted", "gpt-5.3-codex-spark", http.StatusTooManyRequests)
	require.Equal(t, 1, repo.setModelRateLimitCalls)
	require.Equal(t, "gpt-5.3-codex-spark", repo.lastModelRateLimitKey)
	require.Greater(t, time.Until(repo.lastModelRateLimitedUntil), 6*24*time.Hour)
	require.Zero(t, repo.setRateLimitedCalls)
}

func sparkQuotaHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "20")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")
	return headers
}
