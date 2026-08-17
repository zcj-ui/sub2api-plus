//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAI429FastPath_RequiresTwoOAuthResponsesBeforeCoolingDown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	firstShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
	apiKeyShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), apiKeyAccount, http.StatusTooManyRequests, http.Header{}, nil)

	require.False(t, firstShouldDisable)
	require.False(t, apiKeyShouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(apiKeyAccount))

	secondShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
	require.False(t, secondShouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAI429FastPath_GuardStaysActiveAfterConfirmedFallbackCooldown(t *testing.T) {
	tests := []struct {
		name       string
		guard      bool
		wantActive bool
		wantReason string
	}{
		{name: "enabled", guard: true, wantActive: true, wantReason: "429"},
		{name: "disabled", guard: false, wantActive: false, wantReason: "429_fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &openAI429SnapshotRepo{}
			rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimitService}
			rateLimitService.SetAccountRuntimeBlocker(svc)
			account := &Account{
				ID:       420,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{OpenAICodex429GuardEnabledExtraKey: tt.guard},
			}

			// No reset header exercises the short fallback branch. The first
			// signal is observational only; the second is the confirmed state.
			svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
			require.False(t, svc.isOpenAI429GuardRuntimeBlocked(account))

			svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
			require.Equal(t, tt.wantActive, svc.isOpenAI429GuardRuntimeBlocked(account))

			reason, ok := svc.openaiAccountRuntimeBlockReason.Load(account.ID)
			require.True(t, ok)
			require.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestOpenAI429FastPath_ConfirmationExpiresAndSuccessClearsIt(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now))
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now.Add(openAIOAuth429ConfirmationWindow+time.Second)))
	svc.clearOpenAIOAuth429Streak(account.ID)
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now.Add(openAIOAuth429ConfirmationWindow+2*time.Second)))
}

func TestOpenAI429FastPath_ConcurrentConfirmationIsAtomic(t *testing.T) {
	svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(nil, nil, nil, nil, nil)}
	now := time.Now()
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- svc.confirmOpenAIOAuth429(440, now)
		}()
	}
	wg.Wait()
	close(results)

	confirmed := 0
	for result := range results {
		if result {
			confirmed++
		}
	}
	require.Equal(t, 1, confirmed, "exactly one of two concurrent 429 responses should confirm the cooldown")
}

type sharedOpenAI429CounterCache struct {
	mu     sync.Mutex
	counts map[int64]int64
	err    error
}

func (c *sharedOpenAI429CounterCache) IncrementOpenAI429Count(_ context.Context, accountID int64, _ time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	if c.counts == nil {
		c.counts = make(map[int64]int64)
	}
	c.counts[accountID]++
	count := c.counts[accountID]
	if count >= 2 {
		delete(c.counts, accountID)
	}
	return count, nil
}

func (c *sharedOpenAI429CounterCache) ResetOpenAI429Count(_ context.Context, accountID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	delete(c.counts, accountID)
	return nil
}

func TestOpenAI429FastPath_ConfirmationIsSharedAcrossInstances(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{}
	firstRateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	firstRateLimit.SetOpenAI429CounterCache(counter)
	secondRateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	secondRateLimit.SetOpenAI429CounterCache(counter)

	first := &OpenAIGatewayService{rateLimitService: firstRateLimit}
	second := &OpenAIGatewayService{rateLimitService: secondRateLimit}
	now := time.Now()
	require.False(t, first.confirmOpenAIOAuth429(441, now))
	require.True(t, second.confirmOpenAIOAuth429(441, now.Add(time.Second)))
}

func TestOpenAI429FastPath_CacheFailureUsesLocalMirror(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAI429CounterCache(counter)
	svc := &OpenAIGatewayService{rateLimitService: rateLimit}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(442, now))
	counter.err = errors.New("redis unavailable")
	require.True(t, svc.confirmOpenAIOAuth429(442, now.Add(time.Second)))
}

func TestOpenAI429FastPath_CacheFailureThenRecoveryConfirmsSecondResponse(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{err: errors.New("redis unavailable")}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAI429CounterCache(counter)
	svc := &OpenAIGatewayService{rateLimitService: rateLimit}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(443, now))
	counter.mu.Lock()
	counter.err = nil
	counter.mu.Unlock()
	require.True(t, svc.confirmOpenAIOAuth429(443, now.Add(time.Second)), "the recovered Redis count must not replace the first local observation")
}

