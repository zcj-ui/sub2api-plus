//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

type healthProbeRepo struct {
	mockAccountRepoForGemini
	updates     []map[string]any
	updateErr   error
	setErrorID  int64
	rateLimitID int64
}

func (r *healthProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updates = append(r.updates, updates)
	return r.updateErr
}

func (r *healthProbeRepo) SetError(_ context.Context, id int64, _ string) error {
	r.setErrorID = id
	return nil
}

func (r *healthProbeRepo) SetRateLimited(_ context.Context, id int64, _ time.Time) error {
	r.rateLimitID = id
	return nil
}

type healthQuotaStub struct {
	errors   []error
	calls    int
	cancel   context.CancelFunc
	cancelAt int
	usage    *OpenAIQuotaUsage
}

func (s *healthQuotaStub) QueryUsageForHealth(context.Context, int64) (*OpenAIQuotaUsage, error) {
	s.calls++
	if s.cancel != nil && (s.cancelAt == 0 || s.calls == s.cancelAt) {
		s.cancel()
	}
	if len(s.errors) > 0 {
		err := s.errors[0]
		s.errors = s.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.usage != nil {
		return s.usage, nil
	}
	return &OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{}}, nil
}

type inventoryQuotaStub struct {
	*healthQuotaStub
	usageCacheIDs []int64
	resetCacheIDs []int64
	resetCredits  *OpenAIRateLimitResetCredits
	usageCacheErr error
	resetCacheErr error
}

func (s *inventoryQuotaStub) CacheUsageSnapshot(_ context.Context, accountID int64, _ *OpenAIQuotaUsage) error {
	s.usageCacheIDs = append(s.usageCacheIDs, accountID)
	return s.usageCacheErr
}

func (s *inventoryQuotaStub) CacheResetCreditsSnapshot(_ context.Context, accountID int64, credits *OpenAIRateLimitResetCredits) error {
	s.resetCacheIDs = append(s.resetCacheIDs, accountID)
	s.resetCredits = credits
	return s.resetCacheErr
}

func TestProbeOpenAIAccountHealthOAuthRetriesOnceThenFails(t *testing.T) {
	account := &Account{ID: 1, Name: "dead oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{1: account}}}
	quota := &healthQuotaStub{errors: []error{errors.New("first reset-credit failure"), errors.New("second reset-credit failure")}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), 1)

	require.False(t, result.Healthy)
	require.True(t, result.Dead)
	require.Equal(t, 2, result.Attempts)
	require.Equal(t, 2, quota.calls)
	require.Contains(t, result.Reason, "second reset-credit failure")
	require.Len(t, repo.updates, 1)
	snapshot, ok := repo.updates[0][AccountHealthProbeExtraKey].(*AccountHealthProbeSnapshot)
	require.True(t, ok)
	require.Equal(t, AccountHealthProbeStatusFailed, snapshot.Status)
}

func TestProbeOpenAIAccountHealthOAuthSecondAttemptRecovers(t *testing.T) {
	account := &Account{ID: 2, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{2: account}}}
	quota := &healthQuotaStub{errors: []error{errors.New("transient"), nil}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), 2)

	require.True(t, result.Healthy)
	require.False(t, result.Dead)
	require.Equal(t, 2, result.Attempts)
	require.Equal(t, AccountHealthProbeStatusHealthy, result.Snapshot.Status)
}

func TestProbeOpenAIAccountHealthOAuthSuccessClearsPending429Streak(t *testing.T) {
	account := &Account{ID: 22, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{22: account}}}
	quota := &healthQuotaStub{usage: &OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{}}}
	rateLimits := NewRateLimitService(nil, nil, nil, nil, nil)
	// Seed exactly one explicit 429. A successful authoritative health probe
	// must reset it before a later transient 429 can be considered a second hit.
	require.False(t, rateLimits.confirmOpenAIOAuth429Context(context.Background(), account.ID, time.Now()))
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota, rateLimitService: rateLimits}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), account.ID)
	require.True(t, result.Healthy)
	require.False(t, rateLimits.confirmOpenAIOAuth429Context(context.Background(), account.ID, time.Now()))
}

