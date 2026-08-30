package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountUsageServiceProbeOpenAICodexSnapshotFailsClosedForConfiguredProxy(t *testing.T) {
	proxyID := int64(71)
	tests := []struct {
		name  string
		proxy *Proxy
	}{
		{name: "missing relation"},
		{name: "mismatched relation", proxy: &Proxy{ID: proxyID + 1, Protocol: "http", Host: "127.0.0.1", Port: 8080}},
		{name: "invalid URL", proxy: &Proxy{ID: proxyID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "fixture"},
				ProxyID:     &proxyID,
				Proxy:       tt.proxy,
			}
			_, err := (&AccountUsageService{}).probeOpenAICodexSnapshot(context.Background(), account)
			if err == nil || !strings.Contains(err.Error(), "resolve openai probe proxy") {
				t.Fatalf("probeOpenAICodexSnapshot() error = %v, want configured proxy failure", err)
			}
		})
	}
}

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}
	futureAt := now.Add(2 * time.Minute).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_usage_updated_at": futureAt,
		},
	}, usage, now) {
		// A future timestamp is treated as stale; the refresh path must remain
		// available after clock skew or imported data.
		t.Fatal("expected future snapshot timestamp to trigger refresh")
	}

	weeklyOnly := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			OpenAIQuotaUsed5hPercentExtraKey:   nil,
			OpenAIQuotaReset5hSecondsExtraKey:  nil,
			OpenAIQuotaWindow5hMinutesExtraKey: nil,
			OpenAIQuotaUsed7dPercentExtraKey:   22.0,
			OpenAIQuotaWindow7dMinutesExtraKey: 10080,
			"codex_usage_updated_at":           now.Format(time.RFC3339),
			openaiQuotaCreditBalanceKey:        map[string]any{"balance": "0", "updated_at": now.Format(time.RFC3339)},
		},
	}
	if shouldRefreshOpenAICodexSnapshot(weeklyOnly, &UsageInfo{
		FiveHour: nil,
		SevenDay: &UsageProgress{Utilization: 22, WindowSeconds: 7 * 24 * 60 * 60},
	}, now) {
		t.Fatal("weekly-only snapshot with a fresh 7d window should not be probed as incomplete")
	}
}

// TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2 外审第9轮 P1:spark 影子用量走
// QueryUsage(/wham/usage,与 WSv2 无关),staleness 不得被 WSv2 门控,否则首刷后窗口永久冻结。
func TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2(t *testing.T) {
	t.Parallel()

	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}
	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	freshAt := now.Add(-time.Minute).Format(time.RFC3339)
	parentID := int64(7001)

	// 影子无 WSv2,但首刷后窗口已存在;过期 codex_usage_updated_at 必须触发再刷新。
	shadowStale := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": staleAt},
	}
	if !shouldRefreshOpenAICodexSnapshot(shadowStale, usage, now) {
		t.Fatal("expected stale spark shadow (no WSv2) to trigger refresh")
	}

	// 影子时间戳仍新鲜→不刷(TTL 生效)。
	shadowFresh := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": freshAt},
	}
	if shouldRefreshOpenAICodexSnapshot(shadowFresh, usage, now) {
		t.Fatal("expected fresh spark shadow to skip refresh (TTL not elapsed)")
	}

	// 反向对照:普通账号无 WSv2 + 过期时间戳→仍不刷(WSv2 门控普通账号的 probe 刷新)。
	normalNoWS := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"codex_usage_updated_at": staleAt},
	}
	if !shouldRefreshOpenAICodexSnapshot(normalNoWS, usage, now) {
		t.Fatal("expected stale normal account to refresh from wham without WSv2")
	}
}

