package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSConnPool_GuardGenerationRequiresExactConnectionProof(t *testing.T) {
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()

	accountID := int64(701)
	ap := pool.getOrCreateAccountPool(accountID)
	oldConn := newOpenAIWSConn("guard_generation_old", accountID, nil, nil)
	newConn := newOpenAIWSConn("guard_generation_new", accountID, nil, nil)
	ap.conns[oldConn.id] = oldConn
	ap.conns[newConn.id] = newConn

	require.False(t, pool.MarkGuardConnConfirmed(accountID, oldConn.id, 0))
	require.False(t, pool.PinGuardConnForGeneration(accountID, oldConn.id, 0))
	require.False(t, pool.PinGuardConnForGeneration(accountID, oldConn.id, 41), "an unproven socket must not be promoted")

	require.True(t, pool.MarkGuardConnConfirmed(accountID, oldConn.id, 41))
	require.False(t, pool.PinGuardConnForGeneration(accountID, oldConn.id, 42), "a proof from another runtime generation must not match")
	require.True(t, pool.PinGuardConnForGeneration(accountID, oldConn.id, 41))
	require.False(t, pool.PinGuardConnForGeneration(accountID, newConn.id, 41), "a different socket must not inherit the proof")

	oldConn.close()
	require.False(t, pool.MarkGuardConnConfirmed(accountID, oldConn.id, 43), "a closed socket cannot receive proof")
}

func TestOpenAIWSConnPool_GuardCandidateCannotCrossRuntimeGeneration(t *testing.T) {
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()

	accountID := int64(703)
	ap := pool.getOrCreateAccountPool(accountID)
	oldConn := newOpenAIWSConn("guard_candidate_old", accountID, nil, nil)
	freshConn := newOpenAIWSConn("guard_candidate_fresh", accountID, nil, nil)
	ap.conns[oldConn.id] = oldConn

	// The old socket existed at generation 41. A later block generation must
	// not inherit that candidate proof merely because the socket stayed alive.
	pool.MarkExistingConnsAs429GuardCandidates(accountID, 41)
	require.True(t, pool.IsGuardConnCandidate(accountID, oldConn.id, 41))
	require.False(t, pool.IsGuardConnCandidate(accountID, oldConn.id, 42))
	require.False(t, pool.MarkAndPinGuardConnConfirmed(accountID, oldConn.id, 42))
	require.False(t, pool.IsGuardConnPinned(accountID, oldConn.id))

	// A fresh block explicitly establishes a new boundary. Connections already
	// in the pool can become candidates for that exact generation, while a
	// socket published afterwards remains ineligible.
	pool.MarkExistingConnsAs429GuardCandidates(accountID, 42)
	ap.mu.Lock()
	ap.conns[freshConn.id] = freshConn
	ap.mu.Unlock()
	require.True(t, pool.MarkAndPinGuardConnConfirmed(accountID, oldConn.id, 42))
	require.False(t, pool.MarkAndPinGuardConnConfirmed(accountID, freshConn.id, 42))
}

func TestOpenAIWSConnPool_GuardCandidateCutoffExcludesNewlyCreatedSocket(t *testing.T) {
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()

	accountID := int64(706)
	ap := pool.getOrCreateAccountPool(accountID)
	cutoff := time.Now()
	oldConn := newOpenAIWSConn("guard_cutoff_old", accountID, nil, nil)
	oldConn.createdAtNano.Store(cutoff.Add(-time.Millisecond).UnixNano())
	newConn := newOpenAIWSConn("guard_cutoff_new", accountID, nil, nil)
	newConn.createdAtNano.Store(cutoff.Add(time.Millisecond).UnixNano())
	ap.mu.Lock()
	ap.conns[oldConn.id] = oldConn
	ap.conns[newConn.id] = newConn
	ap.mu.Unlock()

	pool.markExistingConnsAs429GuardCandidatesAt(accountID, cutoff, 77)
	require.True(t, pool.IsGuardConnCandidate(accountID, oldConn.id, 77))
	require.False(t, pool.IsGuardConnCandidate(accountID, newConn.id, 77))
	require.True(t, pool.MarkAndPinGuardConnConfirmed(accountID, oldConn.id, 77))
	require.False(t, pool.MarkAndPinGuardConnConfirmed(accountID, newConn.id, 77))
}

