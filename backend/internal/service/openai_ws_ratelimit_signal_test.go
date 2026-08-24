package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type openAIWSRateLimitSignalRepo struct {
	stubOpenAIAccountRepo
	rateLimitCalls []time.Time
	updateExtra    []map[string]any
}

type openAICodexSnapshotAsyncRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
}

type openAICodexExtraListRepo struct {
	stubOpenAIAccountRepo
	rateLimitCh chan time.Time
}

func (r *openAIWSRateLimitSignalRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimitCalls = append(r.rateLimitCalls, resetAt)
	return nil
}

func (r *openAIWSRateLimitSignalRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	copied := make(map[string]any, len(updates))
	for k, v := range updates {
		copied[k] = v
	}
	r.updateExtra = append(r.updateExtra, copied)
	return nil
}

func (r *openAICodexSnapshotAsyncRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func (r *openAICodexSnapshotAsyncRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *openAICodexExtraListRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func (r *openAICodexExtraListRepo) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, *pagination.PaginationResult, error) {
	_ = platform
	_ = accountType
	_ = status
	_ = search
	_ = groupID
	_ = privacyMode
	return r.accounts, &pagination.PaginationResult{Total: int64(len(r.accounts)), Page: params.Page, PageSize: params.PageSize}, nil
}

func TestOpenAIGatewayService_Forward_WSv2ErrorEventUsageLimitPersistsRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		var req map[string]any
		if err := conn.ReadJSON(&req); err != nil {
			t.Errorf("read ws request failed: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type":        "error",
			"status_code": http.StatusTooManyRequests,
			"error": map[string]any{
				"code":      "rate_limit_exceeded",
				"type":      "usage_limit_reached",
				"message":   "The usage limit has been reached",
				"resets_at": resetAt,
			},
		})
	}))
	defer wsServer.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_http_should_not_run"}`)),
		},
	}

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	account := Account{
		ID:          501,
		Name:        "openai-ws-rate-limit-event",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": wsServer.URL,
		},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
		},
	}
	openAITestAccountWithProxyForURL(&account, wsServer.URL)
	repo := &openAIWSRateLimitSignalRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: rateSvc,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		cfg:              cfg,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}

	body := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, &account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Nil(t, upstream.lastReq, "WS 限流 error event 不应回退到同账号 HTTP")
	require.Len(t, repo.rateLimitCalls, 1)
	require.WithinDuration(t, time.Unix(resetAt, 0), repo.rateLimitCalls[0], 2*time.Second)
}

func TestOpenAIGatewayService_ForwardWSv2Confirmed429StatusOnlyKeepsLease(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","status_code":429,"error":{"message":"quota reached"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_guard_after_429","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)
	defer pool.Close()

	account := &Account{
		ID:          5031,
		Name:        "openai-codex-429-v2-lease",
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
	openAITestAccountWithProxy(account)
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	// Seed and pin the socket before the account enters the runtime block. The
	// guard only retains an existing healthy connection; a socket opened after
	// the block must take the normal failover path.
	oldConn := newOpenAIWSConn("guard_status_old_conn", account.ID, captureConn, nil)
	oldConn.handshakeCompatibility = normalizeOpenAIWSHandshakeCompatibility(http.Header{
		"x-codex-beta-features": []string{openAIRemoteCompactionV2Feature},
	})
	ap := pool.getOrCreateAccountPool(account.ID)
	ap.mu.Lock()
	ap.conns[oldConn.id] = oldConn
	ap.mu.Unlock()
	require.True(t, pool.PinGuardConn(account.ID, oldConn.id))
	stateStore := svc.getOpenAIWSStateStore()
	stateStore.BindResponseConn("resp_guard_status_seed", oldConn.id, time.Hour)
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAI429GuardRuntimeBlocked(account))

	forward := func() (*OpenAIForwardResult, *httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")
		requestBody := map[string]any{
			"model":                "gpt-5.1",
			"stream":               false,
			"previous_response_id": "resp_guard_status_seed",
			"input":                []any{map[string]any{"type": "input_text", "text": "hello"}},
		}
		agentTaskRecoveryTried := false
		result, err := svc.forwardOpenAIWSV2(
			context.Background(),
			c,
			account,
			requestBody,
			"access-token",
			OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			true,
			false,
			"gpt-5.1",
			"gpt-5.1",
			time.Now(),
			1,
			"",
			&agentTaskRecoveryTried,
		)
		return result, rec, err
	}

	firstResult, firstRec, firstErr := forward()
	require.Error(t, firstErr)
	require.Nil(t, firstResult)
	require.Equal(t, http.StatusTooManyRequests, firstRec.Code)

	secondResult, _, secondErr := forward()
	require.NoError(t, secondErr)
	require.NotNil(t, secondResult)
	require.Equal(t, "resp_guard_after_429", secondResult.RequestID)
	require.Equal(t, 0, captureDialer.DialCount(), "confirmed 429 must reuse the pre-existing guarded websocket")
	require.Len(t, captureConn.writes, 2)
}