func TestShouldRefreshOpenAICodexSnapshot_QueriesWhamWhenCreditSnapshotMissing(t *testing.T) {
	now := time.Now()
	usage := &UsageInfo{FiveHour: &UsageProgress{}, SevenDay: &UsageProgress{}}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Format(time.RFC3339),
		},
	}

	if !shouldRefreshOpenAICodexSnapshot(account, usage, now) {
		t.Fatal("expected missing credit snapshot to trigger an initial wham query")
	}
	account.Extra[openaiQuotaCreditBalanceKey] = map[string]any{
		"balance":    "0",
		"updated_at": now.Format(time.RFC3339),
	}
	if shouldRefreshOpenAICodexSnapshot(account, usage, now) {
		t.Fatal("expected fresh wham credit snapshot to satisfy the refresh check")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	err := svc.persistOpenAICodexProbeSnapshot(context.Background(), 321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("persistOpenAICodexProbeSnapshot() error = %v", err)
	}

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsageReleasesProbeCacheAfterFailure(t *testing.T) {
	proxyID := int64(71)
	cache := NewUsageCache()
	svc := &AccountUsageService{cache: cache}
	account := &Account{
		ID:          711,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "fixture"},
		ProxyID:     &proxyID,
	}

	_, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if _, blocked := cache.openAIProbeCache.Load(account.ID); blocked {
		t.Fatal("failed quota probe must be immediately retryable")
	}
	if !svc.shouldProbeOpenAICodexSnapshot(account.ID, time.Now()) {
		t.Fatal("expected retry admission after failed quota probe")
	}
}

func TestAccountUsageService_ReleaseOpenAIProbeDoesNotDeleteNewerReservation(t *testing.T) {
	cache := NewUsageCache()
	svc := &AccountUsageService{cache: cache}
	accountID := int64(712)
	first := time.Now().Add(-time.Second)
	second := time.Now()

	if !svc.shouldProbeOpenAICodexSnapshot(accountID, first, true) {
		t.Fatal("expected first forced probe reservation")
	}
	if !svc.shouldProbeOpenAICodexSnapshot(accountID, second, true) {
		t.Fatal("expected second forced probe reservation")
	}
	svc.releaseOpenAICodexProbe(accountID, first)

	got, ok := cache.openAIProbeCache.Load(accountID)
	if !ok || got != second {
		t.Fatalf("newer reservation = %v, %v; want %v, true", got, ok, second)
	}
}

func TestAccountUsageService_OpenAIProbeAdmissionIsAtomic(t *testing.T) {
	cache := NewUsageCache()
	svc := &AccountUsageService{cache: cache}
	now := time.Now()
	results := make(chan bool, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- svc.shouldProbeOpenAICodexSnapshot(713, now)
		}()
	}
	wg.Wait()
	close(results)

	admitted := 0
	for result := range results {
		if result {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted probes = %d, want 1", admitted)
	}
}

func TestAccountUsageService_ProbeAdmissionTreatsFutureTimestampAsStale(t *testing.T) {
	cache := NewUsageCache()
	svc := &AccountUsageService{cache: cache}
	accountID := int64(714)
	future := time.Now().Add(5 * time.Minute)
	cache.openAIProbeCache.Store(accountID, future)

	require.True(t, svc.shouldProbeOpenAICodexSnapshot(accountID, time.Now()), "future reservation must not suppress a probe")
}

func TestAccountUsageService_GrokProbeAdmissionTreatsFutureTimestampAsStale(t *testing.T) {
	cache := NewUsageCache()
	svc := &AccountUsageService{cache: cache}
	accountID := int64(715)
	future := time.Now().Add(5 * time.Minute)
	cache.grokProbeCache.Store(accountID, future)

	require.True(t, svc.shouldProbeGrokBilling(accountID, time.Now(), false), "future reservation must not suppress a Grok probe")
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("preserves upstream monthly window length", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent":   12.0,
			"codex_7d_window_minutes": 43200, // 30 days
			"codex_7d_reset_at":       now.Add(10 * 24 * time.Hour).Format(time.RFC3339),
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.WindowSeconds != 30*24*60*60 {
			t.Fatalf("expected monthly window seconds, got %d", progress.WindowSeconds)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})

	t.Run("nil tombstone hides removed window", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": nil,
			"codex_5h_reset_at":     nil,
		}
		if progress := buildCodexUsageProgressFromExtra(extra, "5h", now); progress != nil {
			t.Fatalf("nil quota tombstone must not render a zero-utilization window: %+v", progress)
		}
	})
}