func TestOpenAI429FastPath_ResetFailureStartsFreshRemoteGenerationAfterRecovery(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAI429CounterCache(counter)
	svc := &OpenAIGatewayService{rateLimitService: rateLimit}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(444, now))
	counter.mu.Lock()
	counter.err = errors.New("redis unavailable during reset")
	counter.mu.Unlock()
	svc.clearOpenAIOAuth429Streak(444)

	counter.mu.Lock()
	counter.err = nil
	counter.mu.Unlock()
	require.False(t, svc.confirmOpenAIOAuth429(444, now.Add(time.Second)), "the first 429 after recovery must start a new generation")
	require.True(t, svc.confirmOpenAIOAuth429(444, now.Add(2*time.Second)))
}

func TestOpenAI429FastPath_ResetRecoveryOnSecondNewResponseStillConfirms(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAI429CounterCache(counter)
	svc := &OpenAIGatewayService{rateLimitService: rateLimit}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(445, now))
	counter.mu.Lock()
	counter.err = errors.New("redis unavailable during reset")
	counter.mu.Unlock()
	svc.clearOpenAIOAuth429Streak(445)

	require.False(t, svc.confirmOpenAIOAuth429(445, now.Add(time.Second)))
	counter.mu.Lock()
	counter.err = nil
	counter.mu.Unlock()
	require.True(t, svc.confirmOpenAIOAuth429(445, now.Add(2*time.Second)), "Redis recovery must not discard the first locally observed 429 in the new generation")
}

func TestOpenAI429FastPath_FirstResponsePersistsObservationBeforeSecondFreezes(t *testing.T) {
	repo := &openAI429SnapshotRepo{}
	rateLimitService := NewRateLimitService(repo, nil, nil, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimitService}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{
		ID:          45,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"plan_type": "plus"},
	}
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
		"X-Codex-Primary-Window-Minutes":      []string{"300"},
	}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"limit reached","plan_type":"free","resets_in_seconds":3600}}`)

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, headers, body)
	require.Zero(t, repo.rateLimitedID)
	require.NotEmpty(t, repo.updatedExtra, "the first 429 should refresh quota data without freezing the account")
	require.Equal(t, 1, repo.updatedExtraCalls)
	require.Equal(t, 1, repo.bulkUpdateCalls)
	require.Equal(t, []int64{account.ID}, repo.bulkUpdatedIDs)
	require.Equal(t, "free", repo.bulkUpdatedPayload.Credentials["plan_type"])
	require.Equal(t, "free", account.Credentials["plan_type"])
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, headers, body)
	require.Equal(t, account.ID, repo.rateLimitedID)
	require.Equal(t, 1, repo.rateLimitedCalls)
	require.Equal(t, 2, repo.updatedExtraCalls)
	require.Equal(t, 1, repo.bulkUpdateCalls, "the unchanged observed plan must not be persisted twice")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAI429FastPath_FirstResponseUsesGatewayRepositoryWithoutRateLimitService(t *testing.T) {
	repo := &openAI429SnapshotRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{
		ID:          452,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"plan_type": "plus"},
	}
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
		"X-Codex-Primary-Window-Minutes":      []string{"300"},
	}
	body := []byte(`{"error":{"type":"usage_limit_reached","plan_type":"pro"}}`)

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, headers, body)

	require.Equal(t, 1, repo.updatedExtraCalls)
	require.Equal(t, 1, repo.bulkUpdateCalls)
	require.Equal(t, "pro", account.Credentials["plan_type"])
	require.Zero(t, repo.rateLimitedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAI429FastPath_ConfirmationPrecedesCustomTempRule(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitService}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{
		ID:          451,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"temp_unschedulable_enabled": true,
			"temp_unschedulable_rules": []any{
				map[string]any{
					"error_code":       float64(http.StatusTooManyRequests),
					"keywords":         []any{"rate limit"},
					"duration_minutes": float64(10),
				},
			},
		},
	}
	body := []byte(`{"error":{"message":"rate limit reached"}}`)

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)
	require.Zero(t, repo.tempCalls, "the first 429 must not match a custom cooldown rule")
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, body)
	require.Equal(t, 1, repo.tempCalls)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAI429FastPath_OpenCodeGoUsageLimitUsesMessageResetDuration(t *testing.T) {
	repo := &rateLimit429AccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitService}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"type":"error","error":{"type":"GoUsageLimitError","message":"5-hour usage limit reached. Resets in 4hr 59min. To continue using this model now, enable usage from your available balance: https://opencode.ai/workspace/wrk_test/go"},"metadata":{"workspace":"wrk_test","limitName":"5 hour"}}`)

	before := time.Now()
	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		body,
	)
	after := time.Now()

	require.False(t, shouldDisable)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, account.ID, repo.lastRateLimitID)
	expectedResetAfter := 4*time.Hour + 59*time.Minute
	require.False(t, repo.lastRateLimitReset.Before(before.Add(expectedResetAfter-time.Second)))
	require.False(t, repo.lastRateLimitReset.After(after.Add(expectedResetAfter)))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

