package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSStateStore_BindGetDeleteResponseAccount(t *testing.T) {
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	groupID := int64(7)

	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_abc", 101, time.Minute))

	accountID, err := store.GetResponseAccount(ctx, groupID, "resp_abc")
	require.NoError(t, err)
	require.Equal(t, int64(101), accountID)

	require.NoError(t, store.DeleteResponseAccount(ctx, groupID, "resp_abc"))
	accountID, err = store.GetResponseAccount(ctx, groupID, "resp_abc")
	require.NoError(t, err)
	require.Zero(t, accountID)
}

func TestOpenAIWSStateStore_HTTPResponseOwnerPersistsAcrossStoreInstances(t *testing.T) {
	cache := &stubGatewayCache{}
	ctx := context.Background()
	groupID := int64(8)
	writer := NewOpenAIWSStateStore(cache)

	require.NoError(t, writer.BindHTTPResponseOwner(ctx, groupID, "resp_owned", 201, 301, time.Minute))
	userID, apiKeyID, found, err := writer.GetHTTPResponseOwner(ctx, groupID, "resp_owned")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(201), userID)
	require.Equal(t, int64(301), apiKeyID)

	reader := NewOpenAIWSStateStore(cache)
	userID, apiKeyID, found, err = reader.GetHTTPResponseOwner(ctx, groupID, "resp_owned")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(201), userID)
	require.Equal(t, int64(301), apiKeyID)
}

func TestOpenAIWSStateStore_ResponseConnTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindResponseConn("resp_conn", "conn_1", 30*time.Millisecond)

	connID, ok := store.GetResponseConn("resp_conn")
	require.True(t, ok)
	require.Equal(t, "conn_1", connID)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetResponseConn("resp_conn")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_GuardBindingsDoNotExpire(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	guardStore, ok := raw.(openAIWSGuardBindingStore)
	require.True(t, ok)

	guardStore.BindGuardResponse(7, "resp_guard_persistent", 101, "conn_guard_persistent")
	accountID, err := raw.GetResponseAccount(context.Background(), 7, "resp_guard_persistent")
	require.NoError(t, err)
	require.Equal(t, int64(101), accountID)
	connID, ok := raw.GetResponseConn("resp_guard_persistent")
	require.True(t, ok)
	require.Equal(t, "conn_guard_persistent", connID)

	guardStore.BindGuardSession(7, "session_guard_persistent", 101, "conn_guard_persistent")
	sessionAccount, sessionConn, ok := guardStore.GetGuardSession(7, "session_guard_persistent")
	require.True(t, ok)
	require.Equal(t, int64(101), sessionAccount)
	require.Equal(t, "conn_guard_persistent", sessionConn)

	// A guard binding uses the zero expiry sentinel and must remain available
	// across the ordinary cleanup interval.
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	store.lastCleanupUnixNano.Store(time.Now().Add(-2 * openAIWSStateStoreCleanupInterval).UnixNano())
	store.maybeCleanup()
	_, ok = raw.GetResponseConn("resp_guard_persistent")
	require.True(t, ok)
}

func TestOpenAIWSStateStore_OrdinaryBindingsCannotDowngradeGuard(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	guardStore, ok := raw.(openAIWSGuardBindingStore)
	require.True(t, ok)
	ctx := context.Background()

	guardStore.BindGuardResponse(3, "resp_guard_no_downgrade", 701, "conn_guard")
	raw.BindResponseConn("resp_guard_no_downgrade", "conn_other", time.Minute)
	require.NoError(t, raw.BindResponseAccount(ctx, 3, "resp_guard_no_downgrade", 999, time.Minute))
	accountID, err := raw.GetResponseAccount(ctx, 3, "resp_guard_no_downgrade")
	require.NoError(t, err)
	require.Equal(t, int64(701), accountID)
	connID, ok := raw.GetResponseConn("resp_guard_no_downgrade")
	require.True(t, ok)
	require.Equal(t, "conn_guard", connID)

	guardStore.BindGuardSession(3, "session_guard_no_downgrade", 701, "conn_guard")
	raw.BindSessionConn(3, "session_guard_no_downgrade", "conn_other", time.Minute)
	sessionAccount, sessionConn, ok := guardStore.GetGuardSession(3, "session_guard_no_downgrade")
	require.True(t, ok)
	require.Equal(t, int64(701), sessionAccount)
	require.Equal(t, "conn_guard", sessionConn)
}

