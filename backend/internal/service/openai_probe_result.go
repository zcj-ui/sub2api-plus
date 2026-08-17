package service

import (
	"context"
	"errors"
	"io"
	"time"
)

// openAIProbeVerdict is deliberately tri-state. A transport/protocol failure
// must not overwrite a previously known capability with "unsupported".
type openAIProbeVerdict uint8

const (
	openAIProbeVerdictUnknown openAIProbeVerdict = iota
	openAIProbeVerdictSupported
	openAIProbeVerdictUnsupported
)

var errOpenAIProbeBodyTooLarge = errors.New("probe response body exceeds limit")

const openAIProbePersistenceTimeout = 3 * time.Second

type openAIProbeExtraUpdater interface {
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

// persistOpenAIProbeExtra keeps the diagnostic write independent from a client
// disconnect while retaining context values used by repository tracing.
func persistOpenAIProbeExtra(callerCtx context.Context, repo openAIProbeExtraUpdater, accountID int64, updates map[string]any) error {
	if repo == nil || len(updates) == 0 {
		return nil
	}
	baseCtx := context.Background()
	if callerCtx != nil {
		baseCtx = context.WithoutCancel(callerCtx)
	}
	persistCtx, cancel := context.WithTimeout(baseCtx, openAIProbePersistenceTimeout)
	defer cancel()
	return repo.UpdateExtra(persistCtx, accountID, updates)
}

// readOpenAIProbeBody reads one extra byte so a truncated transcript cannot
// be mistaken for a complete protocol exchange.
func readOpenAIProbeBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = 1
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], errOpenAIProbeBodyTooLarge
	}
	return data, nil
}