func TestOpenAIGatewayService_ForwardWSv2GuardAcquireQueueFullKeepsBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 1

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_guard_queue_seed","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)
	defer pool.Close()

	account := &Account{
		ID:          5041,
		Name:        "openai-codex-429-queue-full",
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
	openAITestAccountWithProxy(account)
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	invoke := func(body map[string]any) (*OpenAIForwardResult, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.98.0")
		recoveryTried := false
		return svc.forwardOpenAIWSV2(
			context.Background(),
			c,
			account,
			body,
			"access-token",
			OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			true,
			false,
			"gpt-5.1",
			"gpt-5.1",
			time.Now(),
			1,
			"",
			&recoveryTried,
		)
	}

	seed, err := invoke(map[string]any{
		"model":  "gpt-5.1",
		"stream": false,
		"input":  []any{map[string]any{"type": "input_text", "text": "seed"}},
	})
	require.NoError(t, err)
	require.NotNil(t, seed)
	store := svc.getOpenAIWSStateStore()
	connID, ok := store.GetResponseConn(seed.RequestID)
	require.True(t, ok)
	require.NotEmpty(t, connID)

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	snapshot := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, snapshot.Active)
	require.True(t, pool.MarkGuardConnConfirmed(account.ID, connID, snapshot.Generation))
	require.True(t, svc.pinOpenAI429GuardConnection(account, connID))

	ap, ok := pool.getAccountPool(account.ID)
	require.True(t, ok)
	ap.mu.Lock()
	guardConn := ap.conns[connID]
	ap.mu.Unlock()
	require.NotNil(t, guardConn)
	require.True(t, guardConn.tryAcquire(), "hold the guarded socket so Acquire returns queue-full")
	guardConn.waiters.Store(1)
	defer func() {
		guardConn.waiters.Store(0)
		guardConn.release()
	}()

	result, err := invoke(map[string]any{
		"model":                "gpt-5.1",
		"stream":               false,
		"previous_response_id": seed.RequestID,
		"input":                []any{map[string]any{"type": "input_text", "text": "next"}},
	})
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "a busy guard socket is not a connection failure")

	boundAccountID, getErr := store.GetResponseAccount(context.Background(), 0, seed.RequestID)
	require.NoError(t, getErr)
	require.Equal(t, account.ID, boundAccountID)
	boundConnID, stillBound := store.GetResponseConn(seed.RequestID)
	require.True(t, stillBound)
	require.Equal(t, connID, boundConnID)
	require.True(t, pool.IsGuardConnPinned(account.ID, connID), "queue-full must not release a healthy guard pin")
}

func TestOpenAIGatewayService_Forward_WSv2Handshake429PersistsRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "100")
		w.Header().Set("x-codex-primary-reset-after-seconds", "7200")
		w.Header().Set("x-codex-primary-window-minutes", "10080")
		w.Header().Set("x-codex-secondary-used-percent", "3")
		w.Header().Set("x-codex-secondary-reset-after-seconds", "1800")
		w.Header().Set("x-codex-secondary-window-minutes", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited"}}`))
	}))
	defer server.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_http_should_not_run"}`)),
		},
	}

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	account := Account{
		ID:          502,
		Name:        "openai-ws-rate-limit-handshake",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
		},
	}
	openAITestAccountWithProxyForURL(&account, server.URL)
	repo := &openAIWSRateLimitSignalRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: rateSvc,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		cfg:              cfg,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}

	body := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, &account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Nil(t, upstream.lastReq, "WS 握手 429 不应回退到同账号 HTTP")
	require.Len(t, repo.rateLimitCalls, 1)
	require.NotEmpty(t, repo.updateExtra, "握手 429 的 x-codex 头应立即落库")
	require.Contains(t, repo.updateExtra[0], "codex_usage_updated_at")
}