func TestOpenAIWSStateStore_OrdinaryDeletesPreserveGuardTuple(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	guardStore, ok := raw.(openAIWSGuardBindingStore)
	require.True(t, ok)
	ctx := context.Background()

	guardStore.BindGuardResponse(5, "resp_guard_delete", 901, "conn_guard_delete")
	require.NoError(t, raw.DeleteResponseAccount(ctx, 5, "resp_guard_delete"))
	raw.DeleteResponseConn("resp_guard_delete")
	accountID, err := raw.GetResponseAccount(ctx, 5, "resp_guard_delete")
	require.NoError(t, err)
	require.Equal(t, int64(901), accountID)
	connID, connOK := raw.GetResponseConn("resp_guard_delete")
	require.True(t, connOK)
	require.Equal(t, "conn_guard_delete", connID)

	guardStore.BindGuardSession(5, "session_guard_delete", 901, "conn_guard_delete")
	raw.DeleteSessionConn(5, "session_guard_delete")
	sessionAccount, sessionConn, sessionOK := guardStore.GetGuardSession(5, "session_guard_delete")
	require.True(t, sessionOK)
	require.Equal(t, int64(901), sessionAccount)
	require.Equal(t, "conn_guard_delete", sessionConn)
}

func TestOpenAIWSStateStore_ConditionalResponseDeleteChecksConnection(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	ctx := context.Background()
	require.NoError(t, raw.BindResponseAccount(ctx, 4, "resp_conditional", 801, time.Minute))
	raw.BindResponseConn("resp_conditional", "conn_new", time.Minute)

	cleaner := openAIWSContinuationBindingCleaner(store)
	require.False(t, cleaner.deleteResponseBindingIfMatches(ctx, 4, "resp_conditional", 801, "conn_old"))
	accountID, err := raw.GetResponseAccount(ctx, 4, "resp_conditional")
	require.NoError(t, err)
	require.Equal(t, int64(801), accountID)

	require.False(t, cleaner.deleteResponseBindingIfMatches(ctx, 4, "resp_conditional", 801, ""))
	accountID, err = raw.GetResponseAccount(ctx, 4, "resp_conditional")
	require.NoError(t, err)
	require.Equal(t, int64(801), accountID)

	require.True(t, cleaner.deleteResponseBindingIfMatches(ctx, 4, "resp_conditional", 801, "conn_new"))
	accountID, err = raw.GetResponseAccount(ctx, 4, "resp_conditional")
	require.NoError(t, err)
	require.Zero(t, accountID)
}

