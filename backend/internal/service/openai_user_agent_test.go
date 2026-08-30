package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIUserAgentCredentials(t *testing.T) {
	t.Run("trims and preserves oauth override", func(t *testing.T) {
		creds := map[string]any{"user_agent": "  codex-tui/0.146.0 (Linux; x86_64) bash  "}
		require.NoError(t, NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeOAuth, creds))
		require.Equal(t, "codex-tui/0.146.0 (Linux; x86_64) bash", creds["user_agent"])
	})

	t.Run("empty clears override", func(t *testing.T) {
		creds := map[string]any{"user_agent": "   ", "email": "user@example.test"}
		require.NoError(t, NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeOAuth, creds))
		_, exists := creds["user_agent"]
		require.False(t, exists)
		require.Equal(t, "user@example.test", creds["email"])
	})

	t.Run("null clears override", func(t *testing.T) {
		creds := map[string]any{"user_agent": nil, "email": "user@example.test"}
		require.NoError(t, NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeOAuth, creds))
		_, exists := creds["user_agent"]
		require.False(t, exists)
	})

	t.Run("rejects control characters", func(t *testing.T) {
		creds := map[string]any{"user_agent": "codex\r\nInjected: true"}
		err := NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeOAuth, creds)
		require.Error(t, err)
		require.Contains(t, err.Error(), "control")
	})

	t.Run("rejects oversized values", func(t *testing.T) {
		creds := map[string]any{"user_agent": strings.Repeat("x", OpenAIAccountUserAgentMaxBytes+1)}
		require.Error(t, NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeOAuth, creds))
	})

	t.Run("ignores non-oauth account types", func(t *testing.T) {
		creds := map[string]any{"user_agent": "  custom  "}
		require.NoError(t, NormalizeOpenAIUserAgentCredentials(PlatformOpenAI, AccountTypeAPIKey, creds))
		require.Equal(t, "  custom  ", creds["user_agent"])
	})
}
