package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexFingerprintRecoveryMarkerOnlyClearsOnExplicitModeEdit(t *testing.T) {
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey:             string(codexFingerprintDevice),
			codexFingerprintSeedExtraKey:             "11111111-1111-4111-8111-111111111111",
			CodexFingerprintRecoveryRequiredExtraKey: true,
			"unrelated_setting":                      "keep",
		},
	}

	unchanged := NormalizeCodexFingerprintExtraForExistingAccount(account, map[string]any{
		"unrelated_setting": "updated",
	})
	require.True(t, IsCodexFingerprintRecoveryRequired(unchanged))

	acknowledged := AcknowledgeCodexFingerprintModeEdit(NormalizeCodexFingerprintExtraForExistingAccount(account, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintFull),
	}))
	require.False(t, IsCodexFingerprintRecoveryRequired(acknowledged))
	require.Equal(t, string(codexFingerprintFull), acknowledged[codexFingerprintModeExtraKey])
	require.Equal(t, "11111111-1111-4111-8111-111111111111", acknowledged[codexFingerprintSeedExtraKey])
}

func TestCodexFingerprintModeUpdateRequestedRequiresModeKey(t *testing.T) {
	require.False(t, codexFingerprintModeUpdateRequested(map[string]any{"name": "renamed"}))
	require.True(t, codexFingerprintModeUpdateRequested(map[string]any{
		codexFingerprintModeExtraKey: nil,
	}))
}

func TestCodexFingerprintExplicitNullModeAcknowledgesMarker(t *testing.T) {
	extra := map[string]any{
		codexFingerprintModeExtraKey:             nil,
		CodexFingerprintRecoveryRequiredExtraKey: true,
	}
	got := AcknowledgeCodexFingerprintModeEdit(extra)
	require.False(t, IsCodexFingerprintRecoveryRequired(got))
	require.NotContains(t, got, codexFingerprintModeExtraKey)
}

func TestCodexFingerprintRecoveryMarkerIsRemovedFromNonOAuthState(t *testing.T) {
	got := NormalizeCodexFingerprintExtraForAccount(PlatformOpenAI, AccountTypeAPIKey, map[string]any{
		codexFingerprintModeExtraKey:             string(codexFingerprintDevice),
		CodexFingerprintRecoveryRequiredExtraKey: true,
		"keep":                                   "value",
	})
	require.Equal(t, "value", got["keep"])
	require.NotContains(t, got, codexFingerprintModeExtraKey)
	require.NotContains(t, got, CodexFingerprintRecoveryRequiredExtraKey)
}
