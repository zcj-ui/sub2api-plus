//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func openAIModelRoutingTestAccount(id int64, groupID int64, priority int) Account {
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{"api_key": "test-key"},
	}
}

func openAIModelRoutingTestContext(groupID int64, model string, route []int64) context.Context {
	group := &Group{
		ID:                  groupID,
		Platform:            PlatformOpenAI,
		Status:              StatusActive,
		Hydrated:            true,
		ModelRoutingEnabled: true,
		ModelRouting:        map[string][]int64{model: route},
	}
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

func TestOpenAIModelRouting_SelectsRoutedAccountAndReplacesOutsideSticky(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(58620)
	model := "gpt-5.6-sol"
	accounts := []Account{
		openAIModelRoutingTestAccount(4, groupID, 10),
		// Give the non-routed sticky account the better priority to prove the
		// route is applied before ordinary sticky/load ranking.
		openAIModelRoutingTestAccount(117, groupID, 0),
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:route-session": 117}}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	routingCtx := openAIModelRoutingTestContext(groupID, model, []int64{4})
	require.Equal(t, []int64{4}, svc.openAIModelRoutingAccountIDs(routingCtx, &groupID, model, PlatformOpenAI))

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		routingCtx,
		&groupID,
		"",
		"route-session",
		model,
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		false,
		false,
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(4), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, int64(4), cache.sessionBindings["openai:route-session"], "sticky binding outside the route must be replaced")
}

func TestOpenAIModelRoutingFallsBackWhenRoutedAccountUnavailable(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(58621)
	model := "gpt-5.6-sol"
	resetAt := time.Now().Add(time.Hour)
	routed := openAIModelRoutingTestAccount(4, groupID, 0)
	routed.RateLimitResetAt = &resetAt
	fallback := openAIModelRoutingTestAccount(117, groupID, 10)
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{routed, fallback}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		openAIModelRoutingTestContext(groupID, model, []int64{4}),
		&groupID,
		"",
		"",
		model,
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		false,
		false,
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(117), selection.Account.ID, "an unavailable routed account must fail open to the normal pool")
}

func TestOpenAIModelRoutingDoesNotAffectOtherPlatforms(t *testing.T) {
	groupID := int64(58622)
	group := &Group{
		ID:                  groupID,
		Platform:            PlatformAnthropic,
		Status:              StatusActive,
		Hydrated:            true,
		ModelRoutingEnabled: true,
		ModelRouting:        map[string][]int64{"claude-opus": {4}},
	}
	svc := &OpenAIGatewayService{}
	require.Empty(t, svc.openAIModelRoutingAccountIDs(context.WithValue(context.Background(), ctxkey.Group, group), &groupID, "claude-opus", PlatformAnthropic))
}
