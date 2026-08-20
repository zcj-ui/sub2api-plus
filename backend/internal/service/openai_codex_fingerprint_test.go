package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const testCodexFingerprintSeed = "11111111-1111-4111-8111-111111111111"

func newTestOAuthAccount(id int64, extra map[string]any) *Account {
	if codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(extra)) {
		if extra == nil {
			extra = make(map[string]any)
		}
		if _, exists := extra[codexFingerprintSeedExtraKey]; !exists {
			extra[codexFingerprintSeedExtraKey] = testCodexFingerprintSeed
		}
	}
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

// --- deriveStableUUIDv4 ---

func TestDeriveStableUUIDv4_Deterministic(t *testing.T) {
	a := deriveStableUUIDv4("test-seed-1")
	b := deriveStableUUIDv4("test-seed-1")
	assert.Equal(t, a, b, "同一种子应返回相同结果")
}

func TestDeriveStableUUIDv4_DifferentSeeds(t *testing.T) {
	a := deriveStableUUIDv4("seed-a")
	b := deriveStableUUIDv4("seed-b")
	assert.NotEqual(t, a, b, "不同种子应返回不同结果")
}

func TestDeriveStableUUIDv4_ValidFormat(t *testing.T) {
	result := deriveStableUUIDv4("test-seed")
	parsed, err := uuid.Parse(result)
	require.NoError(t, err, "应返回合法 UUID 格式")
	assert.Equal(t, uuid.Version(4), parsed.Version(), "应为 UUIDv4")
	assert.Equal(t, uuid.RFC4122, parsed.Variant(), "应为 RFC4122 变体")
}

// --- GetCodexFingerprintMode ---

func TestGetCodexFingerprintMode(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected codexFingerprintMode
	}{
		{"nil 账号", nil, codexFingerprintOff},
		{"非 OAuth 账号", &Account{Platform: PlatformOpenAI, Type: "api_key"}, codexFingerprintOff},
		// 收敛是显式 opt-in：缺省/空/非法一律 off（#5610）。存量账号普遍没有这个
		// extra 键，升级不得把它们静默切进收敛。
		{"无 extra 默认 off", newTestOAuthAccount(1, nil), codexFingerprintOff},
		{"空值默认 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: ""}), codexFingerprintOff},
		{"非法值默认 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "invalid"}), codexFingerprintOff},
		{"显式 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"}), codexFingerprintOff},
		{"device opt-in", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "device"}), codexFingerprintDevice},
		{"session opt-in", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"}), codexFingerprintSession},
		{"full opt-in", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "full"}), codexFingerprintFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.account.GetCodexFingerprintMode())
		})
	}
}

func TestNormalizeCodexFingerprintModeForStorageCanonicalizesValidModes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "device whitespace and case", input: "  DEVICE  ", expected: "device"},
		{name: "off uppercase", input: "OFF", expected: "off"},
		{name: "session", input: " session ", expected: "session"},
		{name: "full", input: "FULL", expected: "full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := map[string]any{codexFingerprintModeExtraKey: tt.input, "keep": "value"}
			got := normalizeCodexFingerprintModeForStorage(input)
			require.Equal(t, tt.expected, got[codexFingerprintModeExtraKey])
			require.Equal(t, "value", got["keep"])
			// Normalization must not mutate a request map that may be reused by
			// a caller when it needs to change the stored representation.
			require.Equal(t, tt.input, input[codexFingerprintModeExtraKey])
		})
	}
}

func TestNormalizeCodexFingerprintModeForStorageLeavesInvalidValues(t *testing.T) {
	input := map[string]any{codexFingerprintModeExtraKey: "not-a-mode"}
	got := normalizeCodexFingerprintModeForStorage(input)
	require.Equal(t, input, got)
	require.Equal(t, "not-a-mode", input[codexFingerprintModeExtraKey])
}

// --- resolveConvergedInstallationID ---

func TestResolveConvergedInstallationID_UsesDeviceID(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{"openai_device_id": "real-device-id"})
	assert.Equal(t, "real-device-id", resolveConvergedInstallationID(account, testCodexFingerprintSeed))
}

func TestResolveConvergedInstallationID_UsesPersistedSeed(t *testing.T) {
	account := newTestOAuthAccount(42, map[string]any{
		codexFingerprintModeExtraKey: "device",
		codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222",
	})
	result := resolveConvergedInstallationID(account)
	assert.Equal(t,
		deriveStableUUIDv4("sub2api:codex-install-id:v2:22222222-2222-4222-8222-222222222222"),
		result,
	)
	_, err := uuid.Parse(result)
	require.NoError(t, err, "派生值应为合法 UUID")
	assert.Equal(t, result, resolveConvergedInstallationID(account), "确定性")
}

func TestResolveConvergedInstallationID_DifferentSeedsWithSameLocalID(t *testing.T) {
	a := resolveConvergedInstallationID(newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	}))
	b := resolveConvergedInstallationID(newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222",
	}))
	assert.NotEqual(t, a, b)
}

func TestResolveConvergedInstallationID_DoesNotFallbackToLocalID(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{codexFingerprintModeExtraKey: "device"}}
	assert.Empty(t, resolveConvergedInstallationID(account))
}

func TestResolveCodexFingerprintIDsRequiresPersistedSeed(t *testing.T) {
	for _, mode := range []codexFingerprintMode{
		codexFingerprintDevice,
		codexFingerprintSession,
		codexFingerprintFull,
	} {
		t.Run(string(mode), func(t *testing.T) {
			// A legacy device_id alone is not enough to mint a complete
			// account-owned snapshot. In particular, session/full must never
			// overwrite client lifecycle headers with empty IDs.
			account := &Account{
				ID:       43,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					codexFingerprintModeExtraKey: string(mode),
					"openai_device_id":           "legacy-device",
				},
			}
			require.Nil(t, resolveCodexFingerprintIDs(account, "client-session", mode))
		})
	}
}

func TestResolveCodexFingerprintIDs_UsesOfficialV2Derivations(t *testing.T) {
	const seed = "11111111-1111-4111-8111-111111111111"
	account := newTestOAuthAccount(42, map[string]any{
		codexFingerprintModeExtraKey: "session",
		codexFingerprintSeedExtraKey: seed,
	})

	ids := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintSession)
	require.NotNil(t, ids)
	assert.Equal(t, deriveStableUUIDv4("sub2api:codex-install-id:v2:"+seed), ids.installationID)
	assert.Equal(t, deriveStableUUIDv4("sub2api:codex-session-id:v2:"+seed), ids.sessionID)
	assert.Equal(t, deriveStableUUIDv4("sub2api:codex-thread-id:v2:"+seed+":client-session"), ids.threadID)
}

func TestResolveCodexFingerprintIDs_IgnoresLegacyDeploymentSeed(t *testing.T) {
	accountA := newTestOAuthAccount(1, map[string]any{
		"chatgpt_account_id":         "chatgpt-a",
		codexFingerprintModeExtraKey: "full",
	})
	accountB := newTestOAuthAccount(1, map[string]any{
		"chatgpt_account_id":         "chatgpt-a",
		codexFingerprintModeExtraKey: "full",
	})
	// The old variadic parameter is intentionally ignored. Identity survives
	// backup/import through the persisted account seed, matching official code.
	one := resolveCodexFingerprintIDs(accountA, "client", codexFingerprintFull, "deployment-a")
	two := resolveCodexFingerprintIDs(accountB, "client", codexFingerprintFull, "deployment-b")
	require.NotNil(t, one)
	require.NotNil(t, two)
	assert.Equal(t, one.installationID, two.installationID)
	assert.Equal(t, one.sessionID, two.sessionID)
	assert.Equal(t, one.sessionID, resolveCodexFingerprintIDs(accountA, "client", codexFingerprintFull, "deployment-a").sessionID)
}

func TestEnsureCodexFingerprintSeedGeneratesOnlyForOpenAIOAuth(t *testing.T) {
	extra := map[string]any{
		codexFingerprintModeExtraKey: "session",
		"openai_device_id":           "legacy-device",
		"openai_session_id":          "legacy-session",
		"keep":                       "value",
	}
	seeded := ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, extra)
	require.NotEmpty(t, seeded[codexFingerprintSeedExtraKey])
	assert.Equal(t, "value", seeded["keep"])
	assert.Empty(t, extra[codexFingerprintSeedExtraKey], "caller map is not mutated")
	assert.Equal(t, seeded, ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, seeded))
	assert.Nil(t, ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeAPIKey, nil))
}

