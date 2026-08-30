//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func runtimeBlockObserverTestAccount(updatedAt time.Time) *Account {
	return &Account{
		ID:          88001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		UpdatedAt:   updatedAt,
	}
}

func TestOpenAIRuntimeBlockObserverClearsStaleCrossInstanceBlock(t *testing.T) {
	initialVersion := time.Now().Add(-2 * time.Second).UTC()
	clearedVersion := initialVersion.Add(time.Second)
	account := runtimeBlockObserverTestAccount(initialVersion)
	svc := &OpenAIGatewayService{}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	// The scheduler outbox event is observed after the other instance has
	// cleared the durable cooldown.  The fresh row version is newer than the
	// version captured when this process installed its block.
	fresh := runtimeBlockObserverTestAccount(clearedVersion)
	svc.ReconcileOpenAIAccountRuntimeBlock(fresh, time.Now().Add(time.Second))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(fresh), "fresh durable clear should retire the local block")
	_, present := svc.openaiAccountRuntimeBlockInstalledAt.Load(account.ID)
	require.False(t, present, "reconciliation should retire the installation marker")
}

func TestOpenAIRuntimeBlockObserverEventRequiresExplicitDurableClearMarker(t *testing.T) {
	initialVersion := time.Now().Add(-2 * time.Second).UTC()
	account := runtimeBlockObserverTestAccount(initialVersion)
	svc := &OpenAIGatewayService{}
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")

	fresh := runtimeBlockObserverTestAccount(initialVersion.Add(time.Second))
	svc.ReconcileOpenAIAccountRuntimeBlockEvent(fresh, time.Now(), map[string]any{
		"quota_snapshot": true,
	})
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(fresh), "an unrelated account update must not clear a fail-closed block")

	svc.ReconcileOpenAIAccountRuntimeBlockEvent(fresh, time.Now(), map[string]any{
		SchedulerRuntimeBlockClearPayloadKey: true,
	})
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(fresh), "an explicit durable clear event should retire the block")
}

func TestOpenAIRuntimeBlockObserverEventDoesNotClearBlockInstalledAfterEventCreation(t *testing.T) {
	initialVersion := time.Now().Add(-3 * time.Second).UTC()
	account := runtimeBlockObserverTestAccount(initialVersion)
	svc := &OpenAIGatewayService{}
	eventCreatedAt := time.Now().Add(-time.Second).UTC()

	// The local request starts after the outbox clear event was created. Even
	// with an explicit clear marker, that older event must not erase this newer
	// fail-closed generation.
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	fresh := runtimeBlockObserverTestAccount(initialVersion.Add(time.Second))
	svc.ReconcileOpenAIAccountRuntimeBlockEvent(fresh, eventCreatedAt, map[string]any{
		SchedulerRuntimeBlockClearPayloadKey: true,
	})
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(fresh))
}

func TestOpenAIRuntimeBlockObserverPreservesNewerRacingBlock(t *testing.T) {
	initialVersion := time.Now().Add(-3 * time.Second).UTC()
	clearedVersion := initialVersion.Add(2 * time.Second)
	account := runtimeBlockObserverTestAccount(initialVersion)
	svc := &OpenAIGatewayService{}

	// Simulate a new request arriving after the outbox worker captured its
	// observation timestamp, but before the callback runs.  It may still be
	// waiting for its durable SetRateLimited write, so the local block must win.
	eventObservedAt := time.Now().Add(-time.Second).UTC()
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	svc.ReconcileOpenAIAccountRuntimeBlock(runtimeBlockObserverTestAccount(clearedVersion), eventObservedAt)

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "a newer local block must survive an older outbox observation")
}

func TestOpenAIRuntimeBlockObserverKeepsDurableCooldown(t *testing.T) {
	initialVersion := time.Now().Add(-2 * time.Second).UTC()
	account := runtimeBlockObserverTestAccount(initialVersion)
	svc := &OpenAIGatewayService{}
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")

	cooldownUntil := time.Now().Add(5 * time.Minute)
	fresh := runtimeBlockObserverTestAccount(initialVersion.Add(time.Second))
	fresh.RateLimitResetAt = &cooldownUntil
	svc.ReconcileOpenAIAccountRuntimeBlock(fresh, time.Now())

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(fresh), "a durable cooldown must not be cleared by the observer")
}

func TestOpenAIRuntimeBlockObserverReleasesPermanent429GuardPin(t *testing.T) {
	initialVersion := time.Now().Add(-2 * time.Second).UTC()
	account := runtimeBlockObserverTestAccount(initialVersion)
	account.Extra = map[string]any{OpenAICodex429GuardEnabledExtraKey: true}

	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()
	svc := &OpenAIGatewayService{openaiWSPool: pool}
	// Initialize the atomic pool reference used by existingOpenAIWSConnPool.
	require.Same(t, pool, svc.getOpenAIWSConnPool())

	ap := pool.getOrCreateAccountPool(account.ID)
	conn := newOpenAIWSConn("observer_guard", account.ID, nil, nil)
	ap.conns[conn.id] = conn

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, pool.PinGuardConn(account.ID, conn.id))
	require.True(t, pool.HasPermanentGuardPin(account.ID))
	require.True(t, svc.hasOpenAI429GuardReservation(account), "guard pin should reserve the account before recovery")

	fresh := runtimeBlockObserverTestAccount(initialVersion.Add(time.Second))
	svc.ReconcileOpenAIAccountRuntimeBlock(fresh, time.Now())

	// A durable recovery must release the continuation-only guard reservation;
	// otherwise ordinary scheduling would remain filtered after the local block
	// itself was cleared.
	require.False(t, pool.HasPermanentGuardPin(account.ID))
	require.False(t, svc.hasOpenAI429GuardReservation(fresh))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(fresh))
}
