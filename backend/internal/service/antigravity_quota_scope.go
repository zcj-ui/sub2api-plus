package service

import (
	"context"
	"strings"
	"time"
)

func normalizeAntigravityModelName(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(normalized, "/publishers/google/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/google/models/"):]
	} else if idx := strings.LastIndex(normalized, "/publishers/anthropic/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/anthropic/models/"):]
	} else if idx := strings.LastIndex(normalized, "/models/"); idx != -1 {
		normalized = normalized[idx+len("/models/"):]
	} else {
		normalized = strings.TrimPrefix(normalized, "publishers/google/models/")
		normalized = strings.TrimPrefix(normalized, "publishers/anthropic/models/")
		normalized = strings.TrimPrefix(normalized, "models/")
	}
	return normalized
}

// resolveAntigravityModelKey 根据请求的模型名解析限流 key
// 返回空字符串表示无法解析
func resolveAntigravityModelKey(requestedModel string) string {
	return normalizeAntigravityModelName(requestedModel)
}

// IsSchedulableForModel 结合模型级限流判断是否可调度。
// 保持旧签名以兼容既有调用方；默认使用 context.Background()。
func (a *Account) IsSchedulableForModel(requestedModel string) bool {
	return a.IsSchedulableForModelWithContext(context.Background(), requestedModel)
}

func (a *Account) IsSchedulableForModelWithContext(ctx context.Context, requestedModel string) bool {
	if a == nil {
		return false
	}
	if !a.IsSchedulable() && !openAIImagesCanBypassThresholdPause(ctx, a) {
		return false
	}
	if a.isModelRateLimitedWithContext(ctx, requestedModel) {
		// Antigravity + overages 启用 + 积分未耗尽 → 放行（有积分可用）
		if a.Platform == PlatformAntigravity && a.IsOveragesEnabled() && !a.isCreditsExhausted() {
			return true
		}
		return false
	}
	return true
}

// IsSchedulableForCompactModelWithContext keeps the legacy
// /v1/responses/compact request independent from ordinary account-wide quota
// and 429 windows. Lifecycle, administrator schedulability, health, expiry,
// overload, temporary blocks, and model-scoped limits still apply. The
// scheduler performs the separate compact capability check after fresh
// hydration so an unsupported account is reported distinctly.
func (a *Account) IsSchedulableForCompactModelWithContext(ctx context.Context, requestedModel string) bool {
	if a == nil || !a.IsActive() || !a.Schedulable || a.HasFailedHealthProbe() {
		return false
	}
	now := time.Now()
	if a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	if a.OverloadUntil != nil && now.Before(*a.OverloadUntil) {
		return false
	}
	if a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil) {
		return false
	}
	return !a.isModelRateLimitedWithContext(ctx, requestedModel)
}

// openAIImagesCanBypassThresholdPause keeps the dedicated /v1/images/* quota
// independent from the Codex text-window auto-pause. It deliberately accepts
// only the threshold reason and rechecks every other account-level gate, so a
// 401/429/transport/admin error or an expired/overloaded account remains out of
// the image pool (#6035).
func openAIImagesCanBypassThresholdPause(ctx context.Context, account *Account) bool {
	if account == nil || !account.IsOpenAI() || !OpenAIImagesEndpointFromContext(ctx) ||
		account.TempUnschedulableUntil == nil || !time.Now().Before(*account.TempUnschedulableUntil) ||
		!IsAccountSchedulingThresholdReason(account.TempUnschedulableReason) {
		return false
	}
	if !account.IsActive() || !account.Schedulable || account.HasFailedHealthProbe() {
		return false
	}
	now := time.Now()
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return false
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return false
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return false
	}
	return !account.IsAPIKeyOrBedrock() || !account.IsQuotaExceeded()
}

// GetRateLimitRemainingTime 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTime(requestedModel string) time.Duration {
	return a.GetRateLimitRemainingTimeWithContext(context.Background(), requestedModel)
}

// GetRateLimitRemainingTimeWithContext 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTimeWithContext(ctx context.Context, requestedModel string) time.Duration {
	if a == nil {
		return 0
	}
	return a.GetModelRateLimitRemainingTimeWithContext(ctx, requestedModel)
}
