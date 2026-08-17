package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIGatewayService_Forward_WSv2_Guard429ErrorRetainsHealthySocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = false
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_guard_error_seed","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
		[]byte(`{"type":"error","error":{"status_code":429,"code":"rate_limit_exceeded","message":"quota reached"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_guard_error_next","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
		[]byte(`{"type":"error","error":{"status_code":503,"code":"upstream_error","message":"relay unhealthy"}}`),
	}}
	p := newOpenAIWSConnPool(cfg)
	dialer := &openAIWSCaptureDialer{conn: captureConn}
	p.setClientDialerForTest(dialer)
	defer p.Close()
	account := &Account{
		ID:          1310,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey:             true,
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     p,
	}
	// The error frame below is the second explicit signal. The first signal is
	// observational and must not activate the account guard by itself.
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, time.Now()))

	forward := func(input, previousResponseID string) (*OpenAIForwardResult, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")
		body := fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"input":"%s"}`, input)
		if previousResponseID != "" {
			body = fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"previous_response_id":"%s","input":"%s"}`, previousResponseID, input)
		}
		return svc.Forward(context.Background(), c, account, []byte(body))
	}

	seed, err := forward("seed", "")
	require.NoError(t, err)
	require.NotNil(t, seed)
	require.Equal(t, 1, dialer.DialCount())

	failed, err := forward("guarded-429", "resp_guard_error_seed")
	require.Error(t, err)
	require.Nil(t, failed)
	require.True(t, svc.isOpenAI429GuardRuntimeBlocked(account))

	ap, ok := p.getAccountPool(account.ID)
	require.True(t, ok)
	ap.mu.Lock()
	connIDs := make([]string, 0, len(ap.conns))
	for id := range ap.conns {
		connIDs = append(connIDs, id)
	}
	ap.mu.Unlock()
	require.Len(t, connIDs, 1)
	require.True(t, p.IsGuardConnPinned(account.ID, connIDs[0]), "a rate-limit error event must retain the healthy old socket")
	// The bare error has no new response id. The previous response tuple must be
	// promoted to a permanent local binding before its ordinary sticky TTL can
	// expire, otherwise the pinned socket becomes an unreachable reservation.
	stateStore := svc.getOpenAIWSStateStore()
	boundAccount, bindErr := stateStore.GetResponseAccount(context.Background(), 0, "resp_guard_error_seed")
	require.NoError(t, bindErr)
	require.Equal(t, account.ID, boundAccount)
	boundConn, bound := stateStore.GetResponseConn("resp_guard_error_seed")
	require.True(t, bound)
	require.Equal(t, connIDs[0], boundConn)

	// A permanent pin outlives the short scheduling cooldown. This exercises a
	// subsequent ordinary turn after expiry before making the old socket fail.
	svc.openaiAccountRuntimeBlockUntil.Store(account.ID, time.Now().Add(-time.Second))
	svc.openaiAccountRuntimeBlockReason.Store(account.ID, "429")
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	require.True(t, p.IsGuardConnPinned(account.ID, connIDs[0]))

	next, err := forward("after-guard", "resp_guard_error_seed")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, 1, dialer.DialCount(), "retained 429 socket should serve the next turn without redial")

	broken, err := forward("broken-after-expiry", "resp_guard_error_next")
	require.Error(t, err)
	require.Nil(t, broken)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, 1, dialer.DialCount(), "a broken guard socket must not redial the same account")

	ap.mu.Lock()
	_, stillPooled := ap.conns[connIDs[0]]
	ap.mu.Unlock()
	require.False(t, stillPooled, "a non-429 error must immediately evict the retained guard socket")
}

func TestOpenAIGatewayService_Forward_WSv2_Guard429ResponseFailedWithoutResponseRetainsHealthySocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = false
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_failed_seed","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
		// Some relays omit the nested response object on a non-streaming
		// response.failed frame. The frame still represents the confirming 429.
		[]byte(`{"type":"response.failed","status_code":429,"error":{"code":"rate_limit_exceeded","message":"quota reached"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_failed_next","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	p := newOpenAIWSConnPool(cfg)
	dialer := &openAIWSCaptureDialer{conn: captureConn}
	p.setClientDialerForTest(dialer)
	defer p.Close()
	account := &Account{
		ID:          1311,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey:             true,
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     p,
	}
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, time.Now()))

	forward := func(input, previousResponseID string) (*OpenAIForwardResult, *httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")
		body := fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"input":"%s"}`, input)
		if previousResponseID != "" {
			body = fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"previous_response_id":"%s","input":"%s"}`, previousResponseID, input)
		}
		result, err := svc.Forward(context.Background(), c, account, []byte(body))
		return result, rec, err
	}

	seed, seedRec, err := forward("seed", "")
	require.NoError(t, err)
	require.NotNil(t, seed)
	require.Equal(t, http.StatusOK, seedRec.Code)
	require.Equal(t, 1, dialer.DialCount())

	failed, failedRec, err := forward("guarded-429", "resp_failed_seed")
	require.Error(t, err)
	require.Nil(t, failed)
	require.Equal(t, http.StatusTooManyRequests, failedRec.Code)
	require.True(t, svc.isOpenAI429GuardRuntimeBlocked(account))

	ap, ok := p.getAccountPool(account.ID)
	require.True(t, ok)
	ap.mu.Lock()
	connIDs := make([]string, 0, len(ap.conns))
	for id := range ap.conns {
		connIDs = append(connIDs, id)
	}
	ap.mu.Unlock()
	require.Len(t, connIDs, 1)
	require.True(t, p.IsGuardConnPinned(account.ID, connIDs[0]))
	stateStore := svc.getOpenAIWSStateStore()
	boundAccount, bindErr := stateStore.GetResponseAccount(context.Background(), 0, "resp_failed_seed")
	require.NoError(t, bindErr)
	require.Equal(t, account.ID, boundAccount)
	boundConn, bound := stateStore.GetResponseConn("resp_failed_seed")
	require.True(t, bound)
	require.Equal(t, connIDs[0], boundConn)

	next, _, err := forward("after-guard", "resp_failed_seed")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "resp_failed_next", next.RequestID)
	require.Equal(t, 1, dialer.DialCount(), "bare response.failed 429 must retain the healthy socket")
}
