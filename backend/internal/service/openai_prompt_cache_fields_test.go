//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func promptCacheAPIKeyAccount(capability bool) *Account {
	credentials := map[string]any{"api_key": "sk-test", "base_url": "https://relay.example.test/v1"}
	if capability {
		credentials[openAIEndpointCapabilitiesCredentialKey] = []string{
			string(OpenAIEndpointCapabilityChatCompletions),
			string(OpenAIEndpointCapabilityPromptCacheRetention),
		}
	}
	return &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: credentials}
}

func TestNormalizeOpenAIPromptCacheFieldsForEgress_DefaultDenyAndModelShapes(t *testing.T) {
	tests := []struct {
		name          string
		account       *Account
		model         string
		wantRetention bool
		wantOptions   bool
	}{
		{
			name:          "legacy without explicit capability strips both",
			account:       promptCacheAPIKeyAccount(false),
			model:         "gpt-5.5",
			wantRetention: false,
			wantOptions:   false,
		},
		{
			name:          "legacy with explicit capability keeps retention only",
			account:       promptCacheAPIKeyAccount(true),
			model:         "gpt-5.5-2026-08-01",
			wantRetention: true,
			wantOptions:   false,
		},
		{
			name:          "gpt 5.6 keeps options only",
			account:       promptCacheAPIKeyAccount(true),
			model:         "gpt-5.6-2026-08-01",
			wantRetention: false,
			wantOptions:   true,
		},
		{
			name:          "unknown model strips both",
			account:       promptCacheAPIKeyAccount(true),
			model:         "gpt-5.6-custom",
			wantRetention: false,
			wantOptions:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"placeholder","prompt_cache_retention":"24h","prompt_cache_options":{"enabled":true},"prompt_cache_key":"keep"}`)
			normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgressWithModel(body, tt.account, "https://relay.example.test/v1/responses", tt.model, "")
			require.NoError(t, err)
			require.True(t, changed)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(normalized, &decoded))
			_, retentionPresent := decoded["prompt_cache_retention"]
			_, optionsPresent := decoded["prompt_cache_options"]
			require.Equal(t, tt.wantRetention, retentionPresent)
			require.Equal(t, tt.wantOptions, optionsPresent)
			require.Equal(t, "keep", decoded["prompt_cache_key"])
		})
	}
}

func TestNormalizeOpenAIPromptCacheFieldsForEgressScopesOAuthAndProviders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","prompt_cache_retention":"24h","prompt_cache_options":{"enabled":true},"prompt_cache_key":"keep"}`)
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, target := range []string{
		"https://chatgpt.com/backend-api/codex/responses",
		"https://relay.example.test/v1/responses",
	} {
		normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgress(body, oauth, target)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, normalized)
	}

	for _, account := range []*Account{
		{Platform: PlatformGrok, Type: AccountTypeAPIKey},
		{Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
		nil,
	} {
		normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgress(body, account, "https://api.openai.com/v1/responses")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, normalized)
	}
}

func TestNormalizeOpenAIPromptCacheFieldsForEgressUsesSessionModel(t *testing.T) {
	account := promptCacheAPIKeyAccount(true)
	body := []byte(`{"type":"session.update","session":{"model":"gpt-5.5","prompt_cache_retention":"24h","prompt_cache_options":{"enabled":true},"prompt_cache_key":"keep"}}`)
	normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgressWithModel(
		body,
		account,
		"wss://relay.example.test/v1/responses",
		"",
		"gpt-5.5-2026-08-01",
	)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(normalized, &decoded))
	session, ok := decoded["session"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, session, "prompt_cache_retention")
	require.NotContains(t, session, "prompt_cache_options")
	require.Equal(t, "keep", session["prompt_cache_key"])
}

func TestNormalizeOpenAIPromptCacheFieldsForEgressUsesResolvedModelWhenFrameOmitsModel(t *testing.T) {
	account := promptCacheAPIKeyAccount(true)
	body := []byte(`{"type":"response.create","prompt_cache_retention":"24h","prompt_cache_options":{"enabled":true}}`)
	normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgressWithModel(
		body,
		account,
		"wss://relay.example.test/v1/responses",
		"gpt-5.6-sol",
		"gpt-5.6-sol",
	)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(normalized, &decoded))
	require.NotContains(t, decoded, "prompt_cache_retention")
	require.Contains(t, decoded, "prompt_cache_options")
}

func TestOpenAIPromptCacheModelSuffixValidation(t *testing.T) {
	for _, model := range []string{"gpt-5.6", "gpt-5.6-2026-08-01", "gpt-5.6-sol", "gpt-6.0"} {
		require.True(t, isOpenAIPromptCacheGPT56OrLater(model), model)
	}
	for _, model := range []string{"gpt-5.6-codex", "gpt-5.6-garbage", "gpt-5.6-2026-8-01", "gpt-5.6--2026-08-01"} {
		require.False(t, isOpenAIPromptCacheGPT56OrLater(model), model)
	}
}
