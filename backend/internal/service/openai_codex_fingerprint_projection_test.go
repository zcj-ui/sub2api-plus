package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// newCodexFingerprintProjectionAccount keeps these tests on the same path as
// a deployed OAuth passthrough account. A persisted seed/device ID is
// intentional: convergence is opt-in and snapshots must have an owner.
func newCodexFingerprintProjectionAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "codex-projection-test",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-test-token",
			"chatgpt_account_id": "chatgpt-test-account",
		},
		Extra: map[string]any{
			"openai_passthrough":                        true,
			codexFingerprintModeExtraKey:                string(codexFingerprintDevice),
			codexFingerprintSeedExtraKey:                "11111111-1111-4111-8111-111111111111",
			"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff,
		},
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
	}
}

func newCodexFingerprintProjectionService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		}},
		httpUpstream: upstream,
	}
}

func newCodexFingerprintProjectionContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "codex-tui/0.146.0 (Mac OS X 14.0; arm64) iTerm")
	c.Request.Header.Set("session-id", "client-session-1")
	c.Request.Header.Set("thread-id", "client-thread-1")
	c.Request.Header.Set("x-client-request-id", "client-thread-1")
	c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"client-install","session_id":"client-session-1","thread_id":"client-thread-1","turn_id":"019956a2-a4a1-7000-8000-000000000001","window_id":"client-thread-1:0","sandbox":"seatbelt"}`)
	return c, rec
}

func TestOpenAICodexFingerprintProjection_OrdinaryRequestPreservesClientMetadata(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"test","input":[{"type":"text","text":"hello"}],"prompt_cache_key":"client-cache"}`)
	c, rec := newCodexFingerprintProjectionContext("/v1/responses", body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-projection"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-projection\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")),
	}}
	svc := newCodexFingerprintProjectionService(upstream)
	account := newCodexFingerprintProjectionAccount(7101)

	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)

	// Device mode owns the installation projection while preserving the
	// client's canonical session/thread lifecycle.
	require.Equal(t, resolveConvergedInstallationID(account), upstream.lastReq.Header.Get("x-codex-installation-id"))
	// A complete official snapshot owns its request lifecycle, including the
	// per-turn request id; it must be forwarded unchanged.
	require.Equal(t, "client-thread-1", upstream.lastReq.Header.Get("x-client-request-id"))
	require.Equal(t, "client-session-1", upstream.lastReq.Header.Get("session-id"))
	require.Equal(t, "client-session-1", upstream.lastReq.Header.Get("session_id"))
	require.Equal(t, resolveConvergedInstallationID(account), gjson.GetBytes(upstream.lastBody, "client_metadata.x-codex-installation-id").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "client_metadata").IsObject())
	require.Equal(t, "client-cache", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Contains(t, rec.Body.String(), "resp-projection")
}

func TestOpenAICodexFingerprintProjection_CompactPreservesClientHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","stream":true,"store":true,"instructions":"test","input":[{"type":"text","text":"compact"}],"prompt_cache_key":"compact-cache"}`)
	c, rec := newCodexFingerprintProjectionContext("/v1/responses/compact", body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-compact-projection"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"compact-projection","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := newCodexFingerprintProjectionService(upstream)
	account := newCodexFingerprintProjectionAccount(7102)

	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)

	// Compact is a unary compatibility endpoint. The official client does not
	// carry the normal Responses fingerprint lifecycle on this path, so the
	// staged convergence headers must not be synthesized.
	require.Empty(t, upstream.lastReq.Header.Get("x-codex-installation-id"))
	require.NotEqual(t, "client-thread-1", upstream.lastReq.Header.Get("x-client-request-id"))
	require.NotEqual(t, "client-session-1", upstream.lastReq.Header.Get("session-id"))
	require.NotEqual(t, "client-session-1", upstream.lastReq.Header.Get("session_id"))
	require.NotEqual(t, "client-thread-1", upstream.lastReq.Header.Get("thread-id"))
	require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
	require.Equal(t, "compact-cache", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Exists())
	require.Contains(t, rec.Body.String(), "compact-projection")
}

func TestOpenAICodexFingerprintProjection_CompactSkipsSessionAndFullModes(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","stream":true,"instructions":"test","input":[{"type":"text","text":"compact"}],"prompt_cache_key":"compact-cache"}`)
			c, _ := newCodexFingerprintProjectionContext("/v1/responses/compact", body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"compact-projection","status":"completed"}`)),
			}}
			svc := newCodexFingerprintProjectionService(upstream)
			account := newCodexFingerprintProjectionAccount(7103)
			account.Extra[codexFingerprintModeExtraKey] = string(mode)

			_, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())
			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Empty(t, upstream.lastReq.Header.Get("x-codex-installation-id"))
			require.Empty(t, upstream.lastReq.Header.Get("x-codex-window-id"))
			require.NotEqual(t, "client-thread-1", upstream.lastReq.Header.Get("x-client-request-id"))
			require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
		})
	}
}
