package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestStripChatGPTInternalUnsupportedFieldsAtEgressIsOAuthScoped(t *testing.T) {
	oauth := newOpenAIOAuthNamespaceTestAccount()
	body := []byte(`{"model":"gpt-5.6","input":[{"content":{"metadata":"preserve"}}]}`)
	for _, field := range openAIChatGPTInternalUnsupportedFields {
		var err error
		body, err = sjson.SetBytes(body, field, map[string]any{"probe": field})
		require.NoError(t, err)
	}

	out, stripped := stripChatGPTInternalUnsupportedFieldsAtEgress(oauth, body)
	require.ElementsMatch(t, openAIChatGPTInternalUnsupportedFields, stripped)
	for _, field := range openAIChatGPTInternalUnsupportedFields {
		require.False(t, gjson.GetBytes(out, field).Exists(), "top-level %s should be removed", field)
	}
	// Nested user content is not touched by a top-level egress cleanup.
	require.Equal(t, "preserve", gjson.GetBytes(out, "input.0.content.metadata").String())

	apiKey := newOpenAIRejectedFieldTestAccount()
	apiBody := []byte(`{"model":"gpt-5.5","prompt_cache_retention":"24h","user":"u1"}`)
	kept, apiStripped := stripChatGPTInternalUnsupportedFieldsAtEgress(apiKey, apiBody)
	require.Empty(t, apiStripped)
	require.Equal(t, apiBody, kept)

	kept, nilStripped := stripChatGPTInternalUnsupportedFieldsAtEgress(nil, apiBody)
	require.Empty(t, nilStripped)
	require.Equal(t, apiBody, kept)
}

func TestStripChatGPTInternalUnsupportedFieldsAtEgressUsesSingleTopLevelSource(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":"hi"}`)
	for _, field := range openAIChatGPTInternalUnsupportedFields {
		var err error
		body, err = sjson.SetBytes(body, field, map[string]any{"probe": field})
		require.NoError(t, err)
	}
	out, stripped := stripChatGPTInternalUnsupportedFieldsAtEgress(newOpenAIOAuthNamespaceTestAccount(), body)
	require.ElementsMatch(t, openAIChatGPTInternalUnsupportedFields, stripped)
	require.Equal(t, "gpt-5.6", gjson.GetBytes(out, "model").String())
	require.Equal(t, "hi", gjson.GetBytes(out, "input").String())
}

func TestApplyChatGPTInternalEgressStripLeavesBodyWhenNoFieldMatches(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":"hi"}`)
	require.Equal(t, body, applyChatGPTInternalEgressStrip(context.Background(), newOpenAIOAuthNamespaceTestAccount(), body, openAIEgressForward))
}

func TestStripChatGPTInternalUnsupportedFieldsAtEgress_StripsOnlyDisabledTruncation(t *testing.T) {
	oauth := newOpenAIOAuthNamespaceTestAccount()

	disabled := []byte(`{"model":"gpt-5.4","truncation":"disabled","input":"hi"}`)
	strippedDisabled, fields := stripChatGPTInternalUnsupportedFieldsAtEgress(oauth, disabled)
	require.Contains(t, fields, "truncation")
	require.False(t, gjson.GetBytes(strippedDisabled, "truncation").Exists())

	auto := []byte(`{"model":"gpt-5.4","truncation":"auto","input":"hi"}`)
	strippedAuto, fields := stripChatGPTInternalUnsupportedFieldsAtEgress(oauth, auto)
	require.NotContains(t, fields, "truncation")
	require.Equal(t, "auto", gjson.GetBytes(strippedAuto, "truncation").String())

	// The final ChatGPT-only guard must not rewrite API-key or Claude routes.
	for _, account := range []*Account{
		newOpenAIRejectedFieldTestAccount(),
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
	} {
		kept, untouched := stripChatGPTInternalUnsupportedFieldsAtEgress(account, disabled)
		require.Empty(t, untouched)
		require.Equal(t, disabled, kept)
	}
}

func TestChatGPTInternalEgressGuardDoesNotTouchOAuthRelay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","prompt_cache_retention":"24h","metadata":{"k":"v"}}`)
	relayAccount := newOpenAIOAuthNamespaceTestAccount()
	relayAccount.Credentials["base_url"] = "https://relay.example.test/v1"

	kept, stripped := stripChatGPTInternalUnsupportedFieldsAtEgressForURL(
		relayAccount, "https://relay.example.test/v1/responses", body,
	)
	require.Empty(t, stripped)
	require.Equal(t, body, kept)

	official, stripped := stripChatGPTInternalUnsupportedFieldsAtEgressForURL(
		newOpenAIOAuthNamespaceTestAccount(), chatgptCodexURL, body,
	)
	require.Contains(t, stripped, "prompt_cache_retention")
	require.False(t, gjson.GetBytes(official, "prompt_cache_retention").Exists())
}