func TestInventoryOpenAIAccountReturnsAndCachesOAuthQuota(t *testing.T) {
	account := &Account{ID: 8, Name: "oauth inventory", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{8: account}}}
	usage := &OpenAIQuotaUsage{
		Credits:               &OpenAICodexCredits{HasCredits: true, Balance: "250"},
		RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 3},
	}
	quota := &inventoryQuotaStub{healthQuotaStub: &healthQuotaStub{usage: usage}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.InventoryOpenAIAccount(context.Background(), 8)

	require.True(t, result.Healthy)
	require.Same(t, usage, result.Quota)
	require.Equal(t, "250", result.Quota.Credits.Balance)
	require.Equal(t, []int64{8}, quota.usageCacheIDs)
	require.Equal(t, []int64{8}, quota.resetCacheIDs)
	require.Same(t, usage.RateLimitResetCredits, quota.resetCredits)
	require.True(t, result.HealthPersisted)
	require.True(t, result.QuotaPersisted)
	require.Len(t, repo.updates, 1, "health snapshot remains persisted independently from quota cache stubs")
}

func TestInventoryOpenAIAccountReportsQuotaPersistenceFailure(t *testing.T) {
	account := &Account{ID: 18, Name: "oauth inventory", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{18: account}}}
	usage := &OpenAIQuotaUsage{RateLimitResetCredits: &OpenAIRateLimitResetCredits{}}
	quota := &inventoryQuotaStub{
		healthQuotaStub: &healthQuotaStub{usage: usage},
		usageCacheErr:   errors.New("database unavailable"),
	}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.InventoryOpenAIAccount(context.Background(), 18)

	require.True(t, result.Healthy)
	require.False(t, result.QuotaPersisted)
	require.Contains(t, result.Reason, "could not be persisted")
}

func TestInventoryOpenAIAccountTreatsMissingResetCreditEnvelopeAsPersisted(t *testing.T) {
	account := &Account{ID: 19, Name: "oauth inventory without reset cards", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{19: account}}}
	usage := &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 18000}},
		// The upstream may omit rate_limit_reset_credits entirely.  This is a
		// successful quota snapshot and should not make inventory persistence
		// appear failed merely because there is no reset-card row to cache.
		RateLimitResetCredits: nil,
	}
	quota := &inventoryQuotaStub{healthQuotaStub: &healthQuotaStub{usage: usage}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.InventoryOpenAIAccount(context.Background(), 19)

	require.True(t, result.Healthy)
	require.True(t, result.QuotaPersisted)
	require.Equal(t, []int64{19}, quota.usageCacheIDs)
	require.Empty(t, quota.resetCacheIDs, "nil reset-credit envelope should not invoke a cache write")
	require.Empty(t, result.Reason)
}

func TestInventoryOpenAIAccountHidesParentCreditsForSparkShadow(t *testing.T) {
	parentID := int64(7)
	shadow := &Account{
		ID:              8,
		Name:            "spark inventory",
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{8: shadow}}}
	usage := &OpenAIQuotaUsage{
		Credits: &OpenAICodexCredits{HasCredits: true, Balance: "250"},
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 90},
		},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{
			{
				MeteredFeature: "codex_bengalfox",
				RateLimit:      &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 12}},
			},
		},
	}
	quota := &inventoryQuotaStub{healthQuotaStub: &healthQuotaStub{usage: usage}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.InventoryOpenAIAccount(context.Background(), 8)

	require.True(t, result.Healthy)
	require.NotNil(t, result.Quota)
	require.Nil(t, result.Quota.Credits)
	require.NotNil(t, result.Quota.RateLimit)
	require.InDelta(t, 12.0, result.Quota.RateLimit.PrimaryWindow.UsedPercent, 1e-9)
	require.Equal(t, []int64{8}, quota.usageCacheIDs)
}

