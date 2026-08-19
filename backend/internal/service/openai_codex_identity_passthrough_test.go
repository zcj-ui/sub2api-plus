package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func newCompleteOfficialCodexIdentityContext(t *testing.T) (*gin.Context, *Account, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-codex","stream":true,"input":[],"client_metadata":{"x-codex-installation-id":"install-1","session_id":"session-1","thread_id":"thread-1","x-codex-window-id":"thread-1:0","x-codex-parent-thread-id":"parent-1","ws_request_header_x_openai_internal_codex_responses_lite":"true","x-codex-turn-metadata":"{\"installation_id\":\"install-1\",\"session_id\":\"session-1\",\"thread_id\":\"thread-1\",\"window_id\":\"thread-1:0\",\"parent_thread_id\":\"parent-1\",\"request_kind\":\"turn\"}"}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	identityHeaders := map[string]string{
		"User-Agent":                            "cccc/0.146.0 (Ubuntu 24.04; x86_64) xterm-256color (codex-tui; 0.146.0)",
		"Originator":                            "cccc",
		"Version":                               "0.146.0",
		"OpenAI-Beta":                           "responses_websockets=2026-02-06",
		"Accept-Language":                       "en-US",
		"Session-Id":                            "session-1",
		"Thread-Id":                             "thread-1",
		"X-Client-Request-Id":                   "thread-1",
		"X-Codex-Installation-Id":               "install-1",
		"X-Codex-Inference-Call-Id":             "inference-call-1",
		"X-Codex-Parent-Thread-Id":              "parent-1",
		"X-Codex-Routing-Hint":                  "model=client-model;tier=flex",
		"X-Codex-Turn-Metadata":                 `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1"}`,
		"X-Codex-Turn-State":                    "turn-state-1",
		"X-Codex-Window-Id":                     "thread-1:0",
		"X-Oai-Attestation":                     "attestation-1",
		"X-Openai-Internal-Codex-Residency":     "us",
		"X-Openai-Memgen-Request":               "true",
		"X-Responsesapi-Include-Timing-Metrics": "true",
		"X-Openai-Subagent":                     "review",
		"X-Codex-Beta-Features":                 "feature-a",
	}
	for key, value := range identityHeaders {
		c.Request.Header.Set(key, value)
	}
	account := &Account{
		ID:          9001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"},
		Extra:       map[string]any{"openai_oauth_passthrough": true},
	}
	return c, account, body
}

func requireCompleteOfficialCodexIdentity(t *testing.T, headers http.Header) {
	t.Helper()
	for key, want := range map[string]string{
		"User-Agent":                            "cccc/0.146.0 (Ubuntu 24.04; x86_64) xterm-256color (codex-tui; 0.146.0)",
		"Originator":                            "cccc",
		"Version":                               "0.146.0",
		"OpenAI-Beta":                           "responses_websockets=2026-02-06",
		"Accept-Language":                       "en-US",
		"Session-Id":                            "session-1",
		"Thread-Id":                             "thread-1",
		"X-Client-Request-Id":                   "thread-1",
		"X-Codex-Installation-Id":               "install-1",
		"X-Codex-Inference-Call-Id":             "inference-call-1",
		"X-Codex-Parent-Thread-Id":              "parent-1",
		"X-Codex-Routing-Hint":                  "model=client-model;tier=flex",
		"X-Codex-Turn-Metadata":                 `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1"}`,
		"X-Codex-Turn-State":                    "turn-state-1",
		"X-Codex-Window-Id":                     "thread-1:0",
		"X-Oai-Attestation":                     "attestation-1",
		"X-Openai-Internal-Codex-Residency":     "us",
		"X-Openai-Memgen-Request":               "true",
		"X-Responsesapi-Include-Timing-Metrics": "true",
		"X-Openai-Subagent":                     "review",
		"X-Codex-Beta-Features":                 "feature-a",
	} {
		require.Equal(t, want, headers.Get(key), key)
	}
}

