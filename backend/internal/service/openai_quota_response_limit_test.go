package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestParseOpenAIRateLimitResetCreditDetailsRejectsMoreThan64Entries(t *testing.T) {
	entries := make([]map[string]any, openAIQuotaMaxResetCreditEntries+1)
	for i := range entries {
		entries[i] = map[string]any{"expires_at": "2099-01-01T00:00:00Z"}
	}
	body, err := json.Marshal(entries)
	require.NoError(t, err)

	details, err := parseOpenAIRateLimitResetCreditDetails(body)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOpenAIQuotaResetCreditEntriesTooMany)
	require.Empty(t, details.Credits, "oversized lists must not be partially exposed")
	require.Empty(t, details.AutoResetCandidates)
}

func TestParseOpenAIRateLimitResetCreditDetailsRejectsOversizedBody(t *testing.T) {
	body := bytes.Repeat([]byte{' '}, int(openAIQuotaMaxResponseBodyBytes)+1)
	_, err := parseOpenAIRateLimitResetCreditDetails(body)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOpenAIQuotaResponseBodyTooLarge)
}

func TestRequestOpenAIQuotaJSONBoundsDecodedResponseBody(t *testing.T) {
	// The wire representation is compressed below 1 MiB, but the decoded JSON
	// body is above the limit. The limiter must run after req/v3 decompression.
	decoded := bytes.Repeat([]byte{' '}, int(openAIQuotaMaxResponseBodyBytes)+1)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write(decoded)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed.Bytes())
	}))
	defer srv.Close()

	client := req.C().SetTimeout(5 * time.Second)
	resp, body, err := requestOpenAIQuotaJSON(
		context.Background(),
		client,
		http.MethodGet,
		srv.URL,
		nil,
		nil,
		nil,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOpenAIQuotaResponseBodyTooLarge)
	require.NotNil(t, resp)
	require.Nil(t, body)
}

func TestRequestOpenAIQuotaJSONAcceptsBodyAtLimit(t *testing.T) {
	body := append(bytes.Repeat([]byte{' '}, int(openAIQuotaMaxResponseBodyBytes)-2), '{', '}')
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := req.C().SetTimeout(5 * time.Second)
	var decoded map[string]any
	resp, responseBody, err := requestOpenAIQuotaJSON(
		context.Background(),
		client,
		http.MethodGet,
		srv.URL,
		nil,
		nil,
		&decoded,
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, responseBody, int(openAIQuotaMaxResponseBodyBytes))
	require.Empty(t, decoded)
}

func TestOpenAIQuotaServiceRejectsOversizedUsageBody(t *testing.T) {
	accountID := int64(9601)
	account := &Account{
		ID:       accountID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "quota-limit-account",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{accountID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	provider := NewOpenAITokenProvider(repo, tokens, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, int(openAIQuotaMaxResponseBodyBytes)+1))
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(srv))
	_, err := svc.QueryUsage(context.Background(), accountID)
	require.Error(t, err)
	require.Equal(t, "OPENAI_QUOTA_RESPONSE_TOO_LARGE", infraerrors.Reason(err))
	require.ErrorIs(t, err, ErrOpenAIQuotaResponseBodyTooLarge)
}

func TestOpenAIQuotaServiceRejectsOversizedResetDetailsBody(t *testing.T) {
	accountID := int64(9602)
	account := &Account{
		ID:       accountID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "quota-limit-details",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{accountID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	provider := NewOpenAITokenProvider(repo, tokens, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/backend-api/wham/usage" {
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`))
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, int(openAIQuotaMaxResponseBodyBytes)+1))
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(srv))
	_, err := svc.QueryUsage(context.Background(), accountID)
	require.Error(t, err)
	require.Equal(t, "OPENAI_QUOTA_RESPONSE_TOO_LARGE", infraerrors.Reason(err))
	require.ErrorIs(t, err, ErrOpenAIQuotaResponseBodyTooLarge)
}

func TestOpenAIQuotaServiceRejectsOversizedResetBody(t *testing.T) {
	accountID := int64(9603)
	account := &Account{
		ID:       accountID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "quota-limit-reset",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{accountID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	provider := NewOpenAITokenProvider(repo, tokens, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, int(openAIQuotaMaxResponseBodyBytes)+1))
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(srv))
	_, err := svc.ResetCredit(context.Background(), accountID)
	require.Error(t, err)
	require.Equal(t, "OPENAI_QUOTA_RESPONSE_TOO_LARGE", infraerrors.Reason(err))
	require.ErrorIs(t, err, ErrOpenAIQuotaResponseBodyTooLarge)
}

func TestOpenAIQuotaServiceNilContextAndInvalidIDGuards(t *testing.T) {
	var svc *OpenAIQuotaService
	_, err := svc.QueryUsage(nil, 0)
	require.ErrorIs(t, err, ErrOpenAIQuotaInvalidAccountID)
	_, err = svc.ResetCredit(nil, 0)
	require.ErrorIs(t, err, ErrOpenAIQuotaInvalidAccountID)

	var nilCacheService *OpenAIQuotaService
	err = nilCacheService.CacheResetCreditsSnapshot(nil, 1, &OpenAIRateLimitResetCredits{})
	require.Error(t, err)
	require.Equal(t, "OPENAI_QUOTA_CACHE_WRITE_FAILED", infraerrors.Reason(err))

	// A configured service also normalizes a nil context before touching the
	// repository; the invalid ID guard must win without a panic.
	svc = &OpenAIQuotaService{}
	_, err = svc.QueryUsage(nil, -1)
	require.ErrorIs(t, err, ErrOpenAIQuotaInvalidAccountID)
}

func TestOpenAIQuotaResponseSentinelsRemainDistinct(t *testing.T) {
	require.False(t, errors.Is(ErrOpenAIQuotaResponseBodyTooLarge, ErrOpenAIQuotaResetCreditEntriesTooMany))
}