func TestEnsureCodexFingerprintSeedReplacesInvalidSeed(t *testing.T) {
	extra := map[string]any{
		codexFingerprintModeExtraKey: "session",
		codexFingerprintSeedExtraKey: "not-a-uuid",
	}
	seeded := ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, extra)
	seed, ok := seeded[codexFingerprintSeedExtraKey].(string)
	require.True(t, ok)
	require.NotEqual(t, "not-a-uuid", seed)
	_, err := uuid.Parse(seed)
	require.NoError(t, err)
	require.Equal(t, "not-a-uuid", extra[codexFingerprintSeedExtraKey])
}

func TestEnsureCodexFingerprintSeedRejectsNonCanonicalUUID(t *testing.T) {
	for _, seed := range []string{
		"11111111-1111-4111-8111-11111111111A",
		"00000000-0000-0000-0000-000000000000",
	} {
		t.Run(seed, func(t *testing.T) {
			extra := map[string]any{
				codexFingerprintModeExtraKey: string(codexFingerprintSession),
				codexFingerprintSeedExtraKey: seed,
			}
			got := ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, extra)
			newSeed, ok := got[codexFingerprintSeedExtraKey].(string)
			require.True(t, ok)
			require.NotEqual(t, seed, newSeed)
			require.True(t, isCodexFingerprintSeed(newSeed))
		})
	}
}

func TestEnsureCodexFingerprintSeedPreservesCanonicalNonV4UUID(t *testing.T) {
	// The seed is opaque persisted account state. Accepting a canonical UUIDv1
	// avoids rotating identities restored from an older export.
	const importedSeed = "11111111-1111-1111-8111-111111111111"
	extra := map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintSession),
		codexFingerprintSeedExtraKey: importedSeed,
	}
	got := ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, extra)
	require.Equal(t, importedSeed, got[codexFingerprintSeedExtraKey])
	require.True(t, isCodexFingerprintSeed(importedSeed))
}

func TestCodexFingerprintSeedImportDoesNotChooseAccountIdentity(t *testing.T) {
	const importedSeed = " 11111111-1111-1111-8111-1111111111AA "
	const canonicalSeed = "11111111-1111-1111-8111-1111111111aa"
	extra := map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintSession),
		codexFingerprintSeedExtraKey: importedSeed,
	}

	require.NoError(t, ValidateCodexFingerprintExtra(PlatformOpenAI, AccountTypeOAuth, extra))
	normalized := NormalizeCodexFingerprintExtraForAccount(PlatformOpenAI, AccountTypeOAuth, extra)
	seed, ok := codexFingerprintSeed(normalized)
	require.True(t, ok)
	require.NotEqual(t, canonicalSeed, seed)
	require.Equal(t, importedSeed, extra[codexFingerprintSeedExtraKey], "normalization must not mutate the import payload")
}

func TestValidateCodexFingerprintExtraIgnoresServerOwnedSeed(t *testing.T) {
	for _, tt := range []struct {
		name        string
		platform    string
		accountType string
		extra       map[string]any
	}{
		{
			name:        "malformed OAuth seed",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			extra: map[string]any{
				codexFingerprintModeExtraKey: "session",
				codexFingerprintSeedExtraKey: []string{"not-a-uuid"},
			},
		},
		{
			name:        "non OAuth seed",
			platform:    PlatformOpenAI,
			accountType: AccountTypeAPIKey,
			extra: map[string]any{
				codexFingerprintSeedExtraKey: "not-a-uuid",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, ValidateCodexFingerprintExtra(tt.platform, tt.accountType, tt.extra))
		})
	}
}

func TestEnsureCodexFingerprintSeed_LeavesOptOutAccountUnchanged(t *testing.T) {
	extra := map[string]any{"keep": "value"}
	got := ensureCodexFingerprintSeed(PlatformOpenAI, AccountTypeOAuth, extra)
	assert.Equal(t, extra, got)
	assert.Empty(t, got[codexFingerprintSeedExtraKey])
}

func TestBuildAccountForCreate_ScrubsOAuthOnlyIdentityFromNonOAuthImport(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			extra := map[string]any{
				codexFingerprintModeExtraKey:       "device",
				codexFingerprintSeedExtraKey:       "11111111-1111-4111-8111-111111111111",
				"openai_device_id":                 "legacy-device",
				"openai_session_id":                "legacy-session",
				OpenAICodex429GuardEnabledExtraKey: true,
				"keep":                             "value",
			}
			account, err := buildAccountForCreate(&CreateAccountInput{
				Name:     "legacy-import",
				Platform: PlatformOpenAI,
				Type:     accountType,
			}, extra)
			require.NoError(t, err)
			for _, key := range []string{
				codexFingerprintModeExtraKey,
				codexFingerprintSeedExtraKey,
				"openai_device_id",
				"openai_session_id",
				OpenAICodex429GuardEnabledExtraKey,
			} {
				require.NotContains(t, account.Extra, key)
				require.Contains(t, extra, key, "create normalization must not mutate the import payload")
			}
			require.Equal(t, "value", account.Extra["keep"])
		})
	}
}

// --- resolveConvergedThreadID ---

func TestResolveConvergedThreadID_PerClientSession(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-bbb")
	assert.NotEqual(t, a, b, "不同客户端 session 应得到不同 thread_id")
}

func TestResolveConvergedThreadID_Deterministic(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	assert.Equal(t, a, b, "同一客户端 session 应得到相同 thread_id")
}

func TestResolveConvergedThreadID_EmptySession(t *testing.T) {
	assert.Equal(t, "", resolveConvergedThreadID(testCodexFingerprintSeed, ""))
}

func TestResolveCodexFingerprintIDsForRequest_DoesNotUseCacheOrContentAsThreadAnchor(t *testing.T) {
	round1 := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]}]}`)
	round2 := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}]}`)
	other := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"different opener"}]}]}`)

	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids1 := resolveCodexFingerprintIDsForRequest(account, nil, round1, 17)
	ids2 := resolveCodexFingerprintIDsForRequest(account, nil, round2, 17)
	idsOther := resolveCodexFingerprintIDsForRequest(account, nil, other, 17)
	require.NotNil(t, ids1)
	require.Equal(t, ids1.installationID, ids2.installationID)
	require.Equal(t, ids1.sessionID, ids2.sessionID)
	require.Equal(t, ids1.sessionID, idsOther.sessionID, "official session mode is account-scoped")
	require.Equal(t, ids1.threadID, ids2.threadID)
	require.Equal(t, ids1.threadID, idsOther.threadID, "cache keys and prompt content are not lifecycle identities")
}

func TestResolveCodexFingerprintIDsForRequest_UsesExplicitBodySessionAsCompatibilityAnchor(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	first := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"session_id":"client-session-a"},"prompt_cache_key":"cache-a"}`), 17)
	second := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"session_id":"client-session-a"},"prompt_cache_key":"cache-b"}`), 17)
	other := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"session_id":"client-session-b"}}`), 17)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.NotNil(t, other)
	require.Equal(t, first.threadID, second.threadID)
	require.NotEqual(t, first.threadID, other.threadID)
}

func TestResolveCodexFingerprintIDsForRequest_UsesNestedTurnMetadataSession(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	first := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-a\",\"thread_id\":\"nested-thread-a\"}"}}`), 17)
	second := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-a\",\"thread_id\":\"nested-thread-a\"}"}}`), 17)
	other := resolveCodexFingerprintIDsForRequest(account, nil, []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-b\",\"thread_id\":\"nested-thread-b\"}"}}`), 17)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.NotNil(t, other)
	require.Equal(t, first.threadID, second.threadID)
	require.NotEqual(t, first.threadID, other.threadID)
}

// --- off 模式：resolveCodexFingerprintIDsFromRequest 返回 nil ---

func TestCodexWSIdentityCompatibilityKey_UsesNestedTurnMetadataSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: string(codexFingerprintDevice)})
	bodyA := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-a\",\"thread_id\":\"nested-thread-a\"}"}}`)
	bodyB := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-b\",\"thread_id\":\"nested-thread-b\"}"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(bodyA)))
	c.Set("api_key", &APIKey{ID: 17})
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, bodyA, 17)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	keyA, freshA := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyA)
	keyB, freshB := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyB)
	require.False(t, freshA)
	require.False(t, freshB)
	require.NotEmpty(t, keyA)
	require.NotEmpty(t, keyB)
	require.NotEqual(t, keyA, keyB)

	// A proxy can leave one flat carrier while retaining the nested snapshot.
	// The present thread header must not suppress the nested session lookup.
	c.Request.Header.Set("thread-id", "flat-thread")
	bodyC := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-c\",\"thread_id\":\"nested-thread-c\"}"}}`)
	bodyD := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"nested-install\",\"session_id\":\"nested-session-d\",\"thread_id\":\"nested-thread-c\"}"}}`)
	keyC, freshC := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyC)
	keyD, freshD := codexWSIdentityCompatibilityKey(account, c, c.Request.Header, bodyD)
	require.False(t, freshC)
	require.False(t, freshD)
	require.NotEqual(t, keyC, keyD)
}