// TestOpenAI429FastPath_SkipsSparkShadow 外审第8轮 P1:spark 影子被选中后若 /responses 返回 429,
// 不得按 global x-codex-* 信号写内存运行时熔断(否则 spark 被冷却到 global reset、单影子场景无可用账号)。
func TestOpenAI429FastPath_SkipsSparkShadow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	parentID := int64(800)
	shadow := &Account{
		ID:              801,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	dimensionOnlyShadow := &Account{
		ID:             803,
		Platform:       PlatformOpenAI,
		Type:           AccountTypeOAuth,
		QuotaDimension: QuotaDimensionSpark,
	}
	normal := &Account{ID: 802, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "18000")
	headers.Set("x-codex-primary-window-minutes", "300")

	svc.markOpenAIOAuth429RateLimited(context.Background(), shadow, headers, nil)
	svc.handleOpenAIAccountUpstreamError(context.Background(), dimensionOnlyShadow, http.StatusTooManyRequests, headers, nil)
	svc.handleOpenAIAccountUpstreamError(context.Background(), dimensionOnlyShadow, http.StatusTooManyRequests, headers, nil)
	svc.handleOpenAIAccountUpstreamError(context.Background(), normal, http.StatusTooManyRequests, headers, nil)
	svc.handleOpenAIAccountUpstreamError(context.Background(), normal, http.StatusTooManyRequests, headers, nil)

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(shadow), "spark shadow must not be runtime-blocked by /responses global 429")
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(dimensionOnlyShadow), "spark quota dimension must be excluded even when parent linkage is missing")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(normal), "normal OpenAI OAuth account should still be runtime-blocked")
}

func TestOpenAIRuntimeBlock_AppliesToOpenAIAPIKeyWhenRateLimitServiceStopsScheduling(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_DoesNotApplyToOtherPlatforms(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_CreditsOnlyOverrideLocalThresholdReason(t *testing.T) {
	account := &Account{
		ID:          46,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			openaiQuotaCreditBalanceKey: map[string]any{
				"has_credits": true,
				"balance":     "25",
				"updated_at":  time.Now().Format(time.RFC3339),
			},
		},
	}
	require.True(t, account.HasAvailableCodexCredits())

	thresholdSvc := &OpenAIGatewayService{}
	thresholdSvc.BlockAccountScheduling(account, time.Now().Add(time.Hour), "account_scheduling_threshold")
	thresholdReason, ok := thresholdSvc.openaiAccountRuntimeBlockReason.Load(account.ID)
	require.True(t, ok)
	require.Equal(t, "account_scheduling_threshold", thresholdReason)
	require.False(t, thresholdSvc.isOpenAIAccountRuntimeBlocked(account))

	upstreamSvc := &OpenAIGatewayService{}
	upstreamSvc.BlockAccountScheduling(account, time.Now().Add(time.Hour), "429")
	require.True(t, upstreamSvc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlocker_IgnoresNonOpenAIFromRateLimitService(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	repo := &rateLimitAccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	shouldDisable := rateLimitService.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte("forbidden"))

	require.True(t, shouldDisable)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
}

// 自 #4547（issue 4527 第4点）起，临时不可调度规则命中已知模型时按模型隔离：
// 只封 (账号, 模型) 对，不再账号级一刀切；未知模型仍走账号级兜底
// （见 TestOpenAITempUnschedulable_UnknownModelKeepsAccountRuntimeBlock）。
// 池模式规则仍然生效（issue 4470）：停止同账号重试并对命中模型设临时封锁。
func TestOpenAIPoolModeTempRule_StopsSameAccountRetryAndIsolatesBlockToModel(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{
		cfg:              &config.Config{},
		rateLimitService: rateLimitService,
	}
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{
		ID:          46,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusServiceUnavailable)},
			"temp_unschedulable_enabled":   true,
			"temp_unschedulable_rules": []any{
				map[string]any{
					"error_code":       float64(http.StatusServiceUnavailable),
					"keywords":         []any{"unavailable"},
					"duration_minutes": float64(30),
				},
			},
		},
	}
	body := []byte(`{"error":{"message":"Service temporarily unavailable"}}`)
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
	}

	failoverErr := gateway.failoverOpenAIUpstreamHTTPError(
		context.Background(),
		nil,
		account,
		resp,
		body,
		"Service temporarily unavailable",
		"gpt-5.4",
	)

	require.NotNil(t, failoverErr)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Zero(t, repo.tempCalls)
	require.Equal(t, 0, repo.setErrCalls)
	require.Equal(t, StatusActive, account.Status)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"))
}

