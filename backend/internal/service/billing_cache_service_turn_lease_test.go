package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBillingCacheServiceSubscriptionUsageTurnLeaseLocalFallback(t *testing.T) {
	svc := NewBillingCacheService(&billingCacheWorkerStub{}, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)

	first, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 41, 59)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, first)

	second, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 41, 59)
	require.NoError(t, err)
	require.False(t, acquired)
	require.Nil(t, second)

	first.Release()
	third, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 41, 59)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, third)
	third.Release()
}

func TestBillingCacheServiceSubscriptionUsageTurnLeaseSeparatesSubscriptions(t *testing.T) {
	svc := NewBillingCacheService(&billingCacheWorkerStub{}, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)

	first, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 41, 59)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(first.Release)

	otherGroup, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 41, 60)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(otherGroup.Release)

	otherUser, acquired, err := svc.AcquireSubscriptionUsageTurnLease(context.Background(), 42, 59)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(otherUser.Release)
}