func requireCompleteOfficialCodexWSHandshakeIdentity(t *testing.T, headers http.Header) {
	t.Helper()
	for key, want := range map[string]string{
		"User-Agent":                            "cccc/0.146.0 (Ubuntu 24.04; x86_64) xterm-256color (codex-tui; 0.146.0)",
		"Originator":                            "cccc",
		"Version":                               "0.146.0",
		"OpenAI-Beta":                           openAIWSBetaV2Value,
		"Accept-Language":                       "en-US",
		"Session-Id":                            "session-1",
		"Thread-Id":                             "thread-1",
		"X-Client-Request-Id":                   "thread-1",
		"X-Codex-Installation-Id":               "install-1",
		"X-Codex-Inference-Call-Id":             "inference-call-1",
		"X-Codex-Parent-Thread-Id":              "parent-1",
		"X-Codex-Routing-Hint":                  "model=client-model;tier=flex",
		"X-Codex-Turn-Metadata":                 `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1"}`,
		"X-Codex-Window-Id":                     "thread-1:0",
		"X-Oai-Attestation":                     "attestation-1",
		"X-Openai-Internal-Codex-Residency":     "us",
		"X-Openai-Memgen-Request":               "true",
		"X-Responsesapi-Include-Timing-Metrics": "true",
		"X-Openai-Subagent":                     "review",
		"X-Codex-Beta-Features":                 "feature-a",
	} {
		require.Equal(t, want, headers.Get(key), key)
	}
	require.Empty(t, headers.Get(openAIWSTurnStateHeader))
	require.Empty(t, headers.Get(responsesLiteHeaderKey))
}

// Direct request builders apply the gateway's canonical Codex identity. The
// full client snapshot is tested separately at ingress/body projection seams;
// these assertions intentionally avoid treating a builder call in isolation
// as an opt-in passthrough decision.
func requireCanonicalCodexIdentity(t *testing.T, headers http.Header) {
	t.Helper()
	require.Equal(t, codexCLIUserAgent, headers.Get("User-Agent"))
	require.Equal(t, openai.CodexDefaultOriginator, headers.Get("Originator"))
	require.Equal(t, codexCLIVersion, headers.Get("Version"))
}

func requireCanonicalCodexWSHandshakeIdentity(t *testing.T, headers http.Header) {
	t.Helper()
	requireCanonicalCodexIdentity(t, headers)
	require.Equal(t, openAIWSBetaV2Value, headers.Get("OpenAI-Beta"))
}

func TestOfficialCodexIdentityUsesCanonicalAcrossHTTPBuilders(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	ordinary, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", true, "", true)
	require.NoError(t, err)
	requireCanonicalCodexIdentity(t, ordinary.Header)

	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "oauth-token")
	require.NoError(t, err)
	requireCanonicalCodexIdentity(t, passthrough.Header)

	countTokens, err := svc.buildInputTokensUpstreamRequest(context.Background(), c, account, body, "oauth-token")
	require.NoError(t, err)
	// count_tokens is not a Responses turn. The official client sends only
	// transport language/UA hints here; no Codex lifecycle identity is added.
	require.Equal(t, c.Request.Header.Get("User-Agent"), countTokens.Header.Get("User-Agent"))
	require.Empty(t, countTokens.Header.Get("Originator"))
	require.Empty(t, countTokens.Header.Get("Version"))
	require.Empty(t, countTokens.Header.Get("X-Codex-Installation-Id"))
	require.Empty(t, countTokens.Header.Get("Session-Id"))
	require.Empty(t, countTokens.Header.Get("Thread-Id"))
}

func TestOfficialCodexIdentityKeepsStagedFingerprintAccountBoundAcrossBuilders(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	account.Extra["openai_device_id"] = "legacy-server-installation"
	account.Extra[codexFingerprintModeExtraKey] = string(codexFingerprintDevice)
	stageCodexFingerprintIDs(c, &codexFingerprintIDs{
		accountID:      account.ID,
		mode:           codexFingerprintDevice,
		installationID: "legacy-server-installation",
	})

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	ordinary, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", true, "", true)
	require.NoError(t, err)
	requireCanonicalCodexIdentity(t, ordinary.Header)

	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "oauth-token")
	require.NoError(t, err)
	requireCanonicalCodexIdentity(t, passthrough.Header)

	countTokens, err := svc.buildInputTokensUpstreamRequest(context.Background(), c, account, body, "oauth-token")
	require.NoError(t, err)
	// Explicit account convergence does not extend to input_tokens.
	require.Equal(t, c.Request.Header.Get("User-Agent"), countTokens.Header.Get("User-Agent"))
	for _, key := range []string{
		"Originator", "Version", "X-Codex-Installation-Id", "Session-Id",
		"Thread-Id", "X-Client-Request-Id", "X-Codex-Turn-Metadata",
	} {
		require.Empty(t, countTokens.Header.Get(key), key)
	}

	wsHeaders, _, err := svc.buildOpenAIWSHeadersWithBody(
		context.Background(), c, account, "oauth-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "", "", "", "gpt-5.6-codex", "", body,
	)
	require.NoError(t, err)
	requireCanonicalCodexWSHandshakeIdentity(t, wsHeaders)
}