func TestShouldApplyCodexFingerprintForRequestUsesExplicitAccountOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newTestOAuthAccount(1004, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	newContext := func(body []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("User-Agent", "curl/8.0")
		return c
	}

	plain := []byte(`{"model":"gpt-5.4","input":[]}`)
	// A reverse proxy may remove all Codex-specific headers and use a generic
	// model alias. An explicitly enabled account must still converge.
	require.True(t, shouldApplyCodexFingerprintForRequest(newContext(plain), account, plain))

	codexModel := []byte(`{"model":"gpt-5.4-codex","input":[]}`)
	require.True(t, shouldApplyCodexFingerprintForRequest(newContext(codexModel), account, codexModel))

	bodyCarrier := []byte(`{"model":"gpt-5.4","input":[],"client_metadata":{"session_id":"s1"}}`)
	require.True(t, shouldApplyCodexFingerprintForRequest(newContext(bodyCarrier), account, bodyCarrier))

	marked := newContext(plain)
	stageCodexFingerprintClientClassification(marked, true)
	require.True(t, shouldApplyCodexFingerprintForRequest(marked, account, plain))

	offAccount := newTestOAuthAccount(1006, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintOff),
	})
	require.False(t, shouldApplyCodexFingerprintForRequest(newContext(plain), offAccount, plain))
}

func newBodyOnlyDeviceFingerprintContext(t *testing.T) (*gin.Context, *Account, []byte) {
	t.Helper()
	body := []byte(`{"model":"gpt-5.6-codex","stream":true,"input":[],"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"body-install\",\"session_id\":\"body-session\",\"thread_id\":\"body-thread\",\"window_id\":\"body-thread:0\",\"parent_thread_id\":\"body-parent\"}"}}`)
	c := newFingerprintStageTestContext(t)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	account := newTestOAuthAccount(1005, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
		"openai_oauth_passthrough":   true,
	})
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 17)
	require.NotNil(t, ids)
	require.False(t, ids.projectionMalformed)
	stageCodexFingerprintIDs(c, ids)
	return c, account, body
}

func requireBodyOnlyCodexLifecycleHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	require.Equal(t, "body-session", headers.Get("session-id"))
	require.Equal(t, "body-session", headers.Get("session_id"))
	require.Equal(t, "body-thread", headers.Get("thread-id"))
	require.Equal(t, "body-thread", headers.Get("thread_id"))
	require.Equal(t, "body-thread:0", headers.Get("x-codex-window-id"))
	require.Equal(t, "body-parent", headers.Get("x-codex-parent-thread-id"))
}

func TestBodyOnlyCodexLifecycleProjectionRestoredAcrossHTTPAndWSBuilders(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	t.Run("ordinary HTTP", func(t *testing.T) {
		c, account, body := newBodyOnlyDeviceFingerprintContext(t)
		req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", true, "", true)
		require.NoError(t, err)
		requireBodyOnlyCodexLifecycleHeaders(t, req.Header)
	})

	t.Run("passthrough HTTP", func(t *testing.T) {
		c, account, body := newBodyOnlyDeviceFingerprintContext(t)
		req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "oauth-token")
		require.NoError(t, err)
		requireBodyOnlyCodexLifecycleHeaders(t, req.Header)
	})

	t.Run("WebSocket handshake", func(t *testing.T) {
		c, account, body := newBodyOnlyDeviceFingerprintContext(t)
		headers, _, err := svc.buildOpenAIWSHeadersWithBody(
			context.Background(), c, account, "oauth-token",
			OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			true, "", "", "", "gpt-5.6-codex", "", body,
		)
		require.NoError(t, err)
		requireBodyOnlyCodexLifecycleHeaders(t, headers)
	})
}

func TestBodyOnlyCodexLifecycleProjectionDoesNotMixWithExplicitHeaders(t *testing.T) {
	c, account, body := newBodyOnlyDeviceFingerprintContext(t)
	c.Request.Header.Set("session-id", "explicit-session")
	headers := http.Header{}
	headers.Set("session-id", "explicit-session")

	restoreStagedCodexIdentityHeadersFromBody(c, account, headers, body)
	// A body from another session must not complete an explicit header snapshot.
	require.Equal(t, "explicit-session", headers.Get("session-id"))
	require.Empty(t, headers.Get("session_id"))
	require.Empty(t, headers.Get("thread-id"))
	require.Empty(t, headers.Get("x-codex-window-id"))
	require.Empty(t, headers.Get("x-codex-parent-thread-id"))
}

func TestBodyOnlySessionConvergenceOverwritesBothThreadHeaderAliases(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	for _, builder := range []struct {
		name string
		call func(t *testing.T, c *gin.Context, account *Account, body []byte) http.Header
	}{
		{
			name: "ordinary HTTP",
			call: func(t *testing.T, c *gin.Context, account *Account, body []byte) http.Header {
				req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "oauth-token", true, "", true)
				require.NoError(t, err)
				return req.Header
			},
		},
		{
			name: "passthrough HTTP",
			call: func(t *testing.T, c *gin.Context, account *Account, body []byte) http.Header {
				req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "oauth-token")
				require.NoError(t, err)
				return req.Header
			},
		},
		{
			name: "WebSocket handshake",
			call: func(t *testing.T, c *gin.Context, account *Account, body []byte) http.Header {
				headers, _, err := svc.buildOpenAIWSHeadersWithBody(
					context.Background(), c, account, "oauth-token",
					OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
					true, "", "", "", "gpt-5.6-codex", "", body,
				)
				require.NoError(t, err)
				return headers
			},
		},
	} {
		t.Run(builder.name, func(t *testing.T) {
			c, account, body := newBodyOnlyDeviceFingerprintContext(t)
			account.Extra[codexFingerprintModeExtraKey] = string(codexFingerprintSession)
			ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 17)
			require.NotNil(t, ids)
			stageCodexFingerprintIDs(c, ids)

			headers := builder.call(t, c, account, body)
			require.Equal(t, ids.sessionID, headers.Get("session-id"))
			require.Equal(t, ids.sessionID, headers.Get("session_id"))
			require.Equal(t, ids.threadID, headers.Get("thread-id"))
			require.Equal(t, ids.threadID, headers.Get("thread_id"))
			require.Equal(t, ids.windowID, headers.Get("x-codex-window-id"))
			// parent_thread_id remains client-owned in session mode and is restored
			// from the valid body only because the relay did not send a header.
			require.Equal(t, "body-parent", headers.Get("x-codex-parent-thread-id"))
		})
	}
}

func TestResolveCodexFingerprintIDsFromRequest_ExplicitOff(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	assert.Nil(t, ids, "显式 off 模式应返回 nil")
}

// 未显式配置的存量账号不得被收敛（#5610）：默认返回 nil，出站身份保持
// v0.1.175 之前的客户端原值。
func TestResolveCodexFingerprintIDsFromRequest_DefaultIsOff(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	assert.Nil(t, resolveCodexFingerprintIDsFromRequest(account, nil), "无 extra 应视为 off")
}

// 管理员显式 opt-in 的账号行为不变。
func TestResolveCodexFingerprintIDsFromRequest_ExplicitOptInHonored(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: mode})
			ids := resolveCodexFingerprintIDsFromRequest(account, nil)
			require.NotNil(t, ids, "explicit opt-in must create a fingerprint snapshot")
			assert.Equal(t, codexFingerprintMode(mode), ids.mode)
			assert.NotEmpty(t, ids.installationID)
		})
	}
}

func TestResolveCodexFingerprintIDsFromRequest_EnabledModesRequireValidSeed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra map[string]any
	}{
		{name: "missing", extra: map[string]any{codexFingerprintModeExtraKey: "device"}},
		{name: "missing with device override", extra: map[string]any{codexFingerprintModeExtraKey: "device", "openai_device_id": "real-device"}},
		{name: "blank", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: ""}},
		{name: "nil uuid", extra: map[string]any{codexFingerprintModeExtraKey: "device", codexFingerprintSeedExtraKey: "00000000-0000-0000-0000-000000000000"}},
		{name: "non string", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: 123}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tt.extra}
			require.Nil(t, resolveCodexFingerprintIDsFromRequest(account, nil))
		})
	}
}

