package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const AccountHealthProbeExtraKey = "account_health_probe"

const (
	AccountHealthProbeStatusHealthy = "healthy"
	AccountHealthProbeStatusFailed  = "failed"
	AccountHealthProbeModeOAuth     = "openai_oauth_quota"
	AccountHealthProbeModeAPIKey    = "openai_apikey_connection"
)

type accountHealthProbeContextKey struct{}

func withAccountHealthProbeContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, accountHealthProbeContextKey{}, true)
}

func isAccountHealthProbeContext(ctx context.Context) bool {
	enabled, _ := ctx.Value(accountHealthProbeContextKey{}).(bool)
	return enabled
}

type openAIHealthQuotaService interface {
	QueryUsageForHealth(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
}

type openAIInventoryQuotaCacheService interface {
	CacheUsageSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error
	CacheResetCreditsSnapshot(ctx context.Context, accountID int64, credits *OpenAIRateLimitResetCredits) error
}

type AccountHealthProbeSnapshot struct {
	Status    string `json:"status"`
	Mode      string `json:"mode"`
	Attempts  int    `json:"attempts"`
	CheckedAt string `json:"checked_at"`
	Reason    string `json:"reason,omitempty"`
}

type AccountHealthProbeResult struct {
	AccountID       int64                       `json:"account_id"`
	Name            string                      `json:"name"`
	Platform        string                      `json:"platform"`
	Type            string                      `json:"type"`
	Healthy         bool                        `json:"healthy"`
	Dead            bool                        `json:"dead"`
	Attempts        int                         `json:"attempts"`
	Mode            string                      `json:"mode"`
	Reason          string                      `json:"reason,omitempty"`
	Snapshot        *AccountHealthProbeSnapshot `json:"snapshot,omitempty"`
	HealthPersisted bool                        `json:"health_persisted"`
}

// AccountInventoryResult extends a health probe with the sanitized quota view
// returned by /wham/usage. API Key accounts have no ChatGPT credits envelope,
// so Quota is nil while their connection health is still reported.
type AccountInventoryResult struct {
	AccountHealthProbeResult
	Quota          *OpenAIQuotaUsage `json:"quota,omitempty"`
	QuotaPersisted bool              `json:"quota_persisted"`
}

func (a *Account) HasFailedHealthProbe() bool {
	snapshot := a.HealthProbeSnapshot()
	return snapshot != nil && snapshot.Status == AccountHealthProbeStatusFailed
}

func (a *Account) HealthProbeSnapshot() *AccountHealthProbeSnapshot {
	if a == nil || a.Platform != PlatformOpenAI || a.Extra == nil {
		return nil
	}
	raw, ok := a.Extra[AccountHealthProbeExtraKey]
	if !ok || raw == nil {
		return nil
	}
	switch snapshot := raw.(type) {
	case AccountHealthProbeSnapshot:
		copy := snapshot
		return &copy
	case *AccountHealthProbeSnapshot:
		if snapshot == nil {
			return nil
		}
		copy := *snapshot
		return &copy
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snapshot AccountHealthProbeSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil || strings.TrimSpace(snapshot.Status) == "" {
		return nil
	}
	return &snapshot
}

func (s *AccountTestService) ProbeOpenAIAccountHealth(ctx context.Context, accountID int64) AccountHealthProbeResult {
	result, _ := s.probeOpenAIAccountHealth(ctx, accountID)
	return result
}

// InventoryOpenAIAccount performs the same two-attempt health confirmation as
// ProbeOpenAIAccountHealth and returns the OAuth quota/credit payload from the
// successful attempt for selected-account inventory.
func (s *AccountTestService) InventoryOpenAIAccount(ctx context.Context, accountID int64) AccountInventoryResult {
	result, quota := s.probeOpenAIAccountHealth(ctx, accountID)
	quota = inventoryQuotaForAccount(s.accountRepo, ctx, accountID, quota)
	quotaPersisted := false
	if s != nil && result.Healthy && quota != nil {
		if cache, ok := s.openAIQuotaService.(openAIInventoryQuotaCacheService); ok {
			usageErr := cache.CacheUsageSnapshot(ctx, accountID, quota)
			creditsErr := cache.CacheResetCreditsSnapshot(ctx, accountID, quota.RateLimitResetCredits)
			quotaPersisted = usageErr == nil && creditsErr == nil
			if !quotaPersisted {
				result.Reason = "quota fetched but its scheduling snapshot could not be persisted"
			}
		} else {
			result.Reason = "quota fetched but the quota cache service is unavailable"
		}
	}
	return AccountInventoryResult{AccountHealthProbeResult: result, Quota: quota, QuotaPersisted: quotaPersisted}
}

// inventoryQuotaForAccount keeps Spark/shadow inventory from showing the
// parent paid-credit envelope. Persistence already isolates spark windows;
// the API response must do the same.
func inventoryQuotaForAccount(repo AccountRepository, ctx context.Context, accountID int64, quota *OpenAIQuotaUsage) *OpenAIQuotaUsage {
	if quota == nil || repo == nil || accountID <= 0 {
		return quota
	}
	account, err := repo.GetByID(ctx, accountID)
	if err != nil || account == nil || !account.IsShadow() {
		return quota
	}
	projected := *quota
	projected.Credits = nil
	projected.RateLimit = sparkInventoryRateLimit(quota)
	return &projected
}

func sparkInventoryRateLimit(quota *OpenAIQuotaUsage) *OpenAIRateLimit {
	if quota == nil {
		return nil
	}
	for i := range quota.AdditionalRateLimits {
		item := quota.AdditionalRateLimits[i]
		if item.MeteredFeature == "codex_bengalfox" {
			return item.RateLimit
		}
	}
	return nil
}

func (s *AccountTestService) probeOpenAIAccountHealth(ctx context.Context, accountID int64) (AccountHealthProbeResult, *OpenAIQuotaUsage) {
	result := AccountHealthProbeResult{AccountID: accountID}
	if s == nil || s.accountRepo == nil {
		result.Reason = "account health service is unavailable"
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		result.Reason = err.Error()
		return result, nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Reason = ctxErr.Error()
			return result, nil
		}
		result.Reason = "account not found"
		return result, nil
	}
	result.Name = account.Name
	result.Platform = account.Platform
	result.Type = account.Type
	if account.Platform != PlatformOpenAI || (account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey) {
		result.Reason = "health mode supports OpenAI OAuth and API Key accounts only"
		return result, nil
	}
	if account.Type == AccountTypeOAuth {
		result.Mode = AccountHealthProbeModeOAuth
	} else {
		result.Mode = AccountHealthProbeModeAPIKey
	}

	var quota *OpenAIQuotaUsage
	probe := func() error {
		if account.Type == AccountTypeOAuth {
			if s.openAIQuotaService == nil {
				return fmt.Errorf("OpenAI quota service is unavailable")
			}
			usage, err := s.openAIQuotaService.QueryUsageForHealth(ctx, account.ID)
			if err == nil {
				quota = usage
			}
			return err
		}
		backgroundResult, err := s.RunTestBackground(withAccountHealthProbeContext(ctx), account.ID, "")
		if err != nil {
			return err
		}
		if backgroundResult == nil || backgroundResult.Status != AccountHealthProbeStatusHealthy && backgroundResult.Status != "success" {
			if backgroundResult != nil && strings.TrimSpace(backgroundResult.ErrorMessage) != "" {
				return fmt.Errorf("%s", strings.TrimSpace(backgroundResult.ErrorMessage))
			}
			return fmt.Errorf("OpenAI API Key connection test failed")
		}
		return nil
	}

	var lastErr error
	for result.Attempts < 2 {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		result.Attempts++
		if lastErr = probe(); lastErr == nil {
			result.Healthy = true
			break
		}
	}
	if !result.Healthy {
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Dead = false
			result.Reason = ctxErr.Error()
			return result, nil
		}
	}
	if !result.Healthy && result.Attempts < 2 {
		result.Reason = truncate(strings.TrimSpace(lastErr.Error()), 240)
		return result, nil
	}

	snapshot := &AccountHealthProbeSnapshot{
		Status:    AccountHealthProbeStatusHealthy,
		Mode:      result.Mode,
		Attempts:  result.Attempts,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if lastErr != nil {
		result.Dead = true
		result.Reason = truncate(strings.TrimSpace(lastErr.Error()), 240)
		snapshot.Status = AccountHealthProbeStatusFailed
		snapshot.Reason = result.Reason
	}
	result.Snapshot = snapshot
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{AccountHealthProbeExtraKey: snapshot}); err != nil {
		result.Healthy = false
		result.Dead = false
		result.Snapshot = nil
		result.Reason = "health result could not be persisted"
	} else {
		result.HealthPersisted = true
	}
	return result, quota
}