func TestOpenAIWSStateStore_ConditionalGuardDeleteRequiresExactConnection(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	guardStore, ok := raw.(openAIWSGuardBindingStore)
	require.True(t, ok)
	ctx := context.Background()
	guardStore.BindGuardResponse(6, "resp_guard_conditional", 902, "conn_guard_conditional")
	cleaner := openAIWSContinuationBindingCleaner(store)

	require.False(t, cleaner.deleteResponseBindingIfMatches(ctx, 6, "resp_guard_conditional", 902, ""))
	require.False(t, cleaner.deleteResponseBindingIfMatches(ctx, 6, "resp_guard_conditional", 902, "conn_other"))
	accountID, err := raw.GetResponseAccount(ctx, 6, "resp_guard_conditional")
	require.NoError(t, err)
	require.Equal(t, int64(902), accountID)

	require.True(t, cleaner.deleteResponseBindingIfMatches(ctx, 6, "resp_guard_conditional", 902, "conn_guard_conditional"))
	accountID, err = raw.GetResponseAccount(ctx, 6, "resp_guard_conditional")
	require.NoError(t, err)
	require.Zero(t, accountID)
	_, ok = raw.GetResponseConn("resp_guard_conditional")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_PoolEvictionClearsExactConnectionBindings(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	pool := newOpenAIWSConnPool(nil)
	defer pool.Close()
	pool.setClientDialerForTest(&openAIWSFakeDialer{})
	pool.setGuardBindingInvalidator(store.invalidateConnectionBindings)

	account := &Account{ID: 913, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	lease, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.test/responses",
	})
	require.NoError(t, err)
	require.NotNil(t, lease)

	connID := lease.ConnID()
	guardStore, ok := raw.(openAIWSGuardBindingStore)
	require.True(t, ok)
	guardStore.BindGuardResponse(9, "resp_pool_evicted", account.ID, connID)
	guardStore.BindGuardSession(9, "session_pool_evicted", account.ID, connID)
	raw.BindSessionTurnState(9, "session_pool_evicted", "turn_state_pool_evicted", time.Hour)

	lease.MarkBroken()
	lease.Release()

	responseAccountID, err := raw.GetResponseAccount(context.Background(), 9, "resp_pool_evicted")
	require.NoError(t, err)
	require.Zero(t, responseAccountID)
	_, responseConnOK := raw.GetResponseConn("resp_pool_evicted")
	require.False(t, responseConnOK)
	_, _, guardSessionOK := guardStore.GetGuardSession(9, "session_pool_evicted")
	require.False(t, guardSessionOK)
	_, turnStateOK := raw.GetSessionTurnState(9, "session_pool_evicted")
	require.False(t, turnStateOK)
}

func TestOpenAIWSStateStore_SessionTurnStateTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindSessionTurnState(9, "session_hash_1", "turn_state_1", 30*time.Millisecond)

	state, ok := store.GetSessionTurnState(9, "session_hash_1")
	require.True(t, ok)
	require.Equal(t, "turn_state_1", state)

	// group 隔离
	_, ok = store.GetSessionTurnState(10, "session_hash_1")
	require.False(t, ok)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetSessionTurnState(9, "session_hash_1")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_SessionConnTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindSessionConn(9, "session_hash_conn_1", "conn_1", 30*time.Millisecond)

	connID, ok := store.GetSessionConn(9, "session_hash_conn_1")
	require.True(t, ok)
	require.Equal(t, "conn_1", connID)

	// group 隔离
	_, ok = store.GetSessionConn(10, "session_hash_conn_1")
	require.False(t, ok)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetSessionConn(9, "session_hash_conn_1")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_GetResponseAccount_NoStaleAfterCacheMiss(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	store := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	groupID := int64(17)
	responseID := "resp_cache_stale"
	cacheKey := openAIWSResponseAccountCacheKey(responseID)

	cache.sessionBindings[cacheKey] = 501
	accountID, err := store.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Equal(t, int64(501), accountID)

	delete(cache.sessionBindings, cacheKey)
	accountID, err = store.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Zero(t, accountID, "上游缓存失效后不应继续命中本地陈旧映射")
}