func TestOpenAIGatewayService_Forward_WSv2Handshake502RecordsModelTransient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req-ws-502")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"bad gateway"}}`))
	}))
	defer server.Close()

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	account := Account{
		ID:          504,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": server.URL},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		rateLimitService: NewRateLimitService(transientCooldownAccountRepo{}, nil, cfg, nil, nil),
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)

	for range 2 {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
		result, err := svc.Forward(context.Background(), c, &account, body)
		require.Error(t, err)
		require.Nil(t, result)
	}

	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(&account, "gpt-5.5"))
}

func TestOpenAIGatewayService_ProxyResponsesWebSocketFromClient_ErrorEventUsageLimitPersistsRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	resetAt := time.Now().Add(90 * time.Minute).Unix()
	captureConn := &openAIWSCaptureConn{
		events: [][]byte{
			[]byte(`{"type":"error","status_code":429,"error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":PLACEHOLDER}}`),
		},
	}
	captureConn.events[0] = []byte(strings.ReplaceAll(string(captureConn.events[0]), "PLACEHOLDER", strconv.FormatInt(resetAt, 10)))
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	account := Account{
		ID:          503,
		Name:        "openai-ingress-rate-limit",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
		},
	}
	repo := &openAIWSRateLimitSignalRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: rateSvc,
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
		req := r.Clone(r.Context())
		req.Header = req.Header.Clone()
		req.Header.Set("User-Agent", "unit-test-agent/1.0")
		ginCtx.Request = req

		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			serverErrCh <- io.ErrUnexpectedEOF
			return
		}

		serverErrCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, openAITestAccountWithProxy(&account), "sk-test", firstMessage, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)

	select {
	case serverErr := <-serverErrCh:
		require.Error(t, serverErr)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, serverErr, &failoverErr)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.Len(t, repo.rateLimitCalls, 1)
		require.WithinDuration(t, time.Unix(resetAt, 0), repo.rateLimitCalls[0], 2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("等待 ingress websocket 结束超时")
	}
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_ExhaustedSnapshotDoesNotSetRateLimit(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	// 独立零间隔节流器：避免 -count>1 时落入包级 30s 默认节流窗口而偶发失败。
	svc := &OpenAIGatewayService{
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(0),
	}
	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         ptrFloat64WS(100),
		PrimaryResetAfterSeconds:   ptrIntWS(3600),
		PrimaryWindowMinutes:       ptrIntWS(10080),
		SecondaryUsedPercent:       ptrFloat64WS(12),
		SecondaryResetAfterSeconds: ptrIntWS(1200),
		SecondaryWindowMinutes:     ptrIntWS(300),
	}
	svc.updateCodexUsageSnapshot(context.Background(), 601, snapshot)

	select {
	case updates := <-repo.updateExtraCh:
		require.Equal(t, 100.0, updates["codex_7d_used_percent"])
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 快照落库超时")
	}

	select {
	case resetAt := <-repo.rateLimitCh:
		t.Fatalf("不应因仅写入快照而生成运行时限流时间: %v", resetAt)
	case <-time.After(2 * time.Second):
	}
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_NonExhaustedSnapshotDoesNotSetRateLimit(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	// 独立零间隔节流器：避免 -count>1 时落入包级 30s 默认节流窗口而偶发失败。
	svc := &OpenAIGatewayService{
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(0),
	}
	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         ptrFloat64WS(94),
		PrimaryResetAfterSeconds:   ptrIntWS(3600),
		PrimaryWindowMinutes:       ptrIntWS(10080),
		SecondaryUsedPercent:       ptrFloat64WS(22),
		SecondaryResetAfterSeconds: ptrIntWS(1200),
		SecondaryWindowMinutes:     ptrIntWS(300),
	}
	svc.updateCodexUsageSnapshot(context.Background(), 602, snapshot)

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 快照落库超时")
	}

	select {
	case resetAt := <-repo.rateLimitCh:
		t.Fatalf("不应写入运行时限流时间: %v", resetAt)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_ThrottlesExtraWrites(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		updateExtraCh: make(chan map[string]any, 2),
	}
	svc := &OpenAIGatewayService{
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Hour),
	}
	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         ptrFloat64WS(94),
		PrimaryResetAfterSeconds:   ptrIntWS(3600),
		PrimaryWindowMinutes:       ptrIntWS(10080),
		SecondaryUsedPercent:       ptrFloat64WS(22),
		SecondaryResetAfterSeconds: ptrIntWS(1200),
		SecondaryWindowMinutes:     ptrIntWS(300),
	}

	svc.updateCodexUsageSnapshot(context.Background(), 777, snapshot)
	svc.updateCodexUsageSnapshot(context.Background(), 777, snapshot)

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("等待第一次 codex 快照落库超时")
	}

	select {
	case updates := <-repo.updateExtraCh:
		t.Fatalf("unexpected second codex snapshot write: %v", updates)
	case <-time.After(200 * time.Millisecond):
	}
}

