package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesInputTokensCustomRelayUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          159,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "relay-key",
			"base_url": "https://relay.example/v1",
		},
	}
	body := []byte(`{"model":"gpt-5.4","instructions":"Be concise.","input":"hello world","tools":[{"type":"function","name":"lookup","description":"Look up a value","parameters":{"type":"object"}}]}`)

	err := svc.ForwardResponsesInputTokens(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq, "custom relay must not receive /v1/responses/input_tokens")
}

func TestForwardResponsesInputTokensGrokOAuthUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{ID: 160, Platform: PlatformGrok, Type: AccountTypeOAuth}
	body := []byte(`{"model":"grok-4.1","input":"hello world"}`)

	err := svc.ForwardResponsesInputTokens(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq)
}

func TestForwardResponsesInputTokensUpstream404FallsBackLocally(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Invalid URL (POST /v1/responses/input_tokens)"}}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          171,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "official-key",
			"base_url": "https://api.openai.com/v1",
		},
	}
	body := []byte(`{"model":"gpt-5.4","instructions":"Be concise.","input":"hello world"}`)

	err := svc.ForwardResponsesInputTokens(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.NotNil(t, upstream.lastReq)
}

func TestForwardResponsesInputTokensFailsClosedWhenConfiguredProxyIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}
	proxyID := int64(703)
	account := &Account{
		ID:          703,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		ProxyID:     &proxyID,
		Credentials: map[string]any{
			"api_key":  "official-key",
			"base_url": "https://api.openai.com/v1",
		},
	}

	err := svc.ForwardResponsesInputTokens(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"hello world"}`))

	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Equal(t, "upstream_error", gjson.Get(recorder.Body.String(), "error.type").String())
	require.Nil(t, upstream.lastReq, "configured proxy without a relation must not send input_tokens directly")
}