func TestResolveCodexFingerprintIDsFromRequest_CanonicalizesImportedUppercaseSeed(t *testing.T) {
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "full",
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-AAAAAAAAAAAA",
		},
	}

	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)
	assert.Equal(t, "11111111-1111-4111-8111-aaaaaaaaaaaa", account.getCodexFingerprintSeed())
}

// --- applyCodexFingerprintHeaders: off 模式 ---

func TestApplyCodexFingerprintHeaders_OffMode(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-installation-id", "original-install-id")
	h.Set("x-codex-window-id", "original-window-id")

	applyCodexFingerprintHeaders(h, nil)

	assert.Equal(t, "original-install-id", h.Get("x-codex-installation-id"), "nil ids 不改写")
	assert.Equal(t, "original-window-id", h.Get("x-codex-window-id"), "nil ids 不改写")
}

// --- applyCodexFingerprintHeaders: device 模式 ---

func TestApplyCodexFingerprintHeaders_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","sandbox":"seccomp"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-window:0")
	h.Set("x-codex-turn-metadata", turnMetadata)

	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, "converged-device", h.Get("x-codex-installation-id"), "enabled mode must emit the converged installation header")
	assert.Equal(t, "user-window:0", h.Get("x-codex-window-id"), "device 模式不改写 window_id")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "user-session", meta["session_id"], "device 模式不改写 session_id")
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

// --- applyCodexFingerprintHeaders: session 模式 ---

func TestApplyCodexFingerprintHeaders_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","thread_id":"user-thread","turn_id":"user-turn","window_id":"user-thread:0","sandbox":"seccomp","thread_source":"user"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-thread:0")
	h.Set("x-codex-turn-metadata", turnMetadata)
	h.Set("conversation_id", "independent-conversation")
	h.Set("x-client-request-id", "user-thread")

	ids := resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), codexFingerprintSession)
	applyCodexFingerprintHeaders(h, ids)

	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(seed)
	convergedThread := resolveConvergedThreadID(seed, "client-session-aaa")

	assert.Equal(t, convergedInstall, h.Get("x-codex-installation-id"), "enabled mode must emit the converged installation header")
	assert.Equal(t, convergedSession, h.Get("session-id"))
	assert.Equal(t, convergedSession, h.Get("session_id"), "下划线形式也应被改写")
	assert.Equal(t, convergedThread, h.Get("thread-id"))
	assert.Equal(t, convergedThread, h.Get("thread_id"), "下划线 thread 别名也必须与收敛生命周期一致")
	requestID := h.Get("x-client-request-id")
	require.NotEmpty(t, requestID)
	_, err := uuid.Parse(requestID)
	require.NoError(t, err, "x-client-request-id 应为每次请求生成的 UUID")
	assert.Equal(t, convergedThread, requestID)
	assert.Equal(t, convergedThread+":0", h.Get("x-codex-window-id"))
	assert.Equal(t, "independent-conversation", h.Get("conversation_id"),
		"fingerprint convergence must not overwrite an independent conversation id")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, convergedThread, meta["thread_id"])
	assert.NotEqual(t, "user-turn", meta["turn_id"], "turn_id 应被新生成的值替换")
	assert.Equal(t, "seccomp", meta["sandbox"], "sandbox 保留原样")
	assert.Equal(t, "user", meta["thread_source"], "thread_source 保留原样")
}

func TestDeviceFingerprintMalformedMetadataRebuildsAndDisablesRawSessionPreservation(t *testing.T) {
	account := newTestOAuthAccount(2101, map[string]any{
		codexFingerprintModeExtraKey: "device",
	})
	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session-id", "raw-client-session")
	c.Request.Header.Set("thread-id", "raw-client-thread")
	c.Request.Header.Set("x-codex-turn-metadata", "not-json")
	body := []byte(`{"model":"gpt-5.6-sol","input":[],"client_metadata":{"x-codex-turn-metadata":"not-json"}}`)
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, 77)
	require.NotNil(t, ids)
	require.True(t, ids.projectionMalformed)
	stageCodexFingerprintIDs(c, ids)
	assert.False(t, shouldPreserveCodexClientSessionIdentityForRequest(c, account))

	svc := &OpenAIGatewayService{}
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)
	assert.NotEqual(t, "raw-client-session", req.Header.Get("session-id"),
		"malformed metadata must use per-API-key session isolation")
	assert.NotEqual(t, "raw-client-thread", req.Header.Get("thread-id"),
		"malformed metadata must use per-API-key thread isolation")
	assert.Equal(t, ids.installationID, req.Header.Get("x-codex-installation-id"))
	assert.True(t, gjson.Valid(req.Header.Get("x-codex-turn-metadata")),
		"malformed turn metadata must be rebuilt before forwarding")
}

// --- session 模式：不同客户端得到不同 thread ---