func TestOpenAIWSConnPool_InvalidateGuardConnsEvictsRetainedSocket(t *testing.T) {
	pool := newOpenAIWSConnPool(&config.Config{})
	defer pool.Close()

	accountID := int64(704)
	ap := pool.getOrCreateAccountPool(accountID)
	guardConn := newOpenAIWSConn("guard_invalidate", accountID, nil, nil)
	ttlConn := newOpenAIWSConn("ttl_retention", accountID, nil, nil)
	ap.conns[guardConn.id] = guardConn
	ap.conns[ttlConn.id] = ttlConn
	require.True(t, pool.MarkGuardConnConfirmed(accountID, guardConn.id, 51))
	require.True(t, pool.PinGuardConnForGeneration(accountID, guardConn.id, 51))
	require.True(t, pool.PinConnUntil(accountID, ttlConn.id, time.Now().Add(time.Minute)))

	pool.InvalidateGuardConns(accountID)
	require.False(t, pool.IsGuardConnPinned(accountID, guardConn.id))
	select {
	case <-guardConn.closedCh:
	default:
		t.Fatal("guard connection must be closed when invalidated")
	}
	ap.mu.Lock()
	_, stillPooled := ap.conns[guardConn.id]
	_, ttlStillPooled := ap.conns[ttlConn.id]
	ap.mu.Unlock()
	require.False(t, stillPooled)
	require.True(t, ttlStillPooled, "invalidating a 429 guard must not evict an unrelated TTL pin")
}

func TestOpenAIWSConnPool_NormalAcquireCannotBorrowPermanentGuardConnection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()

	account := &Account{ID: 705, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	ap := pool.getOrCreateAccountPool(account.ID)
	guardConn := newOpenAIWSConn("guard_normal_acquire", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[guardConn.id] = guardConn
	ap.mu.Unlock()
	require.True(t, pool.PinGuardConn(account.ID, guardConn.id))

	// An unrelated request has no force-preferred continuation proof. It must
	// leave the retained socket untouched and let the account scheduler switch.
	lease, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{Account: account, WSURL: "wss://example.com/v1/responses"})
	require.Nil(t, lease)
	require.ErrorIs(t, err, errOpenAIWSConnQueueFull)
	require.True(t, pool.IsGuardConnPinned(account.ID, guardConn.id))

	// The exact continuation path remains able to acquire the old socket.
	lease, err = pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account:            account,
		WSURL:              "wss://example.com/v1/responses",
		PreferredConnID:    guardConn.id,
		ForcePreferredConn: true,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, guardConn.id, lease.ConnID())
	lease.Release()
}

func TestOpenAIWSConnPool_GuardPinSuppressesPrewarmAndReleasesReservations(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 4
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 3
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2

	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := newOpenAIWSFirstDialBlockingCaptureDialer()
	pool.setClientDialerForTest(dialer)

	accountID := int64(702)
	account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	ap := pool.getOrCreateAccountPool(accountID)
	oldConn := newOpenAIWSConn("guard_prewarm_old", accountID, nil, nil)
	ap.conns[oldConn.id] = oldConn
	ap.lastAcquire = &openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.com/v1/responses",
	}

	// One old socket is already present, so the target of three reserves two
	// prewarm slots. Establish the guard only after the first dial is in flight
	// to exercise both result discard and reservation cleanup.
	pool.ensureTargetIdleAsync(accountID)
	select {
	case <-dialer.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("prewarm dial did not start")
	}
	require.True(t, pool.MarkGuardConnConfirmed(accountID, oldConn.id, 71))
	require.True(t, pool.PinGuardConnForGeneration(accountID, oldConn.id, 71))
	close(dialer.releaseFirst)

	require.Eventually(t, func() bool {
		ap.mu.Lock()
		defer ap.mu.Unlock()
		return !ap.prewarmActive && ap.creating == 0
	}, 2*time.Second, 10*time.Millisecond)

	func() {
		ap.mu.Lock()
		defer ap.mu.Unlock()
		require.Len(t, ap.conns, 1, "an in-flight prewarm result must not be published beside the guard socket")
		require.Contains(t, ap.conns, oldConn.id)
		require.True(t, pool.hasPermanentGuardPinLocked(ap))
		require.Equal(t, 1, dialer.DialCount(), "guard activation must stop remaining prewarm dials")
	}()

	// A later ensure call sees the permanent pin and does not start another
	// background dial.
	pool.ensureTargetIdleAsync(accountID)
	require.Equal(t, 1, dialer.DialCount())
}
