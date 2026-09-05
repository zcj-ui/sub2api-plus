package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// HTTP POST /v1/responses → forwardOpenAIWSV2 keeps the final outbound tier
// separate from the observed response tier. Usage-time billing decides whether
// the response declaration is authoritative for the selected account.
func TestForwardOpenAIWSV2_KeepsOutboundAndObservedServiceTiersSeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		requestTier string
		stream      bool
	}{
		{name: "priority_nonstream", requestTier: "priority", stream: false},
		{name: "fast_stream", requestTier: "fast", stream: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

			captureConn := &openAIWSCaptureConn{
				events: [][]byte{
					[]byte(`{"type":"response.completed","response":{"id":"resp_tier_v2","model":"gpt-5.5","status":"completed","service_tier":"default","usage":{"input_tokens":1,"output_tokens":1}}}`),
				},
			}
			captureDialer := &openAIWSCaptureDialer{conn: captureConn}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(captureDialer)

			svc := &OpenAIGatewayService{
				cfg:              cfg,
				httpUpstream:     &httpUpstreamRecorder{},
				cache:            &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
				toolCorrector:    NewCodexToolCorrector(),
				openaiWSPool:     pool,
			}
			account := &Account{
				ID:          5882,
				Name:        "openai-ws-v2-tier",
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true},
			}

			body := []byte(fmt.Sprintf(
				`{"model":"gpt-5.5","stream":%t,"service_tier":%q,"input":[{"type":"input_text","text":"hi"}]}`,
				tc.stream, tc.requestTier,
			))
			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode, "must take HTTP POST → forwardOpenAIWSV2, not HTTP fallback")
			require.Equal(t, tc.stream, result.Stream)
			require.Equal(t, "resp_tier_v2", result.RequestID)
			require.NotNil(t, result.ServiceTier)
			require.Equal(t, "priority", *result.ServiceTier)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
			require.Equal(t, "priority", captureConn.lastWrite["service_tier"],
				"outbound WS payload still carries the requested Fast tier")
		})
	}
}

func TestOpenAIWSBufferedStreamEventBudgetClearsOnByteOverflow(t *testing.T) {
	events := make([][]byte, 0, 2)
	var totalBytes int64
	first := []byte("abcd")
	// Four payload bytes plus the `data: ...\\n\\n` framing fit exactly.
	require.NoError(t, appendOpenAIWSBufferedStreamEvent(&events, &totalBytes, first, 12, 4))
	require.Len(t, events, 1)
	require.Equal(t, int64(12), totalBytes)

	err := appendOpenAIWSBufferedStreamEvent(&events, &totalBytes, []byte("x"), 12, 4)
	require.ErrorIs(t, err, errOpenAIPendingSSELinesLimit)
	require.Nil(t, events, "byte overflow must release the queue backing slice")
	require.Zero(t, totalBytes)
}

func TestOpenAIWSBufferedStreamEventBudgetClearsOnCountOverflow(t *testing.T) {
	events := make([][]byte, 0, 2)
	var totalBytes int64
	require.NoError(t, appendOpenAIWSBufferedStreamEvent(&events, &totalBytes, []byte("a"), 1024, 1))
	require.Len(t, events, 1)

	err := appendOpenAIWSBufferedStreamEvent(&events, &totalBytes, []byte("b"), 1024, 1)
	require.ErrorIs(t, err, errOpenAIPendingSSELinesLimit)
	require.Nil(t, events, "event-count overflow must release queued payloads")
	require.Zero(t, totalBytes)
}

func TestOpenAIWSBufferedStreamEventClearReleasesBackingSlice(t *testing.T) {
	events := make([][]byte, 1, 4)
	events[0] = []byte("retained payload")
	var totalBytes int64 = 32

	clearOpenAIWSBufferedStreamEvents(&events, &totalBytes)

	require.Nil(t, events)
	require.Zero(t, totalBytes)
}

func TestOpenAIPreOutputBufferFailoverIsRequestScoped(t *testing.T) {
	account := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	err := newOpenAIPreOutputBufferFailoverError(
		nil,
		account,
		false,
		"request-buffer-limit",
		http.Header{"X-Request-Id": []string{"request-buffer-limit"}},
	)
	require.True(t, err.RequestScopedTransient)
	require.False(t, err.RetryableOnSameAccount)
	require.Equal(t, GatewayFailureScopeRequest, err.Scope)
	require.Equal(t, openAIPreOutputBufferLimitReason, err.Reason)
	require.False(t, err.ShouldReportAccountScheduleFailure())
	require.Equal(t, "request-buffer-limit", err.ResponseHeaders.Get("X-Request-Id"))
}

func TestForwardOpenAIWSV2_PreOutputBufferOverflowIsRequestScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	events := make([][]byte, openAIWSBufferedStreamEventsMaxCount+1)
	for i := range events {
		events[i] = []byte(fmt.Sprintf(
			`{"type":"response.created","response":{"id":"resp_buffer_%d"}}`,
			i,
		))
	}
	captureConn := &openAIWSCaptureConn{events: events}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn})
	defer pool.Close()

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := &Account{
		ID:          5883,
		Name:        "openai-ws-v2-buffer-limit",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}

	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
	require.True(t, failoverErr.RequestScopedTransient)
	require.Equal(t, GatewayFailureScopeRequest, failoverErr.Scope)
	require.Equal(t, openAIPreOutputBufferLimitReason, failoverErr.Reason)
	require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "buffer overflow must not poison the account")
	captureConn.mu.Lock()
	closed := captureConn.closed
	captureConn.mu.Unlock()
	require.True(t, closed, "overflow must evict the offending pooled socket")
}

func TestForwardOpenAIWSV2_MarksCyberPolicyForFailureEventShapes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		upstreamEvent []byte
		wantError     bool
		wantInput     int
		wantOutput    int
	}{
		{
			name:          "error_before_return",
			upstreamEvent: []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"cyber_policy","message":"rate limit exceeded by cyber policy"},"usage":{"input_tokens":5,"output_tokens":1}}`),
			wantError:     true,
			wantInput:     5,
			wantOutput:    1,
		},
		{
			name:          "response_failed_terminal",
			upstreamEvent: []byte(`{"type":"response.failed","response":{"id":"resp_cyber","status":"failed","error":{"code":"cyber_policy","message":"blocked by cyber policy"},"usage":{"input_tokens":9,"output_tokens":2}}}`),
			wantInput:     9,
			wantOutput:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

			cfg := newOpenAIWSV2TestConfig()
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			captureConn := &openAIWSCaptureConn{events: [][]byte{append([]byte(nil), tt.upstreamEvent...)}}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn})
			svc := &OpenAIGatewayService{
				cfg:              cfg,
				httpUpstream:     &httpUpstreamRecorder{},
				cache:            &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
				toolCorrector:    NewCodexToolCorrector(),
				openaiWSPool:     pool,
			}
			account := &Account{
				ID: 5883, Name: "openai-ws-v2-cyber", Platform: PlatformOpenAI,
				Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true},
			}

			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`))
			if tt.wantError {
				require.Error(t, err)
				require.Nil(t, result)
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr))
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
			}

			mark := GetOpsCyberPolicy(c)
			require.NotNil(t, mark)
			require.Equal(t, "cyber_policy", mark.Code)
			require.Equal(t, tt.wantInput, mark.UpstreamInTok)
			require.Equal(t, tt.wantOutput, mark.UpstreamOutTok)
		})
	}
}