func TestOpenAIWSStateStore_MaybeCleanupRemovesExpiredIncrementally(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)

	expiredAt := time.Now().Add(-time.Minute)
	total := 2048
	store.responseToConnMu.Lock()
	for i := 0; i < total; i++ {
		store.responseToConn[fmt.Sprintf("resp_%d", i)] = openAIWSConnBinding{
			connID:    "conn_incremental",
			expiresAt: expiredAt,
		}
	}
	store.responseToConnMu.Unlock()

	store.lastCleanupUnixNano.Store(time.Now().Add(-2 * openAIWSStateStoreCleanupInterval).UnixNano())
	store.maybeCleanup()

	store.responseToConnMu.RLock()
	remainingAfterFirst := len(store.responseToConn)
	store.responseToConnMu.RUnlock()
	require.Less(t, remainingAfterFirst, total, "单轮 cleanup 应至少有进展")
	require.Greater(t, remainingAfterFirst, 0, "增量清理不要求单轮清空全部键")

	for i := 0; i < 8; i++ {
		store.lastCleanupUnixNano.Store(time.Now().Add(-2 * openAIWSStateStoreCleanupInterval).UnixNano())
		store.maybeCleanup()
	}

	store.responseToConnMu.RLock()
	remaining := len(store.responseToConn)
	store.responseToConnMu.RUnlock()
	require.Zero(t, remaining, "多轮 cleanup 后应逐步清空全部过期键")
}

func TestEnsureBindingCapacity_EvictsOneWhenMapIsFull(t *testing.T) {
	bindings := map[string]int{
		"a": 1,
		"b": 2,
	}

	ensureBindingCapacity(bindings, "c", 2)
	bindings["c"] = 3

	require.Len(t, bindings, 2)
	require.Equal(t, 3, bindings["c"])
}

func TestEnsureBindingCapacity_DoesNotEvictWhenUpdatingExistingKey(t *testing.T) {
	bindings := map[string]int{
		"a": 1,
		"b": 2,
	}

	ensureBindingCapacity(bindings, "a", 2)
	bindings["a"] = 9

	require.Len(t, bindings, 2)
	require.Equal(t, 9, bindings["a"])
}

func TestEnsureBindingCapacityPreservingProtectsPermanentGuards(t *testing.T) {
	permanent := openAIWSConnBinding{connID: "guard"}
	expiring := openAIWSConnBinding{connID: "ordinary", expiresAt: time.Now().Add(time.Minute)}
	bindings := map[string]openAIWSConnBinding{
		"guard":    permanent,
		"ordinary": expiring,
	}
	canEvict := func(binding openAIWSConnBinding) bool { return !binding.expiresAt.IsZero() }

	require.True(t, ensureBindingCapacityPreserving(bindings, "new", 2, canEvict))
	require.NotContains(t, bindings, "ordinary")
	require.Contains(t, bindings, "guard")

	bindings = map[string]openAIWSConnBinding{"guard": permanent}
	require.False(t, ensureBindingCapacityPreserving(bindings, "new", 1, canEvict))
	require.Equal(t, permanent, bindings["guard"])
}

type openAIWSStateStoreTimeoutProbeCache struct {
	setHasDeadline    bool
	setCanceled       bool
	setValue          any
	getHasDeadline    bool
	deleteHasDeadline bool
	setDeadlineDelta  time.Duration
	getDeadlineDelta  time.Duration
	delDeadlineDelta  time.Duration
}

func (c *openAIWSStateStoreTimeoutProbeCache) GetSessionAccountID(ctx context.Context, _ int64, _ string) (int64, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.getHasDeadline = true
		c.getDeadlineDelta = time.Until(deadline)
	}
	return 123, nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) SetSessionAccountID(ctx context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	c.setCanceled = ctx.Err() != nil
	c.setValue = ctx.Value(openAIWSStateStoreProbeContextKey{})
	if deadline, ok := ctx.Deadline(); ok {
		c.setHasDeadline = true
		c.setDeadlineDelta = time.Until(deadline)
	}
	return errors.New("set failed")
}

type openAIWSStateStoreProbeContextKey struct{}

