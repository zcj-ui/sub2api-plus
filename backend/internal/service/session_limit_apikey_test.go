//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sessionLimitAPITestCache is a small in-memory stand-in for the Redis
// session-limit cache.  It deliberately embeds the wider interface because
// these tests exercise only RegisterSession.
type sessionLimitAPITestCache struct {
	SessionLimitCache

	mu     sync.Mutex
	active map[int64]map[string]struct{}
	calls  int
	err    error
}

func (c *sessionLimitAPITestCache) RegisterSession(_ context.Context, accountID int64, sessionID string, maxSessions int, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return false, c.err
	}
	if c.active == nil {
		c.active = make(map[int64]map[string]struct{})
	}
	if c.active[accountID] == nil {
		c.active[accountID] = make(map[string]struct{})
	}
	if _, exists := c.active[accountID][sessionID]; exists {
		return true, nil
	}
	if len(c.active[accountID]) >= maxSessions {
		return false, nil
	}
	c.active[accountID][sessionID] = struct{}{}
	return true, nil
}

func (c *sessionLimitAPITestCache) seed(accountID int64, sessionIDs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = make(map[int64]map[string]struct{})
	}
	if c.active[accountID] == nil {
		c.active[accountID] = make(map[string]struct{})
	}
	for _, id := range sessionIDs {
		c.active[accountID][id] = struct{}{}
	}
}

func TestCheckAndRegisterSession_APIKeyAndOAuthScope(t *testing.T) {
	ctx := context.Background()
	cache := &sessionLimitAPITestCache{}
	svc := &GatewayService{sessionLimitCache: cache}

	apiKey := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"max_sessions": 2},
	}
	require.True(t, svc.checkAndRegisterSession(ctx, apiKey, "session-a"))
	require.True(t, svc.checkAndRegisterSession(ctx, apiKey, "session-a"), "an existing session refreshes in place")
	require.True(t, svc.checkAndRegisterSession(ctx, apiKey, "session-b"))
	require.False(t, svc.checkAndRegisterSession(ctx, apiKey, "session-c"), "a new session beyond max_sessions must be rejected")

	anthropicOAuth := &Account{
		ID:       102,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"max_sessions": 1},
	}
	require.True(t, svc.checkAndRegisterSession(ctx, anthropicOAuth, "anthropic-a"))
	require.False(t, svc.checkAndRegisterSession(ctx, anthropicOAuth, "anthropic-b"), "existing Anthropic behavior must remain enforced")

	openAIOAuth := &Account{
		ID:       103,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"max_sessions": 1},
	}
	// max_sessions is intentionally not expanded to OpenAI OAuth here: its
	// Responses/WS lifecycle has separate admission semantics and must not be
	// changed by the API-key fix.
	require.True(t, svc.checkAndRegisterSession(ctx, openAIOAuth, "oauth-a"))
	require.True(t, svc.checkAndRegisterSession(ctx, openAIOAuth, "oauth-b"))
}

func TestCheckAndRegisterSession_DisabledAndCacheFailureAreFailOpen(t *testing.T) {
	ctx := context.Background()
	cache := &sessionLimitAPITestCache{err: errors.New("redis unavailable")}
	svc := &GatewayService{sessionLimitCache: cache}

	for _, account := range []*Account{
		{ID: 201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"max_sessions": 0}},
		{ID: 203, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"max_sessions": -1}},
	} {
		require.True(t, svc.checkAndRegisterSession(ctx, account, "session"), "disabled/unlimited API-key accounts must pass")
	}
	// No cache call is needed when the cap is disabled.
	require.Zero(t, cache.calls)
}

type sessionLimitOpenAISchedulerStub struct {
	selections []*AccountSelectionResult
	calls      int
}

func (s *sessionLimitOpenAISchedulerStub) Select(_ context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	s.calls++
	for _, selection := range s.selections {
		if selection == nil || selection.Account == nil {
			continue
		}
		if _, excluded := req.ExcludedIDs[selection.Account.ID]; excluded {
			continue
		}
		return selection, OpenAIAccountScheduleDecision{SelectedAccountID: selection.Account.ID}, nil
	}
	return nil, OpenAIAccountScheduleDecision{}, noAvailableOpenAISelectionError(req.RequestedModel, false, "stub_exhausted")
}

func (s *sessionLimitOpenAISchedulerStub) ReportResult(int64, bool, *int) {}
func (s *sessionLimitOpenAISchedulerStub) ReportSwitch()                  {}
func (s *sessionLimitOpenAISchedulerStub) SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	return OpenAIAccountSchedulerMetricsSnapshot{}
}

func TestOpenAISchedulerSessionLimitSkipsFullAPIKeyAccount(t *testing.T) {
	cache := &sessionLimitAPITestCache{}
	cache.seed(301, "already-active")

	first := &Account{
		ID:       301,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"max_sessions": 1},
	}
	second := &Account{
		ID:       302,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"max_sessions": 1},
	}
	released := 0
	scheduler := &sessionLimitOpenAISchedulerStub{selections: []*AccountSelectionResult{
		{Account: first, Acquired: true, ReleaseFunc: func() { released++ }},
		{Account: second, Acquired: true},
	}}
	service := &OpenAIGatewayService{sessionLimitCache: cache}

	selection, _, err := service.selectWithOpenAISessionLimit(
		context.Background(), scheduler,
		OpenAIAccountScheduleRequest{SessionHash: "new-session", RequestedModel: "gpt-5.6"},
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(302), selection.Account.ID)
	require.Equal(t, 1, released, "a rejected acquired candidate must release its account slot")
	require.Equal(t, 2, scheduler.calls, "the scheduler should retry with the next candidate")
}

func TestOpenAISchedulerSessionLimitLeavesUnlimitedAPIKeyUntouched(t *testing.T) {
	cache := &sessionLimitAPITestCache{}
	account := &Account{ID: 303, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	scheduler := &sessionLimitOpenAISchedulerStub{selections: []*AccountSelectionResult{{Account: account}}}
	service := &OpenAIGatewayService{sessionLimitCache: cache}

	selection, _, err := service.selectWithOpenAISessionLimit(
		context.Background(), scheduler,
		OpenAIAccountScheduleRequest{SessionHash: "session", RequestedModel: "gpt-5.6"},
	)
	require.NoError(t, err)
	require.Equal(t, int64(303), selection.Account.ID)
	require.Zero(t, cache.calls, "max_sessions=0 must not touch Redis")
}
