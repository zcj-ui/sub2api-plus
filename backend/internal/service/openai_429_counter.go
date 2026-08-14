package service

import (
	"context"
	"time"
)

// OpenAI429CounterCache coordinates consecutive OAuth 429 confirmation across instances.
type OpenAI429CounterCache interface {
	IncrementOpenAI429Count(ctx context.Context, accountID int64, window time.Duration) (int64, error)
	ResetOpenAI429Count(ctx context.Context, accountID int64) error
}