func TestInventoryOpenAIAccountSkipsQuotaCacheForUnsupportedAccount(t *testing.T) {
	account := &Account{ID: 9, Name: "claude", Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{9: account}}}
	quota := &inventoryQuotaStub{healthQuotaStub: &healthQuotaStub{usage: &OpenAIQuotaUsage{}}}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.InventoryOpenAIAccount(context.Background(), 9)

	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.Nil(t, result.Quota)
	require.Empty(t, quota.usageCacheIDs)
	require.Empty(t, quota.resetCacheIDs)
}

func TestProbeOpenAIAccountHealthCancellationDoesNotCountAsSecondFailure(t *testing.T) {
	account := &Account{ID: 5, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{5: account}}}
	ctx, cancel := context.WithCancel(context.Background())
	quota := &healthQuotaStub{errors: []error{errors.New("first failure")}, cancel: cancel}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.ProbeOpenAIAccountHealth(ctx, 5)

	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.Equal(t, 1, result.Attempts)
	require.Equal(t, 1, quota.calls)
	require.Contains(t, result.Reason, "context canceled")
	require.Nil(t, result.Snapshot)
	require.Empty(t, repo.updates, "an incomplete probe must not persist a dead snapshot")
}

func TestProbeOpenAIAccountHealthCanceledBeforeLookupIsSkipped(t *testing.T) {
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{
		10: {ID: 10, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := (&AccountTestService{accountRepo: repo, openAIQuotaService: &healthQuotaStub{}}).ProbeOpenAIAccountHealth(ctx, 10)

	require.Equal(t, int64(10), result.AccountID)
	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.Zero(t, result.Attempts)
	require.Equal(t, context.Canceled.Error(), result.Reason)
	require.Empty(t, repo.updates)
}

func TestProbeOpenAIAccountHealthCancellationDuringSecondAttemptIsSkipped(t *testing.T) {
	account := &Account{ID: 15, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{15: account}}}
	ctx, cancel := context.WithCancel(context.Background())
	quota := &healthQuotaStub{
		errors:   []error{errors.New("first failure"), context.Canceled},
		cancel:   cancel,
		cancelAt: 2,
	}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: quota}

	result := svc.ProbeOpenAIAccountHealth(ctx, 15)

	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.Equal(t, 2, result.Attempts)
	require.Equal(t, context.Canceled.Error(), result.Reason)
	require.Nil(t, result.Snapshot)
	require.Empty(t, repo.updates)
}