func ptrFloat64WS(v float64) *float64 { return &v }
func ptrIntWS(v int) *int             { return &v }

func TestOpenAIGatewayService_GetSchedulableAccount_ExhaustedCodexExtraDoesNotSetRateLimit(t *testing.T) {
	resetAt := time.Now().Add(6 * 24 * time.Hour)
	account := Account{
		ID:          701,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.UTC().Format(time.RFC3339),
		},
	}
	repo := &openAICodexExtraListRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, rateLimitCh: make(chan time.Time, 1)}
	svc := &OpenAIGatewayService{accountRepo: repo}

	fresh, err := svc.getSchedulableAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	require.Nil(t, fresh.RateLimitResetAt)
	select {
	case persisted := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 提升为运行时限流状态: %v", persisted)
	case <-time.After(2 * time.Second):
	}
}

func TestAdminService_ListAccounts_ExhaustedCodexExtraDoesNotSetRateLimit(t *testing.T) {
	resetAt := time.Now().Add(4 * 24 * time.Hour)
	repo := &openAICodexExtraListRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID:          702,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Extra: map[string]any{
				"codex_7d_used_percent": 100.0,
				"codex_7d_reset_at":     resetAt.UTC().Format(time.RFC3339),
			},
		}}},
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &adminServiceImpl{accountRepo: repo}

	accounts, total, err := svc.ListAccounts(context.Background(), 1, 20, PlatformOpenAI, AccountTypeOAuth, "", "", 0, "", "", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, accounts, 1)
	require.Nil(t, accounts[0].RateLimitResetAt)
	select {
	case persisted := <-repo.rateLimitCh:
		t.Fatalf("不应在账号列表查询时将 codex extra 持久化为运行时限流状态: %v", persisted)
	case <-time.After(2 * time.Second):
	}
}

