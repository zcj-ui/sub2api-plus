package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

func validateConfiguredOpenAIProxy(proxy *Proxy) error {
	if proxy == nil {
		return fmt.Errorf("account proxy is configured but unavailable")
	}
	now := time.Now()
	// Repository-hydrated proxies always carry a status. A few legacy
	// in-memory projections omit it; keep those readable for compatibility,
	// while rejecting every explicit non-active state before OpenAI can fall
	// back to direct egress.
	if status := strings.TrimSpace(proxy.Status); status != "" && !proxy.IsActive() {
		return fmt.Errorf("account proxy is not active (status=%s)", strings.TrimSpace(proxy.Status))
	}
	if proxy.IsExpired(now) {
		return fmt.Errorf("account proxy is expired")
	}
	return nil
}

// resolveConfiguredProxyURL keeps account traffic pinned to its configured
// proxy. A missing or stale relation must be surfaced as a routing failure
// instead of silently changing the account's egress IP to direct.
func resolveConfiguredProxyURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is unavailable")
	}
	if account.ProxyID == nil {
		return "", nil
	}
	if account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
		return "", fmt.Errorf("account proxy is configured but unavailable")
	}
	if account.IsOpenAI() {
		if err := validateConfiguredOpenAIProxy(account.Proxy); err != nil {
			return "", err
		}
	}
	proxyURL := strings.TrimSpace(account.Proxy.URL())
	if proxyURL == "" {
		return "", fmt.Errorf("account proxy URL is unavailable")
	}
	normalized, parsed, err := proxyurl.Parse(proxyURL)
	if err != nil || parsed == nil {
		return "", fmt.Errorf("account proxy URL is invalid: %w", err)
	}
	return normalized, nil
}

// resolveConfiguredProxyURLWithLookup hydrates an explicitly configured proxy
// when the account was loaded without its relation. It retains the fail-closed
// contract: a missing, mismatched, or invalid configured proxy never becomes a
// direct connection.
func resolveConfiguredProxyURLWithLookup(ctx context.Context, account *Account, proxyRepo ProxyRepository) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is unavailable")
	}
	if account.ProxyID == nil {
		return "", nil
	}
	// OpenAI requests are fail-closed against proxy mutations. When a repository
	// is available, refresh even a matching relation so a scheduler/handler
	// snapshot cannot keep using a proxy that was just disabled or expired.
	if account.Proxy == nil || account.Proxy.ID != *account.ProxyID || (account.IsOpenAI() && proxyRepo != nil) {
		if proxyRepo == nil {
			return "", fmt.Errorf("account proxy is configured but unavailable")
		}
		proxy, err := proxyRepo.GetByID(ctx, *account.ProxyID)
		if err != nil {
			return "", fmt.Errorf("load configured account proxy: %w", err)
		}
		if proxy == nil {
			return "", fmt.Errorf("account proxy is configured but unavailable")
		}
		account.Proxy = proxy
	}
	return resolveConfiguredProxyURL(account)
}

// resolveRequiredOpenAIProxyURL preserves the OpenAI-specific helper used by
// WebSocket paths. Accounts without a configured proxy keep their direct
// behavior; once ProxyID is set, a missing or mismatched proxy fails closed.
func resolveRequiredOpenAIProxyURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is unavailable")
	}
	return resolveConfiguredProxyURL(account)
}

// resolveOpenAIAccountProxyURL applies configured-proxy pinning to OpenAI while
// preserving the existing direct behavior of accounts without ProxyID.
func resolveOpenAIAccountProxyURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is unavailable")
	}
	return resolveConfiguredProxyURL(account)
}

// resolveProxyIDURL looks up an explicitly selected proxy. A missing or
// invalid proxy is a routing failure; nil proxyID stays direct.
func resolveProxyIDURL(ctx context.Context, proxyRepo ProxyRepository, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	account := &Account{ProxyID: proxyID}
	return resolveConfiguredProxyURLWithLookup(ctx, account, proxyRepo)
}