func TestCodexFingerprintProjectionRejectsStableHeaderBodyConflicts(t *testing.T) {
	baseHeaders := func() http.Header {
		return http.Header{
			"x-codex-installation-id":  {"header-install"},
			"session-id":               {"stable-session"},
			"thread-id":                {"stable-thread"},
			"x-codex-window-id":        {"stable-thread:0"},
			"x-codex-parent-thread-id": {"stable-parent"},
		}
	}
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"body-install","session_id":"stable-session","thread_id":"stable-thread","x-codex-window-id":"stable-thread:0","parent_thread_id":"stable-parent","x-codex-turn-metadata":"{\"installation_id\":\"body-install\",\"session_id\":\"stable-session\",\"thread_id\":\"stable-thread\",\"turn_id\":\"body-turn\",\"window_id\":\"stable-thread:0\",\"parent_thread_id\":\"stable-parent\"}"}}`)
	for name, mutate := range map[string]func(http.Header){
		"session": func(h http.Header) { h.Set("session-id", "other-session") },
		"thread":  func(h http.Header) { h.Set("thread-id", "other-thread") },
		"window":  func(h http.Header) { h.Set("x-codex-window-id", "other-thread:0") },
		"parent":  func(h http.Header) { h.Set("x-codex-parent-thread-id", "other-parent") },
	} {
		t.Run(name, func(t *testing.T) {
			headers := baseHeaders()
			mutate(headers)
			require.True(t, codexFingerprintProjectionMalformed(headers, body))
		})
	}
}

func TestCodexFingerprintProjectionIgnoresCrossProjectionTurnRotation(t *testing.T) {
	headers := http.Header{
		"x-codex-installation-id":  {"header-install"},
		"session-id":               {"stable-session"},
		"thread-id":                {"stable-thread"},
		"x-codex-window-id":        {"stable-thread:0"},
		"x-codex-parent-thread-id": {"stable-parent"},
		"x-codex-turn-metadata":    {`{"installation_id":"header-install","session_id":"stable-session","thread_id":"stable-thread","turn_id":"header-turn","window_id":"stable-thread:0","parent_thread_id":"stable-parent"}`},
	}
	body := []byte(`{"client_metadata":{"session_id":"stable-session","thread_id":"stable-thread","x-codex-window-id":"stable-thread:0","parent_thread_id":"stable-parent","x-codex-turn-metadata":"{\"installation_id\":\"body-install\",\"session_id\":\"stable-session\",\"thread_id\":\"stable-thread\",\"turn_id\":\"body-turn\",\"window_id\":\"stable-thread:0\",\"parent_thread_id\":\"stable-parent\"}"}}`)
	require.False(t, codexFingerprintProjectionMalformed(headers, body))
	conflictingFlatTurn := []byte(`{"client_metadata":{"session_id":"stable-session","thread_id":"stable-thread","turn_id":"flat-turn","x-codex-turn-metadata":"{\"installation_id\":\"body-install\",\"session_id\":\"stable-session\",\"thread_id\":\"stable-thread\",\"turn_id\":\"nested-turn\"}"}}`)
	require.True(t, codexFingerprintProjectionMalformed(nil, conflictingFlatTurn))
}

func TestMalformedDevicePassthroughAlignsBodyWithIsolatedHeaders(t *testing.T) {
	const apiKeyID int64 = 1701
	account := newTestOAuthAccount(2102, map[string]any{codexFingerprintModeExtraKey: "device", "openai_oauth_passthrough": true})
	body := []byte(`{"model":"gpt-5.6-sol","input":[],"client_metadata":{"session_id":"raw-session","thread_id":"raw-thread","turn_id":"raw-turn","x-codex-window-id":"raw-thread:0","parent_thread_id":"raw-parent","x-codex-turn-metadata":"not-json"}}`)
	c := newFingerprintStageTestContext(t)
	c.Set("api_key", &APIKey{ID: apiKeyID})
	c.Request.Header.Set("session-id", "raw-session")
	c.Request.Header.Set("thread-id", "raw-thread")
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, apiKeyID)
	require.NotNil(t, ids)
	require.True(t, ids.projectionMalformed)
	stageCodexFingerprintIDs(c, ids)
	transformedBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	req, err := (&OpenAIGatewayService{}).buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, transformedBody, "test-token")
	require.NoError(t, err)
	finalBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	wantSession := isolateOpenAISessionID(apiKeyID, "raw-session")
	wantThread := isolateOpenAISessionID(apiKeyID, "raw-thread")
	require.Equal(t, wantSession, req.Header.Get("session-id"))
	require.Equal(t, wantThread, req.Header.Get("thread-id"))
	require.Equal(t, "raw-thread:0", req.Header.Get("x-codex-window-id"))
	require.Equal(t, "raw-parent", req.Header.Get("x-codex-parent-thread-id"))
	require.Equal(t, wantSession, gjson.GetBytes(finalBody, "client_metadata.session_id").String())
	require.Equal(t, wantThread, gjson.GetBytes(finalBody, "client_metadata.thread_id").String())
	require.Equal(t, "raw-thread:0", gjson.GetBytes(finalBody, "client_metadata.x-codex-window-id").String())
	require.Equal(t, "raw-parent", gjson.GetBytes(finalBody, "client_metadata.parent_thread_id").String())
	require.Equal(t, "raw-turn", gjson.GetBytes(finalBody, "client_metadata.turn_id").String())
	embedded := gjson.GetBytes(finalBody, "client_metadata.x-codex-turn-metadata").String()
	require.True(t, gjson.Valid(embedded))
	require.Equal(t, wantSession, gjson.Get(embedded, "session_id").String())
	require.Equal(t, wantThread, gjson.Get(embedded, "thread_id").String())
	require.Equal(t, "raw-parent", gjson.Get(embedded, "parent_thread_id").String())
}

func TestMalformedSessionAndFullParentProjectionUsesHeaderPriority(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			const apiKeyID int64 = 1702
			account := newTestOAuthAccount(2103, map[string]any{codexFingerprintModeExtraKey: string(mode), "openai_oauth_passthrough": true})
			body := []byte(`{"model":"gpt-5.6-sol","input":[],"client_metadata":{"session_id":"body-session","thread_id":"body-thread","x-codex-window-id":"body-thread:0","parent_thread_id":"body-parent","x-codex-turn-metadata":"{\"installation_id\":\"body-install\",\"session_id\":\"body-session\",\"thread_id\":\"body-thread\",\"turn_id\":\"body-turn\",\"window_id\":\"body-thread:0\",\"parent_thread_id\":\"body-parent\"}"}}`)
			c := newFingerprintStageTestContext(t)
			c.Set("api_key", &APIKey{ID: apiKeyID})
			c.Request.Header.Set("session-id", "header-session")
			c.Request.Header.Set("thread-id", "header-thread")
			c.Request.Header.Set("x-codex-window-id", "header-thread:0")
			c.Request.Header.Set("x-codex-parent-thread-id", "header-parent")
			ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, body, apiKeyID)
			require.NotNil(t, ids)
			require.True(t, ids.projectionMalformed)
			stageCodexFingerprintIDs(c, ids)
			transformedBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
			require.NoError(t, err)
			require.True(t, changed)
			req, err := (&OpenAIGatewayService{}).buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, transformedBody, "test-token")
			require.NoError(t, err)
			finalBody, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, "header-parent", req.Header.Get("x-codex-parent-thread-id"))
			require.Equal(t, "header-parent", gjson.GetBytes(finalBody, "client_metadata.parent_thread_id").String())
			embedded := gjson.GetBytes(finalBody, "client_metadata.x-codex-turn-metadata").String()
			require.Equal(t, "header-parent", gjson.Get(embedded, "parent_thread_id").String())
		})
	}
}

func TestApplyCodexFingerprintHeaders_SessionMode_DifferentClients(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})

	makeTurnMeta := func() string {
		return `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`
	}

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDs(account, extractClientSessionID(clientA), codexFingerprintSession)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDs(account, extractClientSessionID(clientB), codexFingerprintSession)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("session-id"), hB.Get("session-id"), "official session mode keeps one account-level session")
	assert.NotEqual(t, hA.Get("thread-id"), hB.Get("thread-id"), "不同客户端 thread_id 应不同")
	assert.NotEqual(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "不同客户端 window_id 应不同")
	assert.Equal(t, hA.Get("x-codex-installation-id"), hB.Get("x-codex-installation-id"))
	assert.NotEmpty(t, hA.Get("x-codex-installation-id"))
}

func TestApplyCodexFingerprintHeaders_RequestIDChangesPerAttempt(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	clientHeaders := http.Header{"session-id": []string{"client-session"}}
	ids := resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), codexFingerprintSession)
	h1 := http.Header{}
	h2 := http.Header{}
	applyCodexFingerprintHeaders(h1, ids)
	applyCodexFingerprintHeaders(h2, ids)
	require.NotEmpty(t, h1.Get("x-client-request-id"))
	require.NotEmpty(t, h2.Get("x-client-request-id"))
	require.Equal(t, h1.Get("x-client-request-id"), h2.Get("x-client-request-id"))
	require.Equal(t, ids.threadID, h1.Get("x-client-request-id"))
	require.Equal(t, h1.Get("session-id"), h2.Get("session-id"))
}

func TestNextCodexFingerprintTurn_PreservesConversationAndRotatesTurn(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-session", codexFingerprintSession)
	require.NotNil(t, ids)
	next := nextCodexFingerprintTurn(ids)
	require.NotNil(t, next)
	require.Equal(t, ids.installationID, next.installationID)
	require.Equal(t, ids.sessionID, next.sessionID)
	require.Equal(t, ids.threadID, next.threadID)
	require.Equal(t, ids.windowID, next.windowID)
	require.NotEqual(t, ids.turnID, next.turnID)
}

// --- full 模式 ---

func TestApplyCodexFingerprintHeaders_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDs(account, extractClientSessionID(clientA), codexFingerprintFull)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDs(account, extractClientSessionID(clientB), codexFingerprintFull)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("thread-id"), hB.Get("thread-id"), "full 模式 thread_id 应相同")
	assert.Equal(t, convergedSession, hA.Get("thread-id"), "full 模式 thread_id 应等于 session_id")
	assert.Equal(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "full 模式 window_id 应相同")
}

// --- H1 修复验证：头和体的 turn_id 一致性 ---

func TestFingerprintIDs_HeaderAndBody_TurnID_Consistent(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-xyz")

	ids := resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), codexFingerprintSession)
	require.NotNil(t, ids)

	// 头改写
	h := http.Header{}
	h.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(h, ids)

	// 体改写（使用同一份 ids）
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "x",
			"session_id":              "x",
			"turn_id":                 "x",
			"x-codex-turn-metadata":   `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}
	applyCodexFingerprintClientMetadata(reqBody, ids)

	// 从头 turn-metadata JSON 提取 turn_id
	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	headerTurnID, ok := headerMeta["turn_id"].(string)
	require.True(t, ok, "头 turn-metadata 应包含 string 类型的 turn_id")

	// 从体 client_metadata 提取 turn_id
	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok, "请求体应包含 client_metadata")
	bodyTurnID, ok := cm["turn_id"].(string)
	require.True(t, ok, "体 client_metadata 应包含 string 类型的 turn_id")

	// 从体内嵌 turn-metadata JSON 提取 turn_id
	embeddedRaw, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok, "体 client_metadata 应包含 x-codex-turn-metadata 字符串")
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(embeddedRaw), &bodyMeta))
	bodyEmbeddedTurnID, ok := bodyMeta["turn_id"].(string)
	require.True(t, ok, "体内嵌 turn-metadata 应包含 string 类型的 turn_id")
	headerTurnStartedAt, ok := headerMeta["turn_started_at_unix_ms"].(float64)
	require.True(t, ok, "头 turn-metadata 应包含 numeric 类型的 turn_started_at_unix_ms")
	bodyTurnStartedAt, ok := bodyMeta["turn_started_at_unix_ms"].(float64)
	require.True(t, ok, "体内嵌 turn-metadata 应包含 numeric 类型的 turn_started_at_unix_ms")

	assert.Equal(t, headerTurnID, bodyTurnID, "头和体的 turn_id 必须一致")
	assert.Equal(t, headerTurnID, bodyEmbeddedTurnID, "头和体内嵌 turn-metadata 的 turn_id 必须一致")
	assert.Equal(t, ids.turnID, headerTurnID, "所有 turn_id 都应来自同一份 ids")
	assert.Equal(t, int64(headerTurnStartedAt), int64(bodyTurnStartedAt), "头和体内嵌元数据的 turn_started_at_unix_ms 必须一致")
	assert.Equal(t, ids.turnStartedAtUnixMs, int64(headerTurnStartedAt), "时间戳必须来自同一份 ids")
}

