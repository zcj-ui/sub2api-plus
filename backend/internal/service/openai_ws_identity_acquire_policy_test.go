package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSIdentityAcquirePolicy(t *testing.T) {
	tests := []struct {
		name               string
		identityFresh      bool
		preferred          string
		guardPreferred     bool
		dedicated          bool
		wantForceNew       bool
		wantForcePreferred bool
	}{
		{
			name:          "identity-less unbound request gets a fresh socket",
			identityFresh: true,
			wantForceNew:  true,
		},
		{
			name:               "identity-less continuation stays on preferred socket",
			identityFresh:      true,
			preferred:          "conn-1",
			wantForcePreferred: true,
		},
		{
			name:               "guard continuation keeps preferred socket",
			preferred:          "conn-1",
			guardPreferred:     true,
			wantForcePreferred: true,
		},
		{
			name:         "dedicated mode always dials fresh",
			preferred:    "conn-1",
			dedicated:    true,
			wantForceNew: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNew, gotPreferred := openAIWSIdentityAcquirePolicy(
				tt.identityFresh,
				tt.preferred,
				tt.guardPreferred,
				tt.dedicated,
			)
			if gotNew != tt.wantForceNew || gotPreferred != tt.wantForcePreferred {
				t.Fatalf("policy = (forceNew=%v, forcePreferred=%v), want (%v, %v)", gotNew, gotPreferred, tt.wantForceNew, tt.wantForcePreferred)
			}
		})
	}
}

func TestCodexWSIdentityCompatibilityKey_DeviceWithoutConversationIsFresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	account := newTestOAuthAccount(8081, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 1)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.Empty(t, key)
	require.True(t, fresh)
}

func TestCodexWSIdentityCompatibilityKey_DeviceWithoutAPIKeyIsFresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"client-session-1","thread_id":"client-thread-1"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	account := newTestOAuthAccount(8084, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 0)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.Empty(t, key)
	require.True(t, fresh)
}

func TestCodexWSIdentityCompatibilityKey_IncludesParentThread(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newTestOAuthAccount(8085, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 505})
	bodyA := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"client-session-1","thread_id":"client-thread-1","parent_thread_id":"parent-a"}}`)
	bodyB := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"client-session-1","thread_id":"client-thread-1","parent_thread_id":"parent-b"}}`)
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, bodyA, 505)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	keyA, freshA := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyA)
	keyB, freshB := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyB)
	require.False(t, freshA)
	require.False(t, freshB)
	require.NotEmpty(t, keyA)
	require.NotEmpty(t, keyB)
	require.NotEqual(t, keyA, keyB)
}

