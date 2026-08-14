package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newAntigravityUpstreamAccount() *Account {
	return &Account{
		ID:          8261,
		Name:        "new-api-relay",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeUpstream,
		Status:      StatusActive,
		Concurrency: 2,
		Credentials: map[string]any{
			"base_url": "https://relay.example/prefix/v1",
			"api_key":  "upstream-test-key",
			"model_mapping": map[string]any{
				"public-claude": "claude-sonnet-4-6",
			},
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides: map[string]any{
				"x-app":      "claude-code",
				"user-agent": "ClaudeCode/2.1.0",
			},
		},
	}
}

func upstreamAnthropicSSE() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"new-api-request"}},
		Body: io.NopCloser(bytes.NewBufferString("event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_upstream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":3}}}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n")),
	}
}

func TestAntigravityUpstreamMessagesUsesNewAPICompatibleRelayRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newAntigravityUpstreamAccount()
	require.Equal(t, map[string]string{"x-app": "claude-code", "user-agent": "ClaudeCode/2.1.0"}, account.GetHeaderOverrides())
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"new-api-json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"model":"claude-sonnet-4-6","usage":{"input_tokens":3,"output_tokens":2}}`)),
	}}}
	var requestURL string
	var requestHeaders http.Header
	upstream.onCall = func(req *http.Request, _ *queuedHTTPUpstreamStub) {
		requestURL = req.URL.String()
		requestHeaders = req.Header.Clone()
	}
	svc := &AntigravityGatewayService{httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"public-claude","stream":true,"messages":[{"role":"user","content":"ok"}]}`))
	c.Request.Header.Set("anthropic-version", "2023-06-01")

	result, err := svc.ForwardUpstream(context.Background(), c, account, []byte(`{"model":"public-claude","stream":true,"messages":[{"role":"user","content":"ok"}]}`))
	require.NoError(t, err)
	require.Equal(t, "https://relay.example/prefix/v1/messages", requestURL)
	require.Equal(t, "Bearer upstream-test-key", requestHeaders.Get("Authorization"))
	require.Equal(t, "upstream-test-key", requestHeaders.Get("x-api-key"))
	require.Equal(t, "claude-code", getHeaderRaw(requestHeaders, "x-app"))
	require.Equal(t, "ClaudeCode/2.1.0", getHeaderRaw(requestHeaders, "user-agent"))
	require.Equal(t, "claude-sonnet-4-6", gjson.GetBytes(upstream.requestBodies[0], "model").String())
	require.Equal(t, "public-claude", result.Model)
	require.Equal(t, "claude-sonnet-4-6", result.UpstreamModel)
	require.Equal(t, "claude-sonnet-4-6", result.UpstreamResponseModel)
	require.False(t, result.Stream, "upstream Content-Type determines result streaming, not the request flag")
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestAntigravityUpstreamMessagesReturnsFailoverErrorWithoutWriting429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"new-api-429"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"upstream busy"}}`)),
	}}}
	svc := &AntigravityGatewayService{httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.ForwardUpstream(context.Background(), c, newAntigravityUpstreamAccount(), []byte(`{"model":"public-claude","messages":[{"role":"user","content":"ok"}]}`))
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.Equal(t, "new-api-429", failoverErr.ResponseHeaders.Get("X-Request-Id"))
	require.Empty(t, recorder.Body.String())
}

func TestAntigravityUpstreamChatAndResponsesUseGenericAnthropicRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		body []byte
		call func(*GatewayService, *gin.Context, *Account, []byte) (*ForwardResult, error)
	}{
		{
			name: "chat completions",
			body: []byte(`{"model":"public-claude","messages":[{"role":"user","content":"ok"}]}`),
			call: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			},
		},
		{
			name: "responses",
			body: []byte(`{"model":"public-claude","input":"ok"}`),
			call: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsResponses(context.Background(), c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{upstreamAnthropicSSE()}}
			var requestURL string
			var headers http.Header
			upstream.onCall = func(req *http.Request, _ *queuedHTTPUpstreamStub) {
				requestURL = req.URL.String()
				headers = req.Header.Clone()
			}
			svc := &GatewayService{
				cfg:          &config.Config{},
				httpUpstream: upstream,
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tt.body))

			result, err := tt.call(svc, c, newAntigravityUpstreamAccount(), tt.body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "https://relay.example/prefix/v1/messages", requestURL)
			require.Equal(t, "Bearer upstream-test-key", getHeaderRaw(headers, "authorization"))
			require.Equal(t, "upstream-test-key", getHeaderRaw(headers, "x-api-key"))
			require.Equal(t, "claude-sonnet-4-6", gjson.GetBytes(upstream.requestBodies[0], "model").String())
			require.Equal(t, "claude-sonnet-4-6", result.UpstreamModel)
			require.Contains(t, recorder.Body.String(), "ok")
		})
	}
}