func TestProbeOpenAIAccountHealthAPIKeyUsesConfiguredCompatPathTwice(t *testing.T) {
	proxyID := int64(31)
	account := &Account{
		ID:          3,
		Name:        "dead api key",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		ProxyID:     &proxyID,
		Proxy:       &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 1080},
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://relay.example"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{3: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"dead key"}}`))},
		{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"dead key"}}`))},
	}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), 3)

	require.True(t, result.Dead)
	require.Equal(t, AccountHealthProbeModeAPIKey, result.Mode)
	require.Equal(t, 2, result.Attempts)
	require.Len(t, upstream.requests, 2)
	require.Zero(t, repo.setErrorID, "health confirmation must not mark the account dead after the first 401")
	for _, request := range upstream.requests {
		require.Equal(t, "https://relay.example/v1/chat/completions", request.URL.String())
	}
	require.Equal(t, []string{"http://127.0.0.1:1080", "http://127.0.0.1:1080"}, upstream.proxyURLs)
}

func TestProbeOpenAIAccountHealthAPIKeyResponses429DoesNotPersistRateLimit(t *testing.T) {
	proxyID := int64(32)
	account := &Account{
		ID:          6,
		Name:        "rate limited api key",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		ProxyID:     &proxyID,
		Proxy:       &Proxy{ID: proxyID, Protocol: "socks5", Host: "127.0.0.1", Port: 1081},
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://relay.example/v1"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
	}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{6: account}}}
	new429Response := func() *http.Response {
		response := newJSONResponse(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
		response.Header.Set("x-codex-primary-used-percent", "100")
		response.Header.Set("x-codex-primary-reset-after-seconds", "60")
		return response
	}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{new429Response(), new429Response()}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), 6)

	require.True(t, result.Dead)
	require.Equal(t, AccountHealthProbeModeAPIKey, result.Mode)
	require.Equal(t, 2, result.Attempts)
	require.Len(t, upstream.requests, 2)
	require.Zero(t, repo.rateLimitID, "health confirmation must not persist normal request rate-limit state")
	for _, request := range upstream.requests {
		require.Equal(t, "https://relay.example/v1/responses", request.URL.String())
	}
	require.Equal(t, []string{"socks5h://127.0.0.1:1081", "socks5h://127.0.0.1:1081"}, upstream.proxyURLs)
}

func TestAccountTestProxyURLRejectsMissingConfiguredProxy(t *testing.T) {
	proxyID := int64(33)
	_, err := accountTestProxyURL(&Account{ProxyID: &proxyID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "proxy")
}

func TestAccountTestProxyURLPreservesOpenAIAccountWithoutProxy(t *testing.T) {
	url, err := accountTestProxyURL(&Account{Platform: PlatformOpenAI})
	require.NoError(t, err)
	require.Empty(t, url)
}

func TestProbeOpenAIAccountHealthSkipsUnsupportedAccount(t *testing.T) {
	account := &Account{ID: 4, Name: "claude", Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{4: account}}}
	result := (&AccountTestService{accountRepo: repo}).ProbeOpenAIAccountHealth(context.Background(), 4)
	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.Contains(t, result.Reason, "supports OpenAI")
	require.Empty(t, repo.updates)
}

func TestProbeOpenAIAccountHealthReportsPersistenceFailure(t *testing.T) {
	account := &Account{ID: 5, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{5: account}},
		updateErr:                errors.New("database unavailable"),
	}
	svc := &AccountTestService{accountRepo: repo, openAIQuotaService: &healthQuotaStub{}}

	result := svc.ProbeOpenAIAccountHealth(context.Background(), 5)

	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.False(t, result.HealthPersisted)
	require.Equal(t, "health result could not be persisted", result.Reason)
	require.Nil(t, result.Snapshot)
}

func TestProbeOpenAIAccountHealthDeadResultRequiresPersistence(t *testing.T) {
	account := &Account{ID: 25, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := &healthProbeRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{25: account}},
		updateErr:                errors.New("database unavailable"),
	}
	quota := &healthQuotaStub{errors: []error{errors.New("first failure"), errors.New("second failure")}}
	result := (&AccountTestService{accountRepo: repo, openAIQuotaService: quota}).ProbeOpenAIAccountHealth(context.Background(), 25)

	require.False(t, result.Healthy)
	require.False(t, result.Dead)
	require.False(t, result.HealthPersisted)
	require.Nil(t, result.Snapshot)
	require.Equal(t, "health result could not be persisted", result.Reason)
}

func TestAccountFailedHealthProbeControlsOpenAIScheduling(t *testing.T) {
	account := &Account{
		ID:          7,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			AccountHealthProbeExtraKey: map[string]any{"status": AccountHealthProbeStatusFailed},
		},
	}

	require.True(t, account.HasFailedHealthProbe())
	require.False(t, account.IsSchedulable())
	account.Extra[AccountHealthProbeExtraKey] = &AccountHealthProbeSnapshot{Status: AccountHealthProbeStatusHealthy}
	require.False(t, account.HasFailedHealthProbe())
	require.True(t, account.IsSchedulable())

	account.Platform = PlatformAnthropic
	account.Extra[AccountHealthProbeExtraKey] = map[string]any{"status": AccountHealthProbeStatusFailed}
	require.False(t, account.HasFailedHealthProbe(), "OpenAI health probes must not alter Claude scheduling")
	require.True(t, account.IsSchedulable())
}
