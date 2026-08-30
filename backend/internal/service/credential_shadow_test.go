package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubCredRepo 是最小化 AccountRepository stub，仅实现 GetByID，供 credential_shadow_test 使用。
// 嵌入接口满足完整方法集；未实现的方法若被调用会 panic，从而快速暴露误调用。
type stubCredRepo struct {
	AccountRepository
	parent *Account
}

func (s *stubCredRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.parent, nil
}

func newStubCredRepo(parent *Account) AccountRepository {
	return &stubCredRepo{parent: parent}
}

func TestResolveCredentialAccount(t *testing.T) {
	ctx := context.Background()
	pid := int64(100)

	// 普通账号（非影子）→ 返回自身
	parent := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}
	repo := newStubCredRepo(parent)
	got, err := resolveCredentialAccount(ctx, repo, parent)
	require.NoError(t, err)
	require.Equal(t, int64(100), got.ID)

	// 影子账号 + 合法 OpenAI OAuth 母账号 → 返回母账号
	shadow := &Account{ID: 200, ParentAccountID: &pid, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	got, err = resolveCredentialAccount(ctx, repo, shadow)
	require.NoError(t, err)
	require.Equal(t, int64(100), got.ID)

	// 影子账号 + 母账号非 OpenAI OAuth（API Key 类型）→ 返回 error
	badRepo := newStubCredRepo(&Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeAPIKey})
	_, err = resolveCredentialAccount(ctx, badRepo, shadow)
	require.Error(t, err)
}

func TestInheritOpenAIShadowUpstreamProfileKeepsShadowRoutingState(t *testing.T) {
	parentID := int64(10)
	shadow := &Account{
		ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ParentAccountID: &parentID,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.3-codex-spark": "gpt-5.3-codex-spark"},
			"access_token":  "stale-shadow-token",
		},
		Extra: map[string]any{
			"openai_ws_force_http":  true,
			"codex_7d_used_percent": 35.0,
			PlatformOpenAI:          map[string]any{"codex_image_generation_bridge": false, "shadow_local": true},
		},
		QuotaDimension: QuotaDimensionSpark,
	}
	parent := &Account{
		ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ProxyID: func() *int64 { v := int64(77); return &v }(),
		Proxy:   &Proxy{ID: 77, Host: "proxy.example.test", Port: 443},
		Credentials: map[string]any{
			"access_token":  "parent-token",
			"base_url":      "https://parent.example.test",
			"model_mapping": map[string]any{"parent": "mapping"},
		},
		Extra: map[string]any{
			"openai_ws_force_http":   false,
			"openai_device_id":       "parent-installation",
			"codex_fingerprint_mode": "session",
			PlatformOpenAI:           map[string]any{"codex_image_generation_bridge": true, "parent_only": true},
		},
	}

	got := InheritOpenAIShadowUpstreamProfile(shadow, parent)
	require.NotNil(t, got)
	require.Equal(t, shadow.ID, got.ID)
	require.Equal(t, QuotaDimensionSpark, got.QuotaDimension)
	require.Equal(t, "parent-token", got.Credentials["access_token"])
	require.Equal(t, "https://parent.example.test", got.Credentials["base_url"])
	require.NotNil(t, got.ProxyID)
	require.Equal(t, int64(77), *got.ProxyID)
	require.Equal(t, int64(77), got.Proxy.ID)
	require.Equal(t, shadow.Credentials["model_mapping"], got.Credentials["model_mapping"])
	require.NotContains(t, got.Credentials, "compact_model_mapping")
	require.Equal(t, "parent-installation", got.Extra["openai_device_id"])
	require.Equal(t, "session", got.Extra["codex_fingerprint_mode"])
	require.Equal(t, 35.0, got.Extra["codex_7d_used_percent"])
	nested, ok := got.Extra[PlatformOpenAI].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, nested["codex_image_generation_bridge"])
	require.Equal(t, true, nested["shadow_local"])
	require.Nil(t, nested["parent_only"], "unrelated parent nested state must not leak into shadow")

	// Inputs must remain untouched; the projection is never persisted.
	require.Equal(t, "stale-shadow-token", shadow.Credentials["access_token"])
	require.Equal(t, true, shadow.Extra["openai_ws_force_http"])
	shadowNested, ok := shadow.Extra[PlatformOpenAI].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, shadowNested["codex_image_generation_bridge"])
	require.Nil(t, InheritOpenAIShadowUpstreamProfile(nil, parent))
	require.Nil(t, InheritOpenAIShadowUpstreamProfile(shadow, nil))
}

func TestEffectiveOpenAIShadowUpstreamProfileRequiresRealOAuthParent(t *testing.T) {
	parentID := int64(10)
	shadow := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID}
	require.Nil(t, effectiveOpenAIShadowUpstreamProfile(shadow, func(int64) *Account { return nil }))
	require.Nil(t, effectiveOpenAIShadowUpstreamProfile(shadow, func(int64) *Account {
		return &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	}))
	got := effectiveOpenAIShadowUpstreamProfile(shadow, func(int64) *Account {
		return &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	})
	require.NotNil(t, got)
	require.Equal(t, shadow.ID, got.ID)
}
