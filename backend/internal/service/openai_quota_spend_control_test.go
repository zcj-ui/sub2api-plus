package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAISpendControl_UnmarshalNumberAndStringShapes(t *testing.T) {
	var usage OpenAIQuotaUsage
	err := json.Unmarshal([]byte(`{
		"plan_type":"team",
		"spend_control":{
			"reached":"false",
			"individual_limit":{
				"source":"workspace_spend_controls",
				"limit":25000,
				"used":"8000",
				"remaining":17000,
				"used_percent":"32",
				"remaining_percent":68,
				"reset_after_seconds":"43200",
				"reset_at":"1780000000"
			}
		}
	}`), &usage)
	require.NoError(t, err)
	require.NotNil(t, usage.SpendControl)
	require.False(t, usage.SpendControl.Reached)
	limit := usage.SpendControl.IndividualLimit
	require.NotNil(t, limit)
	require.Equal(t, "workspace_spend_controls", limit.Source)
	require.Equal(t, "25000", limit.Limit)
	require.Equal(t, "8000", limit.Used)
	require.Equal(t, "17000", limit.Remaining)
	require.Equal(t, float64(32), limit.UsedPercent)
	require.Equal(t, float64(68), limit.RemainingPercent)
	require.Equal(t, int64(43200), limit.ResetAfterSeconds)
	require.Equal(t, int64(1780000000), limit.ResetAt)
}

func TestOpenAISpendControl_UnparseableOptionalLimitDoesNotInvalidateEnvelope(t *testing.T) {
	var usage OpenAIQuotaUsage
	require.NoError(t, json.Unmarshal([]byte(`{
		"spend_control":{"reached":false,"individual_limit":"provider-specific"},
		"credits":{"has_credits":true,"balance":"25"}
	}`), &usage))
	require.NotNil(t, usage.SpendControl)
	require.Nil(t, usage.SpendControl.IndividualLimit)
	require.True(t, hasStructurallyValidOpenAIQuotaPayload(&usage))
}

func TestOpenAIQuotaPayloadAcceptsCreditsOnlyWorkspaceResponse(t *testing.T) {
	account := &Account{
		ID:       901,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "workspace-901",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "token-901",
	}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/backend-api/wham/rate-limit-reset-credits" {
			_, _ = w.Write([]byte(`{"available_count":0,"credits":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"plan_type":"team",
			"credits":{"has_credits":true,"balance":"125.00"},
			"spend_control":{"reached":false,"individual_limit":{"limit":"1000","used":"250","remaining_percent":75,"reset_at":1780000000}}
		}`))
	}))
	defer server.Close()

	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(server))
	usage, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.Credits)
	require.Equal(t, "125.00", usage.Credits.Balance)
	require.NotNil(t, usage.SpendControl)
	require.NotNil(t, usage.SpendControl.IndividualLimit)
	require.Equal(t, "1000", usage.SpendControl.IndividualLimit.Limit)
	require.True(t, hasStructurallyValidOpenAIQuotaPayload(usage))
}

func TestOpenAIQuotaPayloadRejectsCompletelyEmptyResponse(t *testing.T) {
	var usage OpenAIQuotaUsage
	require.NoError(t, json.Unmarshal([]byte(`{}`), &usage))
	require.False(t, hasStructurallyValidOpenAIQuotaPayload(&usage))
}

func TestBuildOpenAIQuotaExtraUpdatesPersistsSpendControlSnapshot(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	usage := &OpenAIQuotaUsage{
		SpendControl: &OpenAISpendControl{
			Reached: true,
			IndividualLimit: &OpenAISpendControlLimit{
				Limit: "1000",
				Used:  "1000",
			},
		},
	}
	updates := buildOpenAIQuotaExtraUpdates(account, usage, time.Now().UTC())
	require.Contains(t, updates, OpenAIQuotaSpendControlExtraKey)
	snapshot, ok := updates[OpenAIQuotaSpendControlExtraKey].(*OpenAISpendControl)
	require.True(t, ok)
	require.True(t, snapshot.Reached)
	require.Equal(t, "1000", snapshot.IndividualLimit.Used)
	observedAt, ok := updates[OpenAICodexUsageObservedAtUnixNanoExtraKey].(int64)
	require.True(t, ok)
	require.Positive(t, observedAt)
}
