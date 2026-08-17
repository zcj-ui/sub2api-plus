package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

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
	if account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
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