func TestFingerprintIDs_MalformedEmbeddedMetadataRebuiltConsistently(t *testing.T) {
	account := newTestOAuthAccount(2, map[string]any{codexFingerprintModeExtraKey: "session"})
	clientHeaders := make(http.Header)
	clientHeaders.Set("session-id", "client-session-malformed")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	h := make(http.Header)
	h.Set("x-codex-turn-metadata", "{malformed")
	applyCodexFingerprintHeaders(h, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "client-session-malformed",
			"x-codex-turn-metadata": "[malformed",
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	clientMetadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	bodyRaw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodyRaw), &bodyMeta))

	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id", "turn_started_at_unix_ms"} {
		assert.Equal(t, headerMeta[key], bodyMeta[key], "rebuilt metadata field %s must match", key)
	}
}

// --- applyCodexFingerprintClientMetadata ---

func TestApplyCodexFingerprintClientMetadata_OffMode(t *testing.T) {
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original",
		},
	}
	modified := applyCodexFingerprintClientMetadata(reqBody, nil)
	assert.False(t, modified, "nil ids 不改写")
}

func TestApplyCodexFingerprintClientMetadata_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"user-session","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "user-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "converged-device", cm["x-codex-installation-id"])
	assert.Equal(t, "user-session", cm["session_id"], "device 模式不改 session_id")

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_SynchronizesCompatibilityInstallationAliases(t *testing.T) {
	account := newTestOAuthAccount(2, map[string]any{
		codexFingerprintModeExtraKey: "device",
	})
	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	require.NotNil(t, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"installation_id": "client-install",
			"x-codex-turn-metadata": map[string]any{
				"installation_id": "nested-client-install",
				"request_kind":    "turn",
			},
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))
	metadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ids.installationID, metadata["installation_id"])
	assert.Equal(t, ids.installationID, metadata["x-codex-installation-id"])
	nested, ok := metadata["x-codex-turn-metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ids.installationID, nested["installation_id"])
	assert.Equal(t, ids.installationID, nested["x-codex-installation-id"])
	assert.Equal(t, "turn", nested["request_kind"])
}

func TestApplyCodexFingerprintClientMetadata_CreatesMissingMetadataOnlyForOptIn(t *testing.T) {
	account := newTestOAuthAccount(17, map[string]any{
		codexFingerprintModeExtraKey: "device",
	})
	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	require.NotNil(t, ids)

	reqBody := map[string]any{"model": "gpt-5.6-codex"}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))
	metadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ids.installationID, metadata["x-codex-installation-id"])

	offBody := map[string]any{"model": "gpt-5.6-codex"}
	assert.False(t, applyCodexFingerprintClientMetadata(offBody, nil))
	assert.NotContains(t, offBody, "client_metadata")
}

func TestApplyCodexFingerprintClientMetadata_PreservesExplicitNonObjectMetadata(t *testing.T) {
	account := newTestOAuthAccount(18, map[string]any{
		codexFingerprintModeExtraKey: "device",
	})
	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	require.NotNil(t, ids)

	for _, value := range []any{"invalid", []any{"invalid"}, 7} {
		reqBody := map[string]any{"client_metadata": value}
		assert.False(t, applyCodexFingerprintClientMetadata(reqBody, ids))
		assert.Equal(t, value, reqBody["client_metadata"])
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_CreatesMissingMetadata(t *testing.T) {
	account := newTestOAuthAccount(19, map[string]any{
		codexFingerprintModeExtraKey: "device",
	})
	ids := resolveCodexFingerprintIDs(account, "", codexFingerprintDevice)
	require.NotNil(t, ids)

	body := []byte(`{"model":"gpt-5.6-codex","input":[]}`)
	next, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, ids.installationID, gjson.GetBytes(next, "client_metadata.x-codex-installation-id").String())

	invalid := []byte(`{"client_metadata":"invalid"}`)
	next, changed, err = applyCodexFingerprintClientMetadataRaw(invalid, ids)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, invalid, next)
}

