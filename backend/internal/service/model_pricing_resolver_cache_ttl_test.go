//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelPricingResolver_PreservesOneHourOnlyIntervals(t *testing.T) {
	resolver := &ModelPricingResolver{}
	channel := &ChannelModelPricing{Intervals: []PricingInterval{{MinTokens: 0, CacheWrite1hPrice: testPtrFloat64(21e-6)}}}
	resolved := &ResolvedPricing{BasePricing: &ModelPricing{CacheCreation5mPrice: 13e-6}}
	resolver.applyTokenOverrides(channel, resolved)
	require.Len(t, resolved.Intervals, 1, "a one-hour-only price interval is not empty")
	require.True(t, resolved.SupportsCacheBreakdown)
	pricing := resolver.GetIntervalPricing(resolved, 100)
	require.True(t, pricing.SupportsCacheBreakdown)
	require.InDelta(t, 21e-6, pricing.CacheCreation1hPrice, 1e-12)
	require.InDelta(t, 13e-6, pricing.CacheCreation5mPrice, 1e-12)
}
