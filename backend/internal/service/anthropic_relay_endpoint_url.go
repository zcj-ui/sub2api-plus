package service

import (
	"net/url"
	"strings"
)

// buildAnthropicRelayEndpointURL resolves sibling Anthropic-compatible relay
// endpoints without duplicating /v1. It accepts a relay root, /v1, a complete
// /v1/messages or /v1/models URL, and arbitrary path prefixes. Query values are
// preserved because some reverse proxies use them for route metadata.
func buildAnthropicRelayEndpointURL(base string, endpoint string) string {
	normalized := strings.TrimSpace(base)
	endpoint = "/" + strings.TrimLeft(strings.TrimSpace(endpoint), "/")
	relative := strings.TrimPrefix(endpoint, "/v1")
	parsed, err := url.Parse(normalized)
	if err != nil {
		return strings.TrimRight(normalized, "/") + endpoint
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, sibling := range []string{"/v1/messages", "/v1/models"} {
		if strings.HasSuffix(path, sibling) {
			path = strings.TrimSuffix(path, sibling) + endpoint
			parsed.Path = path
			parsed.RawPath = ""
			parsed.Fragment = ""
			return parsed.String()
		}
	}
	if strings.HasSuffix(path, endpoint) || strings.HasSuffix(path, relative) {
		parsed.Path = path
		parsed.RawPath = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	if strings.HasSuffix(path, "/v1") {
		path += relative
	} else {
		path += endpoint
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}