func TestCodexWSIdentityCompatibilityKey_ConflictingParentHeadersAreFresh(t *testing.T) {
	account := newTestOAuthAccount(8086, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("x-codex-parent-thread-id", "parent-inbound")
	c.Set("api_key", &APIKey{ID: 506})
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"s","thread_id":"t"}}`)
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 506)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)
	outbound := c.Request.Header.Clone()
	outbound.Set("x-codex-parent-thread-id", "parent-outbound")
	key, fresh := codexWSIdentityCompatibilityKey(account, c, outbound, body)
	require.Empty(t, key)
	require.True(t, fresh)
}

func TestCodexWSIdentityCompatibilityKey_OffWithoutAPIKeyIsFresh(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.Empty(t, key)
	require.True(t, fresh)
}

func TestCodexWSIdentityCompatibilityKey_OffBodyCarrierScopesWithAPIKey(t *testing.T) {
	account := &Account{
		ID:       8087,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{codexFingerprintModeExtraKey: string(codexFingerprintOff)},
	}
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"installation_id":"install","session_id":"session","thread_id":"thread"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Set("api_key", &APIKey{ID: 507})

	key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
	require.NotEmpty(t, key)
	require.False(t, fresh)
}

func TestCodexWSIdentityCompatibilityKey_IgnoresTurnRotation(t *testing.T) {
	account := &Account{
		ID:       8088,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{codexFingerprintModeExtraKey: string(codexFingerprintOff)},
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 508})
	c.Request.Header.Set("session-id", "session")
	c.Request.Header.Set("thread-id", "thread")
	c.Request.Header.Set("x-codex-installation-id", "install")
	bodyA := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"installation_id":"install","session_id":"session","thread_id":"thread","turn_id":"turn-a"}}`)
	bodyB := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"installation_id":"install","session_id":"session","thread_id":"thread","turn_id":"turn-b"}}`)
	keyA, freshA := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyA)
	keyB, freshB := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyB)
	require.NotEmpty(t, keyA)
	require.NotEmpty(t, keyB)
	require.Equal(t, keyA, keyB)
	require.False(t, freshA)
	require.False(t, freshB)
}

func TestCodexWSIdentityCompatibilityKey_ConvergedLifecycleModesScopeByAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"client-session-1"}}`)

	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			account := newTestOAuthAccount(8082, map[string]any{
				codexFingerprintModeExtraKey: string(mode),
				codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
			})
			ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 101)
			require.NotNil(t, ids)
			stageCodexFingerprintIDs(c, ids)
			c.Set("api_key", &APIKey{ID: 101})

			keyA, freshA := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
			require.NotEmpty(t, keyA)
			require.False(t, freshA)

			// A new turn changes only turn-scoped metadata. It must reuse the
			// same socket for this API key and lifecycle.
			nextTurn := *ids
			nextTurn.turnID = "turn-2"
			nextTurn.windowID = "window-2"
			stageCodexFingerprintIDs(c, &nextTurn)
			keySameLifecycle, freshSameLifecycle := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
			require.Equal(t, keyA, keySameLifecycle)
			require.False(t, freshSameLifecycle)

			reqA := openAIWSAcquireRequest{IdentityCompatibility: keyA}
			reqSameLifecycle := openAIWSAcquireRequest{IdentityCompatibility: keySameLifecycle}
			require.Equal(t,
				normalizeOpenAIWSHandshakeCompatibilityForRequest(reqA),
				normalizeOpenAIWSHandshakeCompatibilityForRequest(reqSameLifecycle),
			)

			c.Set("api_key", &APIKey{ID: 202})
			keyB, freshB := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
			require.NotEmpty(t, keyB)
			require.False(t, freshB)
			require.NotEqual(t, keyA, keyB)
			require.NotEqual(t,
				normalizeOpenAIWSHandshakeCompatibilityForRequest(reqA),
				normalizeOpenAIWSHandshakeCompatibilityForRequest(openAIWSAcquireRequest{IdentityCompatibility: keyB}),
			)
		})
	}
}

func TestCodexWSIdentityCompatibilityKey_ConvergedLifecycleWithoutAPIKeyIsFresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"session_id":"client-session-1"}}`)

	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			account := newTestOAuthAccount(8083, map[string]any{
				codexFingerprintModeExtraKey: string(mode),
				codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
			})
			ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 0)
			require.NotNil(t, ids)
			stageCodexFingerprintIDs(c, ids)

			key, fresh := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, body)
			require.Empty(t, key)
			require.True(t, fresh)
		})
	}
}

func TestCodexIdentityPassthrough_DoesNotCacheAPIKeyNegativeAcrossOAuthFailover(t *testing.T) {
	c, oauthAccount, body := newCompleteOfficialCodexIdentityContext(t)
	apiKeyAccount := *oauthAccount
	apiKeyAccount.Type = AccountTypeAPIKey
	apiKeyAccount.Extra = nil

	require.False(t, captureCodexClientIdentityPassthrough(c, &apiKeyAccount, c.Request.Header, body))
	// The API-key attempt must not poison the request-scoped decision. Once
	// failover selects OAuth, the original complete Codex snapshot is eligible
	// for passthrough evaluation.
	require.True(t, shouldPreserveCodexClientIdentityForRequestWithBody(c, oauthAccount, c.Request.Header, body))
}

func TestCodexIdentityPassthrough_FreezesFirstOAuthDecision(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	require.True(t, captureCodexClientIdentityPassthrough(c, account, c.Request.Header, body))

	// A retry may see a normalized body after the first account attempt. The
	// request-level identity decision must remain the one made at ingress.
	for _, key := range []string{
		"Session-Id", "Thread-Id", "X-Codex-Installation-Id", "X-Codex-Turn-Metadata",
	} {
		c.Request.Header.Del(key)
	}
	mutatedBody := []byte(`{"model":"gpt-5.6-codex","input":[]}`)
	require.True(t, captureCodexClientIdentityPassthrough(c, account, c.Request.Header, mutatedBody))
	require.True(t, shouldPreserveCodexClientIdentityForRequestWithBody(c, account, c.Request.Header, mutatedBody))
}