func TestOpenAIPoolModeRetryable5xx_DoesNotCreateModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       47,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(524)},
		},
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			524,
			http.Header{},
			[]byte(`{"error":{"message":"upstream timeout"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAIPoolModeNonRetryable5xx_StillCreatesModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       48,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusGatewayTimeout)},
		},
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			http.StatusServiceUnavailable,
			http.Header{},
			[]byte(`{"error":{"message":"upstream unavailable"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAINonPoolAPIKey5xx_StillCreatesModelTransientBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{
		ID:       49,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	for i := 0; i < 2; i++ {
		shouldDisable := gateway.handleOpenAIAccountUpstreamError(
			context.Background(),
			account,
			http.StatusGatewayTimeout,
			http.Header{},
			[]byte(`{"error":{"message":"upstream timeout"}}`),
			"gpt-5.4",
		)
		require.False(t, shouldDisable)
	}

	require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
}

func TestOpenAIModelNotFound_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIModelTempUnschedulable_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
}

func TestOpenAIModelTempUnschedulable_WriteFailureDoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{modelRateLimitErr: errors.New("write failed")}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIOAuth429_MatchingModelTempRuleAvoidsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()
	account.Type = AccountTypeOAuth
	account.Credentials["temp_unschedulable_rules"] = []any{
		map[string]any{
			"error_code":       float64(http.StatusTooManyRequests),
			"keywords":         []any{"model quota"},
			"duration_minutes": float64(10),
		},
	}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"model quota exhausted"}}`),
		"gpt-5.4",
	)
	require.False(t, shouldDisable)
	require.Empty(t, repo.modelRateLimitCalls)

	shouldDisable = svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"model quota exhausted"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
}

func TestOpenAIOAuth429_NonmatchingModelTempRuleKeepsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()
	account.Type = AccountTypeOAuth
	account.Credentials["temp_unschedulable_rules"] = []any{
		map[string]any{
			"error_code":       float64(http.StatusTooManyRequests),
			"keywords":         []any{"different marker"},
			"duration_minutes": float64(10),
		},
	}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"global rate limit"}}`),
		"gpt-5.4",
	)

	require.False(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	shouldDisable = svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"global rate limit"}}`),
		"gpt-5.4",
	)
	require.False(t, shouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAITempUnschedulable_UnknownModelKeepsAccountRuntimeBlock(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
	)

	require.True(t, shouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 1, repo.tempCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestOpenAIRuntimeBlock_DoesNotShortenExistingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 46, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	longUntil := time.Now().Add(10 * time.Minute)

	svc.BlockAccountScheduling(account, longUntil, "oauth_401")
	svc.BlockAccountScheduling(account, time.Time{}, "upstream_disable")

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, longUntil, actualUntil, time.Second)
}

func TestOpenAIRuntimeBlock_ClearAccountSchedulingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 47, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.ClearAccountSchedulingBlock(account.ID)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_ClearAccountSchedulingBlockResets429Streak(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 4701, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now), "first signal should remain observational")
	svc.BlockAccountScheduling(account, now.Add(time.Minute), "429")
	svc.ClearAccountSchedulingBlock(account.ID)

	// Clearing a recovered account starts a fresh confirmation generation; a
	// single new 429 must not combine with the pre-clear observation.
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now.Add(time.Second)))
}

func TestOpenAIRuntimeBlockSnapshotTracksReasonAndGeneration(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 4702, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	initial := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.False(t, initial.Active)

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "upstream_disable")
	generic := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, generic.Active)
	require.Equal(t, "upstream_disable", generic.Reason)

	// A non-429 runtime failure is stronger than a subsequent 429 signal. The
	// latter must not relabel the active block as a guard and re-enable an old
	// socket before the auth/transport condition is explicitly cleared.
	svc.BlockAccountScheduling(account, time.Now().Add(2*time.Minute), "429")
	confirmed := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, confirmed.Active)
	require.Equal(t, "upstream_disable", confirmed.Reason)
	require.Equal(t, generic.Generation, confirmed.Generation)

	lease := &openAIWSConnLease{}
	svc.stampOpenAIWSLeaseRuntimeBlockState(account.ID, lease, initial)
	require.True(t, lease.openAI429GuardActiveAtAcquire)
	require.Equal(t, confirmed.Generation, lease.openAIRuntimeBlockGeneration)

	svc.ClearAccountSchedulingBlock(account.ID)
	cleared := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.False(t, cleared.Active)
	require.Greater(t, cleared.Generation, confirmed.Generation)

	svc.BlockAccountScheduling(account, time.Now().Add(2*time.Minute), "429")
	fresh429 := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, fresh429.Active)
	require.Equal(t, "429", fresh429.Reason)
	require.Greater(t, fresh429.Generation, cleared.Generation)
}

func TestOpenAI429Guard_OnlyPreBlockPoolSocketMayProveConfirmation(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()

	account := &Account{
		ID:          4704,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey:             true,
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool}
	ap := pool.getOrCreateAccountPool(account.ID)
	oldConn := newOpenAIWSConn("guard-candidate-old", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[oldConn.id] = oldConn
	ap.mu.Unlock()

	// The block transition marks only connections that were already in the
	// pool. A concurrent dial that publishes afterwards must stay ineligible,
	// even if a later Acquire reports it as reused.
	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	newConn := newOpenAIWSConn("guard-candidate-new", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[newConn.id] = newConn
	ap.mu.Unlock()

	oldLease := &openAIWSConnLease{pool: pool, accountID: account.ID, conn: oldConn, reused: true}
	require.True(t, svc.markOpenAI429GuardConnectionProof(account, oldLease))
	require.True(t, pool.IsGuardConnPinned(account.ID, oldConn.id))

	newLease := &openAIWSConnLease{pool: pool, accountID: account.ID, conn: newConn, reused: true}
	require.False(t, svc.markOpenAI429GuardConnectionProof(account, newLease))
	require.False(t, pool.IsGuardConnPinned(account.ID, newConn.id))
}

func TestOpenAI429Guard_Repeated429KeepsCandidateGenerationUntilProof(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	account := &Account{
		ID:          4705,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey:             true,
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool}
	ap := pool.getOrCreateAccountPool(account.ID)
	oldConn := newOpenAIWSConn("guard-repeat-old", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[oldConn.id] = oldConn
	ap.mu.Unlock()

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	first := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	svc.BlockAccountScheduling(account, time.Now().Add(2*time.Minute), "429")
	second := svc.openAIAccountRuntimeBlockSnapshot(account.ID)

	require.True(t, first.Active)
	require.True(t, second.Active)
	require.Equal(t, "429", second.Reason)
	require.Equal(t, first.Generation, second.Generation, "extending the same 429 block must retain its candidate epoch")
	lease := &openAIWSConnLease{pool: pool, accountID: account.ID, conn: oldConn}
	require.True(t, svc.markOpenAI429GuardConnectionProof(account, lease))
	require.True(t, pool.IsGuardConnPinned(account.ID, oldConn.id))
}

func TestOpenAI429Guard_PermanentReservationKeepsOrdinarySchedulingBlocked(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	account := &Account{
		ID:          4707,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey: true,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool}
	require.Same(t, pool, svc.getOpenAIWSConnPool())

	ap := pool.getOrCreateAccountPool(account.ID)
	conn := newOpenAIWSConn("guard-reservation-schedule", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[conn.id] = conn
	ap.mu.Unlock()

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	snapshot := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, snapshot.Active)
	require.True(t, pool.MarkAndPinGuardConnConfirmed(account.ID, conn.id, snapshot.Generation))

	// The original cooldown may expire while the old socket stays healthy. At
	// that point normal scheduling must still skip the account; only the exact
	// guard continuation may force-acquire this connection.
	svc.openaiAccountRuntimeBlockUntil.Store(account.ID, time.Now().Add(-time.Second))
	svc.openaiAccountRuntimeBlockReason.Store(account.ID, "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.True(t, pool.IsGuardConnPinned(account.ID, conn.id))
}

func TestOpenAI429Guard_Non429BlockEvictsRetainedConnection(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	account := &Account{
		ID:          4706,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "access-token"},
		Extra: map[string]any{
			OpenAICodex429GuardEnabledExtraKey:             true,
			"openai_oauth_responses_websockets_v2_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool}
	ap := pool.getOrCreateAccountPool(account.ID)
	oldConn := newOpenAIWSConn("guard-non429-old", account.ID, nil, nil)
	ap.mu.Lock()
	ap.conns[oldConn.id] = oldConn
	ap.mu.Unlock()

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	lease := &openAIWSConnLease{pool: pool, accountID: account.ID, conn: oldConn}
	require.True(t, svc.markOpenAI429GuardConnectionProof(account, lease))
	require.True(t, pool.IsGuardConnPinned(account.ID, oldConn.id))

	svc.BlockAccountScheduling(account, time.Now().Add(2*time.Minute), "upstream_disable")
	block := svc.openAIAccountRuntimeBlockSnapshot(account.ID)
	require.True(t, block.Active)
	require.Equal(t, "upstream_disable", block.Reason)
	require.False(t, pool.IsGuardConnPinned(account.ID, oldConn.id))
	require.False(t, svc.markOpenAI429GuardConnectionProof(account, lease), "a non-429 runtime state must reject late proof publication")
	select {
	case <-oldConn.closedCh:
	default:
		t.Fatal("non-429 block must immediately evict the retained guard socket")
	}
}

func TestOpenAIRuntimeBlockClearLeavesPostReset429Streak(t *testing.T) {
	counter := &sharedOpenAI429CounterCache{}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAI429CounterCache(counter)
	svc := &OpenAIGatewayService{rateLimitService: rateLimit}
	account := &Account{ID: 4703, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	now := time.Now()

	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now))
	svc.BlockAccountScheduling(account, now.Add(time.Minute), "429")
	svc.ClearAccountSchedulingBlock(account.ID)

	// The post-clear observation starts a fresh remote/local generation; the
	// following signal, rather than the pre-clear one, is the confirmer.
	require.False(t, svc.confirmOpenAIOAuth429(account.ID, now.Add(time.Second)))
	require.True(t, svc.confirmOpenAIOAuth429(account.ID, now.Add(2*time.Second)))
}

func TestShouldStopOpenAIOAuth429Failover_OnlyDuringStorm(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	var state OpenAIOAuth429FailoverState

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))

	for i := 0; i < openAIOAuth429StormThreshold; i++ {
		svc.recordOpenAIOAuth429()
	}

	require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
}

func TestShouldStopOpenAIOAuth429Failover_TracksOneGrokFollowupAttempt(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformGrok, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 45, Platform: PlatformGrok, Type: AccountTypeAPIKey}

	t.Run("429 then 500 stops after one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 2, &state))
	})

	t.Run("500 then 429 still allows one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 2, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusBadGateway, 3, &state))
	})

	t.Run("OAuth 429 then API-key failure consumes the same followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusInternalServerError, 2, &state))
	})

	var state OpenAIOAuth429FailoverState
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 2, &state))
}