func TestOpenAIWSErrorHTTPStatusFromRaw_UsageLimitReachedIs429(t *testing.T) {
	require.Equal(t, http.StatusTooManyRequests, openAIWSErrorHTTPStatusFromRaw("", "usage_limit_reached"))
	require.Equal(t, http.StatusTooManyRequests, openAIWSErrorHTTPStatusFromRaw("rate_limit_exceeded", ""))
}

	func TestIsOpenAIWSRateLimitErrorRecognizesExplicit429TransportText(t *testing.T) {
		message := "exceeded retry limit, last status: 429 Too Many Requests"
		require.True(t, isOpenAIWSRateLimitError("", "", message))
		require.True(t, isOpenAIWSRateLimitError("", "", "429 Too Many Requests"))
		require.False(t, isOpenAIWSRateLimitError("", "", "retry attempt 429 exhausted"))
		require.Equal(t, http.StatusTooManyRequests, openAIWSErrorHTTPStatusFromRawWithMessage("", "", message))
	}

	func TestIsOpenAIWSRateLimitSignalPrefersExplicitStatus(t *testing.T) {
		require.True(t, isOpenAIWSRateLimitSignal(http.StatusTooManyRequests, "", "", ""))
		require.True(t, isOpenAIWSRateLimitSignal(0, "rate_limit_exceeded", "", ""))
		require.False(t, isOpenAIWSRateLimitSignal(http.StatusBadGateway, "rate_limit_exceeded", "", ""))
		require.False(t, isOpenAIWSRateLimitSignal(http.StatusServiceUnavailable, "", "usage_limit_reached", ""))
	}

	func TestOpenAIWSPayloadUpstreamStatusIncludesTopLevelFields(t *testing.T) {
		require.Equal(t, http.StatusTooManyRequests, openAIWSPayloadUpstreamStatus([]byte(`{"type":"error","status_code":429}`)))
		require.Equal(t, http.StatusTooManyRequests, openAIWSPayloadUpstreamStatus([]byte(`{"type":"error","status":429}`)))
		require.Equal(t, http.StatusBadGateway, openAIWSPayloadUpstreamStatus([]byte(`{"type":"error","error":{"status_code":502}}`)))
	}

	func TestPersistOpenAIWSRateLimitSignalWithoutRateLimitServiceConfirmsOAuth429(t *testing.T) {
		account := &Account{ID: 9911, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		svc := &OpenAIGatewayService{}

		for range 2 {
			svc.persistOpenAIWSRateLimitSignal(context.Background(), account, nil, []byte(`{"status":429}`), "rate_limit_exceeded", "", "quota reached")
		}

		require.True(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	}

	func TestPersistOpenAIWSRateLimitSignalIgnoresSemanticCodeWithoutExplicit429(t *testing.T) {
		account := &Account{ID: 9912, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		svc := &OpenAIGatewayService{}

		for range 3 {
			svc.persistOpenAIWSRateLimitSignal(context.Background(), account, nil, nil, "rate_limit_exceeded", "rate_limit_error", "quota reached")
		}

		require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))
	}

	func TestIsOpenAIWSExplicit429SignalRequiresStatusEvidence(t *testing.T) {
		require.True(t, isOpenAIWSExplicit429Signal(http.StatusTooManyRequests, "usage_limit_reached", "", "", nil))
		require.True(t, isOpenAIWSExplicit429Signal(0, "", "", "last status: 429 Too Many Requests", nil))
		require.True(t, isOpenAIWSExplicit429Signal(0, "rate_limit_exceeded", "", "", []byte(`{"error":{"status":429}}`)))
		require.False(t, isOpenAIWSExplicit429Signal(0, "usage_limit_reached", "", "quota reached", nil))
		require.False(t, isOpenAIWSExplicit429Signal(0, "rate_limit_exceeded", "", "retry attempt 429 exhausted", nil))
	}

	func TestOpenAIWSRateLimitFailoverError_OAuthKeepsSameAccountDeadline(t *testing.T) {
		svc := &OpenAIGatewayService{}
		headers := http.Header{"Retry-After": []string{"30"}}
		body := []byte(`{"error":{"type":"rate_limit_error","message":"limited"}}`)

		oauthErr := svc.newOpenAIWSRateLimitFailoverError(&Account{
			ID:       904,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
		}, headers, body, "limited")
		require.True(t, oauthErr.RetryableOnSameAccount)
		require.False(t, oauthErr.SameAccountRetryDeadline.IsZero())
		require.Positive(t, oauthErr.SameAccountRetryDelay)
		require.LessOrEqual(t, oauthErr.SameAccountRetryDelay, openAIOAuth429MaxRetryDelay)
		require.Equal(t, body, oauthErr.ResponseBody)
		require.Equal(t, "30", oauthErr.ResponseHeaders.Get("Retry-After"))

		apiKeyErr := svc.newOpenAIWSRateLimitFailoverError(&Account{
			ID:       905,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
		}, headers, body, "limited")
		require.False(t, apiKeyErr.RetryableOnSameAccount)
		require.True(t, apiKeyErr.SameAccountRetryDeadline.IsZero())
		require.Zero(t, apiKeyErr.SameAccountRetryDelay)
	}