func TestApplyCodexFingerprintClientMetadata_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	ids := resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), codexFingerprintSession)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "original-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(seed)
	convergedThread := resolveConvergedThreadID(seed, "client-session-aaa")

	assert.Equal(t, convergedInstall, cm["x-codex-installation-id"])
	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedThread, cm["thread_id"])
	assert.Equal(t, convergedThread+":0", cm["x-codex-window-id"])

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "any-client")

	ids := resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), codexFingerprintFull)
	require.NotNil(t, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "x",
			"thread_id":             "x",
			"x-codex-turn-metadata": `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedSession, cm["thread_id"], "full 模式 thread_id 应等于 session_id")
}

// --- extractClientSessionID ---

func TestExtractClientSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		expected string
	}{
		{"连字符形式优先", func() http.Header {
			h := http.Header{}
			h.Set("session-id", "hyphen-form")
			h.Set("session_id", "underscore-form")
			return h
		}(), "hyphen-form"},
		{"回退到下划线形式", func() http.Header {
			h := http.Header{}
			h.Set("session_id", "underscore-form")
			return h
		}(), "underscore-form"},
		{"都没有", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractClientSessionID(tt.headers))
		})
	}
}

// --- 透传路径：raw 字节版 client_metadata 改写 ---

// rawVsMapClientMetadata 用同一份 ids 分别跑 map 版与 raw 字节版，
// 返回两侧最终的 client_metadata 解码结果。
func rawVsMapClientMetadata(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, ids)
	mapCM, _ := decoded["client_metadata"].(map[string]any)

	rawBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	if !changed {
		return mapCM, nil
	}
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	rawCM, _ := rawDecoded["client_metadata"].(map[string]any)
	return mapCM, rawCM
}

func cloneCodexFingerprintIDsForTest(ids *codexFingerprintIDs) *codexFingerprintIDs {
	if ids == nil {
		return nil
	}
	cloned := *ids
	cloned.originalBodySessionID = ""
	cloned.originalBodySessionIDCaptured = false
	return &cloned
}

func applyMapAndRawFingerprintBodiesForTest(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	mapIDs := cloneCodexFingerprintIDsForTest(ids)
	rawIDs := cloneCodexFingerprintIDsForTest(ids)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, mapIDs)

	rawBody, _, err := applyCodexFingerprintClientMetadataRaw(body, rawIDs)
	require.NoError(t, err)
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	return decoded, rawDecoded
}

func TestApplyCodexFingerprintPromptCacheKey_MapRawEquivalence(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode)+"/default", func(t *testing.T) {
			account := newTestOAuthAccount(4300, map[string]any{codexFingerprintModeExtraKey: string(mode)})
			ids := resolveCodexFingerprintIDs(account, "header-session", mode)
			require.NotNil(t, ids)

			body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"body-session","client_metadata":{"session_id":" body-session ","trace":"keep"},"input":[]}`)
			mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

			require.Equal(t, mapBody["prompt_cache_key"], rawBody["prompt_cache_key"])
			require.Equal(t, ids.sessionID, mapBody["prompt_cache_key"])
			mapCM, _ := mapBody["client_metadata"].(map[string]any)
			rawCM, _ := rawBody["client_metadata"].(map[string]any)
			require.Equal(t, ids.sessionID, mapCM["session_id"])
			require.Equal(t, mapCM["session_id"], rawCM["session_id"])
			require.Equal(t, "keep", rawCM["trace"])
		})
	}

	t.Run("explicit override", func(t *testing.T) {
		account := newTestOAuthAccount(4301, map[string]any{codexFingerprintModeExtraKey: "session"})
		ids := resolveCodexFingerprintIDs(account, "header-session", codexFingerprintSession)
		require.NotNil(t, ids)

		body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"explicit-cache","client_metadata":{"session_id":"body-session"},"input":[]}`)
		mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

		require.Equal(t, "explicit-cache", mapBody["prompt_cache_key"])
		require.Equal(t, "explicit-cache", rawBody["prompt_cache_key"])
		mapCM, _ := mapBody["client_metadata"].(map[string]any)
		rawCM, _ := rawBody["client_metadata"].(map[string]any)
		require.Equal(t, ids.sessionID, mapCM["session_id"])
		require.Equal(t, ids.sessionID, rawCM["session_id"])
	})
}

func TestApplyCodexFingerprintPromptCacheKey_Negatives(t *testing.T) {
	sessionAccount := newTestOAuthAccount(4310, map[string]any{codexFingerprintModeExtraKey: "session"})
	sessionIDs := resolveCodexFingerprintIDs(sessionAccount, "header-session", codexFingerprintSession)
	require.NotNil(t, sessionIDs)
	deviceAccount := newTestOAuthAccount(4311, map[string]any{codexFingerprintModeExtraKey: "device"})
	deviceIDs := resolveCodexFingerprintIDs(deviceAccount, "header-session", codexFingerprintDevice)
	require.NotNil(t, deviceIDs)

	tests := []struct {
		name          string
		body          []byte
		ids           *codexFingerprintIDs
		wantChanged   bool
		wantExists    bool
		wantCacheKey  any
		wantRawString string
	}{
		{
			name:        "missing key is not injected",
			body:        []byte(`{"client_metadata":{"session_id":"body-session"}}`),
			ids:         sessionIDs,
			wantChanged: true,
			wantExists:  false,
		},
		{
			name:         "empty key preserved",
			body:         []byte(`{"prompt_cache_key":"","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: "",
		},
		{
			name:         "whitespace-different key is an explicit override",
			body:         []byte(`{"prompt_cache_key":" body-session ","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: " body-session ",
		},
		{
			name:         "non-string key preserved",
			body:         []byte(`{"prompt_cache_key":123,"client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: float64(123),
		},
		{
			name:         "missing source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session"}`),
			ids:          sessionIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "non-string source session preserves key",
			body:         []byte(`{"prompt_cache_key":"123","client_metadata":{"session_id":123}}`),
			ids:          sessionIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: "123",
		},
		{
			name:         "non-object source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":"bad"}`),
			ids:          sessionIDs,
			wantChanged:  false,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "device mode preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:          deviceIDs,
			wantChanged:  true,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:          "off mode preserves body",
			body:          []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:           nil,
			wantExists:    true,
			wantCacheKey:  "body-session",
			wantRawString: `{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mapBody map[string]any
			require.NoError(t, json.Unmarshal(tt.body, &mapBody))
			changedMap := applyCodexFingerprintClientMetadata(mapBody, cloneCodexFingerprintIDsForTest(tt.ids))

			rawBody, changedRaw, err := applyCodexFingerprintClientMetadataRaw(tt.body, cloneCodexFingerprintIDsForTest(tt.ids))
			require.NoError(t, err)
			if tt.ids == nil {
				require.False(t, changedMap)
				require.False(t, changedRaw)
				require.JSONEq(t, tt.wantRawString, string(rawBody))
				return
			}
			require.Equal(t, tt.wantChanged, changedMap)
			require.Equal(t, tt.wantChanged, changedRaw)

			rawDecoded := map[string]any{}
			require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
			_, mapExists := mapBody["prompt_cache_key"]
			_, rawExists := rawDecoded["prompt_cache_key"]
			require.Equal(t, tt.wantExists, mapExists)
			require.Equal(t, tt.wantExists, rawExists)
			if tt.wantExists {
				require.Equal(t, tt.wantCacheKey, mapBody["prompt_cache_key"])
				require.Equal(t, tt.wantCacheKey, rawDecoded["prompt_cache_key"])
			}
		})
	}
}

func TestApplyCodexFingerprintNonObjectMetadataDoesNotCapturePromptCacheAnchor(t *testing.T) {
	account := newTestOAuthAccount(4312, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "header-session", codexFingerprintSession)
	require.NotNil(t, ids)
	mapIDs := cloneCodexFingerprintIDsForTest(ids)
	rawIDs := cloneCodexFingerprintIDsForTest(ids)

	nonObjectBody := []byte(`{"prompt_cache_key":"body-session","client_metadata":"bad"}`)
	mapBody := map[string]any{}
	require.NoError(t, json.Unmarshal(nonObjectBody, &mapBody))
	require.False(t, applyCodexFingerprintClientMetadata(mapBody, mapIDs))
	rawBody, changed, err := applyCodexFingerprintClientMetadataRaw(nonObjectBody, rawIDs)
	require.NoError(t, err)
	require.False(t, changed)
	require.JSONEq(t, string(nonObjectBody), string(rawBody))
	require.False(t, mapIDs.originalBodySessionIDCaptured)
	require.False(t, rawIDs.originalBodySessionIDCaptured)

	validBody := []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`)
	mapBody = map[string]any{}
	require.NoError(t, json.Unmarshal(validBody, &mapBody))
	require.True(t, applyCodexFingerprintClientMetadata(mapBody, mapIDs))
	rawBody, changed, err = applyCodexFingerprintClientMetadataRaw(validBody, rawIDs)
	require.NoError(t, err)
	require.True(t, changed)

	rawDecoded := map[string]any{}
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	require.True(t, mapIDs.originalBodySessionIDCaptured)
	require.True(t, rawIDs.originalBodySessionIDCaptured)
	require.Equal(t, mapBody["prompt_cache_key"], rawDecoded["prompt_cache_key"])
	require.Equal(t, ids.sessionID, mapBody["prompt_cache_key"])
}

func TestApplyCodexFingerprintClientMetadataRaw_MatchesMapVariant(t *testing.T) {
	embedded := `{\"installation_id\":\"real-install\",\"session_id\":\"real-session\",\"sandbox\":\"seatbelt\"}`
	bodies := map[string]string{
		"no_client_metadata": `{"model":"gpt-5.6-sol","input":[],"stream":true}`,
		"object_with_extras": `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"client-session","traceparent":"00-abc-def-01","x-codex-turn-metadata":"` + embedded + `"},"stream":true}`,
		"non_object_value":   `{"model":"gpt-5.6-sol","client_metadata":"bogus","stream":true}`,
	}
	for _, mode := range []codexFingerprintMode{codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull} {
		account := newTestOAuthAccount(4242, map[string]any{codexFingerprintModeExtraKey: string(mode)})
		ids := resolveCodexFingerprintIDs(account, "client-sess-raw", mode)
		require.NotNil(t, ids)
		for name, body := range bodies {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				mapCM, rawCM := rawVsMapClientMetadata(t, []byte(body), ids)
				assert.Equal(t, mapCM, rawCM, "raw 字节版与 map 版的 client_metadata 结果必须逐点一致")
			})
		}
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_PreservesUnrelatedFields(t *testing.T) {
	account := newTestOAuthAccount(4243, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-preserve", codexFingerprintSession)
	require.NotNil(t, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"hi"}],"stream":true,"prompt_cache_key":"old","client_metadata":{"session_id":"old"}}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))
	assert.Equal(t, "gpt-5.6-sol", decoded["model"])
	assert.Equal(t, ids.sessionID, decoded["prompt_cache_key"], "session/full convergence must keep body cache key aligned with session_id")
	assert.Equal(t, true, decoded["stream"])
	cm, _ := decoded["client_metadata"].(map[string]any)
	require.NotNil(t, cm)
	assert.Equal(t, ids.sessionID, cm["session_id"])
	assert.Equal(t, ids.turnID, cm["turn_id"])
}

func TestApplyCodexFingerprintClientMetadataRaw_PreservesExplicitPromptCacheOverride(t *testing.T) {
	account := newTestOAuthAccount(4244, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-explicit", codexFingerprintSession)
	require.NotNil(t, ids)

	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"explicit-cache","client_metadata":{"session_id":"client-session"}}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, "explicit-cache", gjson.GetBytes(out, "prompt_cache_key").String())
}

func TestApplyCodexFingerprintClientMetadataRaw_Noop(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, body, out)

	out, changed, err = applyCodexFingerprintClientMetadataRaw(nil, &codexFingerprintIDs{mode: codexFingerprintSession, installationID: "x"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, out)
}

// --- context 暂存与出站头应用（透传/非透传共用 seam）---

func newFingerprintStageTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestStagedCodexFingerprintIDs_RequiresExactAccountOwner(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	selected := newTestOAuthAccount(1000, map[string]any{codexFingerprintModeExtraKey: "device"})

	for _, ids := range []*codexFingerprintIDs{
		{accountID: 0, mode: codexFingerprintDevice, installationID: "missing-owner"},
		{accountID: 999, mode: codexFingerprintDevice, installationID: "wrong-owner"},
	} {
		stageCodexFingerprintIDs(c, ids)
		headers := http.Header{}
		headers.Set("x-codex-turn-metadata", `{"installation_id":"client"}`)
		applyStagedCodexFingerprintHeaders(c, selected, headers)
		assert.Equal(t, "client", gjson.Get(headers.Get("x-codex-turn-metadata"), "installation_id").String())
		assert.Empty(t, stagedCodexFingerprintIDs(c, selected))
	}
}

