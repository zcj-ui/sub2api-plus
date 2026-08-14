package service

import (
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