func TestOfficialCodexBodyIdentityDoesNotReconstructHeadersInDirectBuilder(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	for _, key := range []string{
		"X-Codex-Installation-Id",
		"Session-Id",
		"Thread-Id",
		"X-Client-Request-Id",
		"X-Codex-Window-Id",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	c.Request.Header.Set("Accept", "application/json")

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", false, "", true)
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("X-Codex-Installation-Id"))
	require.Empty(t, req.Header.Get("Session-Id"))
	require.Empty(t, req.Header.Get("Thread-Id"))
	require.Empty(t, req.Header.Get("X-Client-Request-Id"))
	require.Empty(t, req.Header.Get("X-Codex-Window-Id"))
	require.Empty(t, req.Header.Get("X-Codex-Parent-Thread-Id"))
	require.Equal(t, "text/event-stream", req.Header.Get("Accept"))
}

func TestOfficialCodexIngressIdentitySurvivesLaterBodyNormalization(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	// Simulate a compatibility normalizer that has already consumed the body
	// metadata projection. The immutable ingress decision remains recorded, but
	// a direct builder still applies its normal canonical identity policy.
	for _, key := range []string{
		"X-Codex-Installation-Id",
		"Session-Id",
		"Session_Id",
		"Thread-Id",
		"Thread_Id",
		"X-Client-Request-Id",
		"X-Codex-Window-Id",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	require.True(t, captureCodexClientIdentityPassthrough(c, account, c.Request.Header, body))
	normalizedBody, err := sjson.DeleteBytes(body, "client_metadata")
	require.NoError(t, err)
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, normalizedBody))

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, normalizedBody, "oauth-token", true, "", true)
	require.NoError(t, err)
	requireCanonicalCodexIdentity(t, req.Header)
	require.Empty(t, req.Header.Get("Session-Id"))
}

func TestOfficialCodexBodyIdentityBoundsTurnMetadataCompatibilityHeader(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Del("X-Codex-Turn-Metadata")
	body, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","tool_namespaces_info":{"mcp":{"functions":["secret-tool"]}},"request_kind":"turn"}`)
	require.NoError(t, err)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", true, "", true)
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("X-Codex-Turn-Metadata"))
}

func TestRestoreCodexIdentityHeadersFromNestedTurnMetadata(t *testing.T) {
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session\",\"thread_id\":\"nested-thread\",\"window_id\":\"nested-window\",\"parent_thread_id\":\"nested-parent\",\"turn_id\":\"nested-turn\",\"request_kind\":\"turn\"}"}}`)
	headers := http.Header{}

	restoreCodexIdentityHeadersFromBody(headers, body, true, true)
	require.Equal(t, "nested-install", headers.Get("X-Codex-Installation-Id"))
	require.Equal(t, "nested-session", headers.Get("Session-Id"))
	require.Equal(t, "nested-thread", headers.Get("Thread-Id"))
	require.Equal(t, "nested-thread", headers.Get("X-Client-Request-Id"))
	require.Equal(t, "nested-window", headers.Get("X-Codex-Window-Id"))
	require.Equal(t, "nested-parent", headers.Get("X-Codex-Parent-Thread-Id"))

	compat := gjson.Parse(headers.Get("X-Codex-Turn-Metadata"))
	require.Equal(t, "nested-turn", compat.Get("turn_id").String())
	require.Equal(t, "nested-session", compat.Get("session_id").String())
}

func TestRestoreCodexIdentityHeadersRejectsContradictoryNestedMetadata(t *testing.T) {
	body := []byte(`{"client_metadata":{"session_id":"flat-session","x-codex-turn-metadata":"{\"session_id\":\"other-session\",\"thread_id\":\"thread-1\",\"installation_id\":\"install-1\"}"}}`)
	headers := http.Header{}

	restoreCodexIdentityHeadersFromBody(headers, body, true, true)
	require.Empty(t, headers.Get("Session-Id"))
	require.Empty(t, headers.Get("Thread-Id"))
	require.Empty(t, headers.Get("X-Codex-Turn-Metadata"))
}

func TestOfficialCodexIdentityUsesCanonicalOnWebSocketHandshake(t *testing.T) {
	c, account, _ := newCompleteOfficialCodexIdentityContext(t)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	headers, _, err := svc.buildOpenAIWSHeaders(
		context.Background(), c, account, "oauth-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "", "", "", "gpt-5.6-codex", "",
	)
	require.NoError(t, err)
	requireCanonicalCodexWSHandshakeIdentity(t, headers)
}

func TestOfficialCodexWSMovesClientTurnStateAndResponsesLiteIntoPayload(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("X-Codex-Turn-State", "client-state")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	headers, _, err := svc.buildOpenAIWSHeadersWithBody(
		context.Background(), c, account, "oauth-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "server-state", "server-metadata", "", "gpt-5.6-codex", "", body,
	)
	require.NoError(t, err)
	require.Empty(t, headers.Get("X-Codex-Turn-State"))
	require.Empty(t, headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Equal(t, `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1"}`, headers.Get("X-Codex-Turn-Metadata"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	payload["type"] = "response.create"
	normalizeOpenAIWSResponseCreatePayload(
		payload,
		openAIWSResponseCreateProtocolOptionsFromHeaders(c.Request.Header, "server-state"),
	)
	metadata := payload["client_metadata"].(map[string]any)
	require.Equal(t, "client-state", metadata[openAIWSTurnStateHeader])
	require.Equal(t, "true", metadata[responsesLiteWSMetadataKey])
}