func TestStagedCodexFingerprintIDs_RejectsChangedModeOrInstallation(t *testing.T) {
	account := newTestOAuthAccount(1001, map[string]any{
		codexFingerprintModeExtraKey: "device",
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	})
	// A snapshot may still be present in an in-flight context after a mode or
	// seed change. The ownership gate must reject it when its official derived
	// installation no longer belongs to the selected account.
	ids := &codexFingerprintIDs{
		accountID:      account.ID,
		mode:           codexFingerprintDevice,
		installationID: resolveConvergedInstallationID(account),
	}

	t.Run("changed mode", func(t *testing.T) {
		changed := *account
		changed.Extra = map[string]any{
			codexFingerprintModeExtraKey: "off",
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
		}
		assert.False(t, codexFingerprintIDsBelongToAccount(ids, &changed))
	})

	t.Run("changed installation", func(t *testing.T) {
		changed := *account
		changed.Extra = map[string]any{
			codexFingerprintModeExtraKey: "device",
			codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222",
		}
		assert.False(t, codexFingerprintIDsBelongToAccount(ids, &changed))
	})

	assert.True(t, codexFingerprintIDsBelongToAccount(ids, account))
}

func TestStagedCodexFingerprintIDs_RejectsInvalidModeAgainstOptOutAccount(t *testing.T) {
	account := newTestOAuthAccount(1004, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintOff),
	})
	stale := &codexFingerprintIDs{
		accountID:      account.ID,
		mode:           codexFingerprintMode("legacy-session"),
		installationID: "stale-installation",
	}
	assert.False(t, codexFingerprintIDsBelongToAccount(stale, account),
		"invalid snapshots must never be treated as an off-mode snapshot")
}

func TestStagedCodexFingerprintIDs_RejectsSeedRotationWithFixedDeviceID(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			account := newTestOAuthAccount(1002, map[string]any{
				codexFingerprintModeExtraKey: string(mode),
				codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
				"openai_device_id":           "fixed-device-id",
			})
			ids := resolveCodexFingerprintIDs(account, "client-session", mode)
			require.NotNil(t, ids)
			require.Equal(t, "fixed-device-id", ids.installationID)
			require.NotEmpty(t, ids.sessionID)
			require.True(t, codexFingerprintIDsBelongToAccount(ids, account))

			rotated := *account
			rotated.Extra = map[string]any{
				codexFingerprintModeExtraKey: string(mode),
				codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222",
				"openai_device_id":           "fixed-device-id",
			}
			// The explicit installation override is unchanged, but the account-wide
			// session projection changed with the seed. A staged snapshot from the
			// previous account state must not cross this boundary.
			assert.False(t, codexFingerprintIDsBelongToAccount(ids, &rotated))
		})
	}
}

func TestStageCodexFingerprintIDs_NilOverwritesPreviousAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1001, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-x", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	// failover 切到 off 模式账号：无条件覆写为 nil，上一账号 IDs 不得残留
	stageCodexFingerprintIDs(c, nil)

	h := http.Header{}
	h.Set("session_id", "isolated-session")
	accountB := newTestOAuthAccount(1002, map[string]any{"codex_fingerprint_mode": "off"})
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "isolated-session", h.Get("session_id"), "off 账号不得应用上一账号的收敛 ID")
	assert.Empty(t, h.Get("x-codex-installation-id"))
}

func TestApplyStagedCodexFingerprintRejectsDifferentOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-a", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	accountB := newTestOAuthAccount(1004, map[string]any{codexFingerprintModeExtraKey: "session"})
	h := make(http.Header)
	h.Set("session-id", "account-b-session")
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "account-b-session", h.Get("session-id"))
	assert.Empty(t, h.Get("x-codex-installation-id"))

	body := map[string]any{"client_metadata": map[string]any{"session_id": "account-b-session"}}
	assert.False(t, applyStagedCodexFingerprintClientMetadata(c, accountB, body))
	clientMetadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "account-b-session", clientMetadata["session_id"])
}

func TestApplyStagedCodexFingerprintHeaders_SkipsNonOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	oauthIDs := resolveCodexFingerprintIDs(newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"}), "sess-y", codexFingerprintSession)
	require.NotNil(t, oauthIDs)
	stageCodexFingerprintIDs(c, oauthIDs)

	h := http.Header{}
	apiKeyAccount := &Account{ID: 1004, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	applyStagedCodexFingerprintHeaders(c, apiKeyAccount, h)
	assert.Empty(t, h.Get("x-codex-installation-id"), "stale 收敛 ID 不得应用到非 OAuth 账号")
}

func TestBuildUpstreamRequestOpenAIPassthrough_AppliesStagedFingerprint(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// 收敛是显式 opt-in（#5610）：显式开启后验证透传路径的出站头收敛。
	account := newTestOAuthAccount(2001, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "device",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session-id", "real-client-session")
	c.Request.Header.Set("thread-id", "real-client-thread")
	c.Request.Header.Set("x-client-request-id", "real-client-request")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1 (Ubuntu 22.4.0; x86_64) xterm-256color")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"real-install","session_id":"real-client-session","thread_id":"real-client-thread","turn_id":"019956a2-a4a1-7000-8000-000000000001","window_id":"real-client-thread:0","sandbox":"seatbelt"}`)

	// 复刻 forwardOpenAIPassthrough 的解析+暂存 seam。
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, nil, 0)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	// Device mode owns only installation_id; a complete client snapshot keeps
	// its canonical session/thread/request lifecycle unchanged.
	assert.Equal(t, "real-client-session", req.Header.Get("session-id"))
	assert.Equal(t, "real-client-thread", req.Header.Get("thread-id"))
	assert.Equal(t, ids.installationID, req.Header.Get("x-codex-installation-id"),
		"regular passthrough keeps the transport installation projection aligned")
	assert.Equal(t, "real-client-request", req.Header.Get("x-client-request-id"))
	turnMetadata := req.Header.Get("x-codex-turn-metadata")
	require.NotEmpty(t, turnMetadata)
	assert.Contains(t, turnMetadata, ids.installationID, "turn-metadata 中 installation 应被收敛")
	assert.Contains(t, turnMetadata, `"sandbox":"seatbelt"`, "turn-metadata 未指定字段应原样保留")
}

func TestBuildUpstreamRequestOpenAIPassthrough_OffModeKeepsIsolatedSession(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2002, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "off",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session_id", "real-client-session")
	c.Request.Header.Set("originator", "codex_cli_rs")

	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.Nil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	assert.NotEmpty(t, req.Header.Get("session_id"))
	assert.NotEqual(t, resolveConvergedSessionID(testCodexFingerprintSeed), req.Header.Get("session_id"), "off 模式不得收敛 session_id")
	assert.Empty(t, req.Header.Get("x-codex-window-id"))
}

func TestCompactPathDoesNotApplyCodexFingerprintProjection(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("x-codex-turn-state", "foreign-account-turn-state")
	account := newTestOAuthAccount(2003, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintDevice),
	})
	ids := resolveCodexFingerprintIDsForRequest(account, c.Request.Header, []byte(`{"prompt_cache_key":"root-session"}`), 0)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	// buildUpstreamRequest has already run guardOpenAICodexTurnStateEcho and
	// removed the foreign state before this compact projection is reached.
	headers := http.Header{}
	// The compact transport deliberately bypasses the four convergence modes.
	// Its generic session handling is tested by the compact gateway tests.
	applyStagedCodexCompactHeaders(c, account, headers, []byte(`{"prompt_cache_key":"root-session"}`))

	require.Empty(t, headers.Get("x-codex-turn-state"))
	require.Empty(t, headers.Get("x-codex-installation-id"))
	require.Empty(t, headers.Get("session-id"))
	require.Empty(t, headers.Get("thread-id"))
}

func TestApplyCodexFingerprintClientMetadataRaw_NonObjectBodyUntouched(t *testing.T) {
	account := newTestOAuthAccount(4244, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-nonobj", codexFingerprintSession)
	require.NotNil(t, ids)

	for _, body := range []string{`[1,2,3]`, `"plain string"`, `not json at all`} {
		out, changed, err := applyCodexFingerprintClientMetadataRaw([]byte(body), ids)
		require.NoError(t, err)
		assert.False(t, changed, "非 JSON 对象 body 不应被改写: %s", body)
		assert.Equal(t, []byte(body), out)
	}
}
