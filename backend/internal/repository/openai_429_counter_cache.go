package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const openAI429CounterPrefix = "openai_429_confirm:account:"

var openAI429CounterIncrScript = redis.NewScript(`
	local key = KEYS[1]
	local ttl = tonumber(ARGV[1])
	local required = tonumber(ARGV[2])

	local count = redis.call('INCR', key)
	redis.call('EXPIRE', key, ttl)
	if count >= required then
		redis.call('DEL', key)
	end
	return count
`)

type openAI429CounterCache struct {
	rdb *redis.Client
}

func NewOpenAI429CounterCache(rdb *redis.Client) service.OpenAI429CounterCache {
	return &openAI429CounterCache{rdb: rdb}
}

func (c *openAI429CounterCache) IncrementOpenAI429Count(ctx context.Context, accountID int64, window time.Duration) (int64, error) {
	if c == nil || c.rdb == nil {
		return 0, fmt.Errorf("openai 429 counter cache is unavailable")
	}
	ttlSeconds := int64(window / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	key := fmt.Sprintf("%s%d", openAI429CounterPrefix, accountID)
	count, err := openAI429CounterIncrScript.Run(ctx, c.rdb, []string{key}, ttlSeconds, 2).Int64()
	if err != nil {
		return 0, fmt.Errorf("increment openai 429 count: %w", err)
	}
	return count, nil
}

func (c *openAI429CounterCache) ResetOpenAI429Count(ctx context.Context, accountID int64) error {
	if c == nil || c.rdb == nil {
		return fmt.Errorf("openai 429 counter cache is unavailable")
	}
	key := fmt.Sprintf("%s%d", openAI429CounterPrefix, accountID)
	return c.rdb.Del(ctx, key).Err()
}
