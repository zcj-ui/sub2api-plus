package service

import (
	"context"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAdminAccountUserAgentOverrideCreateAndClear(t *testing.T) {
	const id int64 = 901
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		id: {
			ID:          id,
			Name:        "oauth-account",
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Credentials: map[string]any{"chatgpt_account_id": "acct-test", "user_agent": "old"},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), id, &UpdateAccountInput{
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-test",
			"user_agent":         "  codex-tui/0.146.0 (Linux; x86_64) bash  ",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "codex-tui/0.146.0 (Linux; x86_64) bash", updated.GetOpenAIUserAgent())

	cleared, err := svc.UpdateAccount(context.Background(), id, &UpdateAccountInput{
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-test",
			"user_agent":         "   ",
		},
	})
	require.NoError(t, err)
	require.Empty(t, cleared.GetOpenAIUserAgent(), "blank override must clear the account key")
}

func TestAdminBulkUserAgentRejectsMixedAccounts(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1, 2},
		Credentials: map[string]any{"user_agent": "codex-tui/0.146.0"},
	})
	require.Error(t, err)
	require.Equal(t, "OPENAI_USER_AGENT_ACCOUNT_INVALID", infraerrors.Reason(err))
}
