package urlvalidator

import (
	"net"
	"testing"
)

func TestValidateURLFormat(t *testing.T) {
	if _, err := ValidateURLFormat("", false); err == nil {
		t.Fatalf("expected empty url to fail")
	}
	if _, err := ValidateURLFormat("://bad", false); err == nil {
		t.Fatalf("expected invalid url to fail")
	}
	if _, err := ValidateURLFormat("http://example.com", false); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateURLFormat("https://example.com", false); err != nil {
		t.Fatalf("expected https to pass, got %v", err)
	}
	if _, err := ValidateURLFormat("http://example.com", true); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateURLFormat("https://example.com:bad", true); err == nil {
		t.Fatalf("expected invalid port to fail")
	}

	// 验证末尾斜杠被移除
	normalized, err := ValidateURLFormat("https://example.com/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected trailing slash to be removed, got %s", normalized)
	}

	// 验证多个末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com///", false)
	if err != nil {
		t.Fatalf("expected multiple trailing slashes to pass, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected all trailing slashes to be removed, got %s", normalized)
	}

	// 验证带路径的 URL 末尾斜杠被移除
	normalized, err = ValidateURLFormat("https://example.com/api/v1/", false)
	if err != nil {
		t.Fatalf("expected trailing slash url with path to pass, got %v", err)
	}
	if normalized != "https://example.com/api/v1" {
		t.Fatalf("expected trailing slash to be removed from path, got %s", normalized)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	if _, err := ValidateHTTPURL("http://example.com", false, ValidationOptions{}); err == nil {
		t.Fatalf("expected http to fail when allow_insecure_http is false")
	}
	if _, err := ValidateHTTPURL("http://example.com", true, ValidationOptions{}); err != nil {
		t.Fatalf("expected http to pass when allow_insecure_http is true, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{RequireAllowlist: true}); err == nil {
		t.Fatalf("expected require allowlist to fail when empty")
	}
	if _, err := ValidateHTTPURL("https://example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err == nil {
		t.Fatalf("expected host not in allowlist to fail")
	}
	if _, err := ValidateHTTPURL("https://api.example.com", false, ValidationOptions{AllowedHosts: []string{"api.example.com"}}); err != nil {
		t.Fatalf("expected allowlisted host to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://sub.api.example.com", false, ValidationOptions{AllowedHosts: []string{"*.example.com"}}); err != nil {
		t.Fatalf("expected wildcard allowlist to pass, got %v", err)
	}
	if _, err := ValidateHTTPURL("https://localhost", false, ValidationOptions{AllowPrivate: false}); err == nil {
		t.Fatalf("expected localhost to be blocked when allow_private_hosts is false")
	}
	if _, err := ValidateHTTPURL("http://[fe80::1%25lo]", true, ValidationOptions{AllowPrivate: true}); err == nil {
		t.Fatalf("expected IPv6 zone identifiers to be rejected")
	}
}

func TestIsBlockedIPRejectsSpecialUseRanges(t *testing.T) {
	tests := []string{
		"100.64.0.1",   // RFC 6598 CGNAT
		"198.18.0.1",   // RFC 2544 benchmarking
		"198.51.100.1", // RFC 5737 documentation
		"203.0.113.1",  // RFC 5737 documentation
		"192.0.2.1",    // RFC 5737 documentation
		"224.0.0.1",    // IPv4 multicast
		"240.0.0.1",    // IPv4 reserved
		"2001:db8::1",  // IPv6 documentation
		"2001:10::1",   // IPv6 ORCHID
		"2001:0000::1", // IPv6 Teredo
		"2001:0002::1", // IPv6 benchmarking
		"2001:0020::1", // IPv6 ORCHIDv2
		"3fff::1",      // IPv6 documentation
		"ff02::1",      // IPv6 multicast
	}
	for _, raw := range tests {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("ParseIP(%q) returned nil", raw)
		}
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q) = false, want true", raw)
		}
	}
}

func TestIsBlockedIPDoesNotOverblockAdjacentIPv6GlobalUnicast(t *testing.T) {
	for _, raw := range []string{"2001:1::1", "2001:3::1", "2001:30::1", "4000::1"} {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("ParseIP(%q) returned nil", raw)
		}
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q) = true, want false", raw)
		}
	}
}

func TestValidateHTTPURLRejectsSpecialUseLiteralTargets(t *testing.T) {
	for _, raw := range []string{
		"http://100.64.0.1/",
		"http://198.18.0.1/",
		"http://198.51.100.1/",
		"http://203.0.113.1/",
		"http://[2001:db8::1]/",
		"http://224.0.0.1/",
	} {
		if _, err := ValidateHTTPURL(raw, true, ValidationOptions{AllowPrivate: false}); err == nil {
			t.Errorf("ValidateHTTPURL(%q) unexpectedly accepted a special-use target", raw)
		}
	}
}
