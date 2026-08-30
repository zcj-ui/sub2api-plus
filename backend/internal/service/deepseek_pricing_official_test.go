//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeepseekPeakMultiplierAtOfficialWindows(t *testing.T) {
	weekday := func(hour, minute int) time.Time {
		return time.Date(2026, 8, 24, hour, minute, 0, 0, time.UTC) // Monday
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want float64
	}{
		{name: "first peak boundary", at: weekday(1, 0), want: 2},
		{name: "first peak end", at: weekday(4, 0), want: 1},
		{name: "second peak boundary", at: weekday(6, 0), want: 2},
		{name: "second peak end", at: weekday(10, 0), want: 1},
		{name: "weekday off peak", at: weekday(12, 0), want: 1},
		{name: "Beijing Saturday is always off peak", at: time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deepseekPeakMultiplierAt(tc.at))
		})
	}
}

func TestCalculateCostUnifiedDeepseekAppliesPeakOnlyToDefaultPricing(t *testing.T) {
	bs := newTestBillingService()
	resolver := NewModelPricingResolver(nil, bs)
	tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 1000}
	offPeak, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "deepseek-v4-flash", Tokens: tokens,
		RateMultiplier: 1, Resolver: resolver,
		PricingAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	peak, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "deepseek-v4-flash", Tokens: tokens,
		RateMultiplier: 1, Resolver: resolver,
		PricingAt: time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.InDelta(t, offPeak.TotalCost*2, peak.TotalCost, 1e-12)

	custom := &ModelPricing{InputPricePerToken: 1e-6, OutputPricePerToken: 2e-6, CacheReadPricePerToken: 3e-8}
	customResult := bs.applyModelSpecificPricingPolicyEx("deepseek-v4-flash", custom, false)
	require.Equal(t, custom, customResult, "custom/group pricing must not be overwritten by official defaults")
}

func TestGetModelPricingUnknownDeepseekUsesFlashOfficialFallback(t *testing.T) {
	bs := newTestBillingService()
	pricing, err := bs.GetModelPricing("deepseek-future-model")
	require.NoError(t, err)
	require.InDelta(t, deepseekFlashOffPeakInputPrice, pricing.InputPricePerToken, 1e-15)
	require.InDelta(t, deepseekFlashOffPeakOutputPrice, pricing.OutputPricePerToken, 1e-15)
	require.InDelta(t, deepseekFlashOffPeakCacheRead, pricing.CacheReadPricePerToken, 1e-15)
}
