package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newBillingCacheTurnLeaseTestCache(t *testing.T) *billingCache {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	return &billingCache{rdb: rdb}
}

func TestBillingCacheSubscriptionUsageTurnLeaseOwnerIsolation(t *testing.T) {
	cache := newBillingCacheTurnLeaseTestCache(t)
	ctx := context.Background()

	acquired, err := cache.AcquireSubscriptionUsageTurnLease(ctx, 7, 9, "owner-a", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)

	acquired, err = cache.AcquireSubscriptionUsageTurnLease(ctx, 7, 9, "owner-b", time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	owned, err := cache.RefreshSubscriptionUsageTurnLease(ctx, 7, 9, "owner-b", time.Minute)
	require.NoError(t, err)
	require.False(t, owned)
	require.NoError(t, cache.ReleaseSubscriptionUsageTurnLease(ctx, 7, 9, "owner-b"))

	owned, err = cache.RefreshSubscriptionUsageTurnLease(ctx, 7, 9, "owner-a", time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, cache.ReleaseSubscriptionUsageTurnLease(ctx, 7, 9, "owner-a"))

	acquired, err = cache.AcquireSubscriptionUsageTurnLease(ctx, 7, 9, "owner-b", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
}
