package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSRateLimitSignalTracker_StatuslessThenExplicit(t *testing.T) {
	tracker := &openAIWSRateLimitSignalTracker{}

	signal, apply := tracker.observe(true, false)
	require.True(t, signal)
	require.False(t, apply, "a semantic signal without HTTP 429 must not apply account side effects")

	signal, apply = tracker.observe(true, true)
	require.True(t, signal)
	require.True(t, apply, "a later explicit 429 must still be processed")

	signal, apply = tracker.observe(true, true)
	require.True(t, signal)
	require.False(t, apply, "the same turn must not apply an explicit 429 side effect twice")

	signal, apply = tracker.observe(false, true)
	require.False(t, signal)
	require.False(t, apply)
}

func TestOpenAIWSTerminalRateLimitSideEffectIsNotDoubleCounted(t *testing.T) {
	account := &Account{
		ID:       9961,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}
	repo := &openAIWSRateLimitSignalRepo{}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateSvc}
	payload := []byte(`{"type":"response.failed","status_code":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"too many requests"}}`)

	// This is the side effect already performed by the response.failed event's
	// rate-limit classifier. The first explicit signal must remain observational.
	svc.persistOpenAIWSRateLimitSignal(context.Background(), account, http.Header{}, payload, "rate_limit_exceeded", "rate_limit_error", "too many requests", http.StatusTooManyRequests)
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	require.Empty(t, repo.rateLimitCalls)

	// Terminal bookkeeping must not re-enter the same account handler for that
	// event. If it did, this single 429 would become the second strike.
	require.Equal(t, "response.failed", svc.handleOpenAIWSTerminalTransientFailureAfterRateLimitSignal(
		context.Background(), account, "gpt-5.6", http.Header{}, payload, true,
	))
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	require.Empty(t, repo.rateLimitCalls)

	// A separate explicit event is the second strike and should now confirm the
	// account-level cooldown exactly once.
	svc.persistOpenAIWSRateLimitSignal(context.Background(), account, http.Header{}, payload, "rate_limit_exceeded", "rate_limit_error", "too many requests", http.StatusTooManyRequests)
	require.True(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	require.Len(t, repo.rateLimitCalls, 1)
}

func TestOpenAIWSTerminalStatuslessUsageLimitDoesNotConfirm429(t *testing.T) {
	account := &Account{
		ID:       9962,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}
	repo := &openAIWSRateLimitSignalRepo{}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateSvc}
	payload := []byte(`{"type":"response.failed","error":{"code":"usage_limit_reached","type":"usage_limit_reached","message":"usage limit reached"}}`)

	// The semantic code is still a request failover signal, but there is no
	// authoritative HTTP status. It must not consume a two-strike confirmation.
	require.Equal(t, "response.failed", svc.handleOpenAIWSTerminalTransientFailureAfterRateLimitSignal(
		context.Background(), account, "gpt-5.6", http.Header{}, payload, true,
	))
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	require.Empty(t, repo.rateLimitCalls)
}

func TestForwardOpenAIWSV2ResponseFailed429CountsOnceAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-single-429"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
		[]byte(`{"type":"response.failed","status_code":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"too many requests"}}`),
	}}
	dialer := &openAIWSCaptureDialer{conn: conn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	defer pool.Close()

	account := &Account{
		ID:          9963,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	openAITestAccountWithProxy(account)
	repo := &openAIWSRateLimitSignalRepo{}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
		cache:            &stubGatewayCache{},
		cfg:              cfg,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")

	result, err := svc.forwardOpenAIWSV2(
		context.Background(),
		c,
		account,
		map[string]any{
			"model":  "gpt-5.6",
			"stream": true,
			"input":  []any{map[string]any{"type": "input_text", "text": "hello"}},
		},
		"",
		"access-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true,
		true,
		"gpt-5.6",
		"gpt-5.6",
		time.Now(),
		1,
		"",
		new(bool),
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account), "one explicit response.failed 429 must remain the first strike")
	require.Empty(t, repo.rateLimitCalls, "the duplicate terminal bookkeeping must not create a cooldown")
	require.NotEmpty(t, rec.Body.String(), "partial output and terminal error should be visible to the stream client")
}

func TestIngressResponseFailed429CountsOnceAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	upstreamConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-ingress-single-429"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
		[]byte(`{"type":"response.failed","status_code":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"too many requests"}}`),
	}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: upstreamConn})
	defer pool.Close()

	account := &Account{
		ID:          9964,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_enabled": true},
	}
	openAITestAccountWithProxy(account)
	repo := &openAIWSRateLimitSignalRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{*account}}}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		cfg:              cfg,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}

	serverErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = r.Clone(r.Context())
		ginCtx.Request.Header = r.Header.Clone()
		ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			serverErrCh <- errors.New("unexpected websocket message type")
			return
		}
		serverErrCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "access-token", firstMessage, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6","stream":true}`)))
	cancelWrite()
	for range 3 {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, readErr := clientConn.Read(readCtx)
		cancelRead()
		require.NoError(t, readErr)
	}
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	select {
	case relayErr := <-serverErrCh:
		require.NoError(t, relayErr)
	case <-time.After(5 * time.Second):
		t.Fatal("等待 ingress response.failed 429 结束超时")
	}
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account), "single ingress response.failed 429 must remain first strike")
	require.Empty(t, repo.rateLimitCalls)
}

func TestPassthroughResponseFailed429CountsOnceAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.output_text.delta","delta":"partial"}`)
	upstream.Send(`{"type":"response.failed","status_code":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"too many requests"}}`)

	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc := newPassthroughLifecycleService(cfg, upstream)
	repo := &openAIWSRateLimitSignalRepo{}
	svc.accountRepo = repo
	svc.rateLimitService = &RateLimitService{accountRepo: repo}
	account := passthroughLifecycleAccount()
	account.Type = AccountTypeOAuth
	account.Credentials = map[string]any{"access_token": "access-token"}
	account.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}

	server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, account)
	defer server.Close()
	clientConn := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = clientConn.CloseNow() }()
	for range 2 {
		_, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
		require.NoError(t, err)
	}
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	select {
	case <-serverErr:
	case <-time.After(5 * time.Second):
		t.Fatal("等待 passthrough response.failed 429 结束超时")
	}
	require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account), "single passthrough response.failed 429 must remain first strike")
	require.Empty(t, repo.rateLimitCalls)
}