func TestOfficialCodexWSMapsCachedTurnStateIntoPayload(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Del("X-Codex-Turn-State")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	headers, _, err := svc.buildOpenAIWSHeadersWithBody(
		context.Background(), c, account, "oauth-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "server-state", "server-metadata", "", "gpt-5.6-codex", "", body,
	)
	require.NoError(t, err)
	require.Empty(t, headers.Get("X-Codex-Turn-State"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	payload["type"] = "response.create"
	normalizeOpenAIWSResponseCreatePayload(
		payload,
		openAIWSResponseCreateProtocolOptionsFromHeaders(c.Request.Header, "server-state"),
	)
	metadata := payload["client_metadata"].(map[string]any)
	require.Equal(t, "server-state", metadata[openAIWSTurnStateHeader])
}

func TestOfficialCodexIdentitySeparatesPooledConnections(t *testing.T) {
	reqA := openAIWSAcquireRequest{
		Headers:               http.Header{"X-Codex-Beta-Features": {"feature-a"}},
		IdentityCompatibility: "codex:identity-a",
	}
	reqB := reqA
	reqB.IdentityCompatibility = "codex:identity-b"
	require.NotEqual(t,
		normalizeOpenAIWSHandshakeCompatibilityForRequest(reqA),
		normalizeOpenAIWSHandshakeCompatibilityForRequest(reqB),
	)
	require.False(t, sameOpenAIWSPrewarmTarget(reqA, reqB))
}

func TestCodexWSIdentityCompatibilityKeyUsesStableIdentityFields(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Set("api_key", &APIKey{ID: 505})
	key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.NotEmpty(t, key)
	require.False(t, fresh)

	c.Request.Header.Set("Thread-Id", "thread-2")
	otherKey, otherFresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.NotEqual(t, key, otherKey)
	require.Empty(t, otherKey)
	require.False(t, otherFresh)

	plain := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	plainCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	plainCtx.Request = plain
	plainKey, plainFresh := codexWSIdentityCompatibilityKey(account, plainCtx, plain.Header, body)
	require.Empty(t, plainKey)
	require.True(t, plainFresh)
}

func TestOfficialCodexIdentityCanBeRecoveredFromBodyMetadata(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	for _, key := range []string{
		"X-Codex-Installation-Id",
		"Session-Id",
		"Session_Id",
		"Thread-Id",
		"Thread_Id",
		"X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	require.True(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	// A mixed snapshot must remain on the isolated compatibility path.
	c.Request.Header.Set("Session-Id", "different-session")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))
}

func TestOfficialCodexBodyMetadataDoesNotOptInGenericClient(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("User-Agent", "curl/8.8.0")
	c.Request.Header.Del("Originator")
	for _, key := range []string{
		"X-Codex-Installation-Id",
		"Session-Id",
		"Session_Id",
		"Thread-Id",
		"Thread_Id",
		"X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))
}

func TestOfficialCodexBodyMetadataRequiresUserAgent(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Del("User-Agent")
	for _, key := range []string{
		"X-Codex-Installation-Id",
		"Session-Id",
		"Session_Id",
		"Thread-Id",
		"Thread_Id",
		"X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))
}

func TestOfficialCodexBodyMetadataRequiresCoherentTransportIdentity(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Del("Originator")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("Originator", "codex-tui")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("Version", "0.999.0")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Add("Originator", "different-originator")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))
}

func TestOfficialCodexIdentityRejectsMalformedOrMixedMetadata(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("X-Codex-Turn-Metadata", "not-json")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"installation_id":"install-1","session_id":"session-1","thread_id":"other-thread"}`)
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("Session_Id", "other-session")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("X-Codex-Window-Id", "other-window")
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, body))

	c, account, body = newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1","turn_id":"other-turn"}`)
	updatedBody := bytes.Replace(body, []byte(`\"request_kind\":\"turn\"`), []byte(`\"turn_id\":\"body-turn\",\"request_kind\":\"turn\"`), 1)
	require.False(t, shouldPassThroughCodexClientIdentityWithBody(account, c.Request.Header, updatedBody))
}
