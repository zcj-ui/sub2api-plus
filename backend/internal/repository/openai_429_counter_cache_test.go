package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAI429CounterCacheCoordinatesAndResets(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	firstInstance := NewOpenAI429CounterCache(rdb)
	secondInstance := NewOpenAI429CounterCache(rdb)
	ctx := context.Background()

	count, err := firstInstance.IncrementOpenAI429Count(ctx, 71, 30*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Greater(t, mr.TTL(openAI429CounterPrefix+"71"), time.Duration(0))

	count, err = secondInstance.IncrementOpenAI429Count(ctx, 71, 30*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.False(t, mr.Exists(openAI429CounterPrefix+"71"), "confirmation must atomically clear the streak")

	count, err = firstInstance.IncrementOpenAI429Count(ctx, 71, 30*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.NoError(t, secondInstance.ResetOpenAI429Count(ctx, 71))
	require.False(t, mr.Exists(openAI429CounterPrefix+"71"))
}