func (c *openAIWSStateStoreTimeoutProbeCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) DeleteSessionAccountID(ctx context.Context, _ int64, _ string) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.deleteHasDeadline = true
		c.delDeadlineDelta = time.Until(deadline)
	}
	return nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *openAIWSStateStoreTimeoutProbeCache) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (c *openAIWSStateStoreTimeoutProbeCache) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) SetReasoningContent(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (c *openAIWSStateStoreTimeoutProbeCache) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func TestOpenAIWSStateStore_RedisOpsUseShortTimeout(t *testing.T) {
	probe := &openAIWSStateStoreTimeoutProbeCache{}
	store := NewOpenAIWSStateStore(probe)
	ctx := context.Background()
	groupID := int64(5)

	err := store.BindResponseAccount(ctx, groupID, "resp_timeout_probe", 11, time.Minute)
	require.Error(t, err)

	accountID, getErr := store.GetResponseAccount(ctx, groupID, "resp_timeout_probe")
	require.NoError(t, getErr)
	require.Equal(t, int64(11), accountID, "本地缓存命中应优先返回已绑定账号")

	require.NoError(t, store.DeleteResponseAccount(ctx, groupID, "resp_timeout_probe"))

	require.True(t, probe.setHasDeadline, "SetSessionAccountID 应携带独立超时上下文")
	require.True(t, probe.deleteHasDeadline, "DeleteSessionAccountID 应携带独立超时上下文")
	require.False(t, probe.getHasDeadline, "GetSessionAccountID 本用例应由本地缓存命中，不触发 Redis 读取")
	require.Greater(t, probe.setDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.setDeadlineDelta, 3*time.Second)
	require.Greater(t, probe.delDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.delDeadlineDelta, 3*time.Second)

	probe2 := &openAIWSStateStoreTimeoutProbeCache{}
	store2 := NewOpenAIWSStateStore(probe2)
	accountID2, err2 := store2.GetResponseAccount(ctx, groupID, "resp_cache_only")
	require.NoError(t, err2)
	require.Equal(t, int64(123), accountID2)
	require.True(t, probe2.getHasDeadline, "GetSessionAccountID 在缓存未命中时应携带独立超时上下文")
	require.Greater(t, probe2.getDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe2.getDeadlineDelta, 3*time.Second)
}

func TestWithOpenAIWSStateStoreRedisTimeout_WithParentContext(t *testing.T) {
	ctx, cancel := withOpenAIWSStateStoreRedisTimeout(context.Background())
	defer cancel()
	require.NotNil(t, ctx)
	_, ok := ctx.Deadline()
	require.True(t, ok, "应附加短超时")
}

func TestOpenAIWSStateStore_BindResponseAccountDetachedWriteContext(t *testing.T) {
	probe := &openAIWSStateStoreTimeoutProbeCache{}
	store := NewOpenAIWSStateStore(probe)
	parent := context.WithValue(context.Background(), openAIWSStateStoreProbeContextKey{}, "trace")
	ctx, cancel := context.WithCancel(parent)
	cancel()

	err := store.BindResponseAccount(ctx, 9, "resp_detached_write", 77, time.Minute)
	require.Error(t, err, "probe deliberately rejects the write")
	require.False(t, probe.setCanceled, "durable binding write must outlive request cancellation")
	require.Equal(t, "trace", probe.setValue, "context values must remain available to the write")
	require.True(t, probe.setHasDeadline)
	require.Greater(t, probe.setDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.setDeadlineDelta, 3*time.Second)
}

func TestOpenAIWSStateStore_BindHTTPResponseOwnerDetachedWriteContext(t *testing.T) {
	probe := &openAIWSStateStoreTimeoutProbeCache{}
	store := NewOpenAIWSStateStore(probe)
	parent := context.WithValue(context.Background(), openAIWSStateStoreProbeContextKey{}, "trace-owner")
	ctx, cancel := context.WithCancel(parent)
	cancel()

	err := store.BindHTTPResponseOwner(ctx, 9, "resp_owner_detached_write", 77, 88, time.Minute)
	require.Error(t, err, "probe deliberately rejects the owner write")
	require.False(t, probe.setCanceled, "durable owner binding must outlive request cancellation")
	require.Equal(t, "trace-owner", probe.setValue, "context values must remain available to the write")
	require.True(t, probe.setHasDeadline)
	require.Greater(t, probe.setDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.setDeadlineDelta, 3*time.Second)
}
