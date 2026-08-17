package handler

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSSubscriptionTurnLeaseStateReleasesUnsettledFirstTurn(t *testing.T) {
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	state := newOpenAIWSSubscriptionTurnLeaseState()

	admitted, err := state.acquire(context.Background(), billing, 101, 202, 1)
	require.NoError(t, err)
	require.True(t, admitted)

	// Models a dial/handshake error where ProxyResponsesWebSocketFromClient
	// returns before it can invoke AfterTurn.
	state.abortUnsettled()

	admitted, err = state.acquire(context.Background(), billing, 101, 202, 1)
	require.NoError(t, err)
	require.True(t, admitted)
	state.releaseTurn(1)
}

func TestOpenAIWSSubscriptionTurnLeaseStateKeepsCompletedTurnUntilSettlement(t *testing.T) {
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	state := newOpenAIWSSubscriptionTurnLeaseState()

	admitted, err := state.acquire(context.Background(), billing, 101, 202, 1)
	require.NoError(t, err)
	require.True(t, admitted)
	held := state.beginSettlement(1)
	require.NotNil(t, held)

	// Handler teardown must not release a turn whose mandatory usage task owns
	// the lease. A subsequent turn waits for that task rather than passing the
	// old cache snapshot.
	state.abortUnsettled()
	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	admitted, err = state.acquire(shortCtx, billing, 101, 202, 2)
	require.False(t, admitted)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	state.settleAfterUsage(held, billing, 101, 202)
	admitted, err = state.acquire(context.Background(), billing, 101, 202, 2)
	require.NoError(t, err)
	require.True(t, admitted)
	state.releaseTurn(2)
}

func TestOpenAIWSSubscriptionTurnLeaseStateReleasesResultNilAndAfterTurnError(t *testing.T) {
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	state := newOpenAIWSSubscriptionTurnLeaseState()

	admitted, err := state.acquire(context.Background(), billing, 101, 202, 1)
	require.NoError(t, err)
	require.True(t, admitted)

	// The handler's AfterTurn defer follows this path for result=nil, including
	// failures emitted by the adapter after a response.create was admitted.
	state.releaseTurn(1)
	admitted, err = state.acquire(context.Background(), billing, 101, 202, 2)
	require.NoError(t, err)
	require.True(t, admitted)

	// A non-nil turnErr with no bill also follows the same early-return defer.
	state.releaseTurn(2)
	admitted, err = state.acquire(context.Background(), billing, 101, 202, 3)
	require.NoError(t, err)
	require.True(t, admitted)
	state.releaseTurn(3)
}
