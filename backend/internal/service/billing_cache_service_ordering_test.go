package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// subscriptionOrderingCache blocks the initial snapshot write. With the old
// multi-worker queue, the usage update can run while that write is blocked.
type subscriptionOrderingCache struct {
	billingCacheWorkerStub

	setStarted    chan struct{}
	releaseSet    chan struct{}
	updateStarted chan struct{}
	setOnce       sync.Once
	updateOnce    sync.Once
	mu            sync.Mutex
	events        []string
}

func newSubscriptionOrderingCache() *subscriptionOrderingCache {
	return &subscriptionOrderingCache{
		setStarted:    make(chan struct{}),
		releaseSet:    make(chan struct{}),
		updateStarted: make(chan struct{}),
	}
}

func (c *subscriptionOrderingCache) SetSubscriptionCache(ctx context.Context, userID, groupID int64, data *SubscriptionCacheData) error {
	c.setOnce.Do(func() { close(c.setStarted) })
	select {
	case <-c.releaseSet:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	c.events = append(c.events, "set")
	c.mu.Unlock()
	return nil
}

func (c *subscriptionOrderingCache) UpdateSubscriptionUsage(ctx context.Context, userID, groupID int64, cost float64) error {
	c.updateOnce.Do(func() { close(c.updateStarted) })
	c.mu.Lock()
	c.events = append(c.events, "update")
	c.mu.Unlock()
	return nil
}

func TestBillingCacheServiceSubscriptionWritesPreserveFIFO(t *testing.T) {
	cache := newSubscriptionOrderingCache()
	svc := NewBillingCacheService(cache, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(func() {
		select {
		case <-cache.releaseSet:
		default:
			close(cache.releaseSet)
		}
		svc.Stop()
	})

	setTask := cacheWriteTask{
		kind:             cacheWriteSetSubscription,
		userID:           7,
		groupID:          11,
		subscriptionData: &subscriptionCacheData{Status: "active"},
	}
	updateTask := cacheWriteTask{
		kind:    cacheWriteUpdateSubscriptionUsage,
		userID:  7,
		groupID: 11,
		amount:  2.5,
	}

	require.True(t, svc.enqueueCacheWrite(setTask))
	select {
	case <-cache.setStarted:
	case <-time.After(time.Second):
		t.Fatal("subscription snapshot write did not start")
	}

	require.True(t, svc.enqueueCacheWrite(updateTask))
	select {
	case <-cache.updateStarted:
		t.Fatal("usage update overtook the earlier subscription snapshot write")
	case <-time.After(100 * time.Millisecond):
	}

	close(cache.releaseSet)
	select {
	case <-cache.updateStarted:
	case <-time.After(time.Second):
		t.Fatal("usage update did not run after the snapshot write")
	}

	cache.mu.Lock()
	require.Equal(t, []string{"set", "update"}, cache.events)
	cache.mu.Unlock()
}

// Keep the embedded stub's interface methods visible to the compiler if the
// service interface gains another cache operation in the future.
var _ BillingCache = (*subscriptionOrderingCache)(nil)
