package urlvalidator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ValidationOptions struct {
	AllowedHosts     []string
	RequireAllowlist bool
	AllowPrivate     bool
}

// ValidateHTTPURL validates an outbound HTTP/HTTPS URL.
//
// It provides a single validation entry point that supports:
// - scheme 校验（https 或可选允许 http）
// - 可选 allowlist（支持 *.example.com 通配）
// - allow_private_hosts 策略（阻断 localhost/私网字面量 IP）
//
// 注意：DNS Rebinding 防护（解析后 IP 校验）应在实际发起请求时执行，避免 TOCTOU。
func ValidateHTTPURL(raw string, allowInsecureHTTP bool, opts ValidationOptions) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid url: %s", trimmed)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (!allowInsecureHTTP || scheme != "http") {
		return "", fmt.Errorf("invalid url scheme: %s", parsed.Scheme)
	}

	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid host")
	}
	if !opts.AllowPrivate && isBlockedHost(host) {
		return "", fmt.Errorf("host is not allowed: %s", host)
	}

	if port := parsed.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num <= 0 || num > 65535 {
			return "", fmt.Errorf("invalid port: %s", port)
		}
	}

	allowlist := normalizeAllowlist(opts.AllowedHosts)
	if opts.RequireAllowlist && len(allowlist) == 0 {
		return "", errors.New("allowlist is not configured")
	}
	if len(allowlist) > 0 && !isAllowedHost(host, allowlist) {
		return "", fmt.Errorf("host is not allowed: %s", host)
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func ValidateURLFormat(raw string, allowInsecureHTTP bool) (string, error) {
	// 最小格式校验：仅保证 URL 可解析且 scheme 合规，不做白名单/私网/SSRF 校验
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid url: %s", trimmed)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (!allowInsecureHTTP || scheme != "http") {
		return "", fmt.Errorf("invalid url scheme: %s", parsed.Scheme)
	}

	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid host")
	}

	if port := parsed.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num <= 0 || num > 65535 {
			return "", fmt.Errorf("invalid port: %s", port)
		}
	}

	return strings.TrimRight(trimmed, "/"), nil
}

func ValidateHTTPSURL(raw string, opts ValidationOptions) (string, error) {
	return ValidateHTTPURL(raw, false, opts)
}

// ValidateResolvedIP 验证 DNS 解析后的 IP 地址是否安全
// 用于防止 DNS Rebinding 攻击：在实际 HTTP 请求时调用此函数验证解析后的 IP
func ValidateResolvedIP(host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("dns resolution failed: %w", err)
	}

	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return fmt.Errorf("resolved ip %s is not allowed", ip.String())
		}
	}
	return nil
}

// IsBlockedIP reports whether an address is unsuitable as an outbound target
// for URL/SSRF validation.  net.IP's convenience predicates intentionally do
// not classify every non-routable special-use range (for example CGNAT,
// benchmarking and documentation networks), so keep those ranges explicit.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	v4 := ip.To4()
	if v4 != nil {
		// 0/8, multicast/reserved 224/4 and the limited broadcast address.
		if v4[0] == 0 || v4[0] >= 224 {
			return true
		}
		// RFC 6598 carrier-grade NAT 100.64.0.0/10.
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// RFC 2544 benchmarking 198.18.0.0/15.
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		// RFC 5737/3849 documentation ranges and RFC 6890 special-use blocks.
		if (v4[0] == 192 && v4[1] == 0 && (v4[2] == 0 || v4[2] == 2)) ||
			(v4[0] == 198 && v4[1] == 51 && v4[2] == 100) ||
			(v4[0] == 203 && v4[1] == 0 && v4[2] == 113) ||
			(v4[0] == 192 && v4[1] == 88 && v4[2] == 99) {
			return true
		}
		return false
	}
	// IPv6 documentation (2001:db8::/32) and ORCHID (2001:10::/28) are not
	// usable public service destinations even though they are global-unicast.
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x00 {
		// Teredo (2001:0000::/32).
		return true
	}
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3]&0xf0 == 0x10 {
		return true
	}
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3]&0xf0 == 0x20 {
		// ORCHIDv2 (2001:20::/28).
		return true
	}
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x02 && ip[4] == 0x00 && ip[5] == 0x00 {
		// Benchmarking (2001:2::/48).
		return true
	}
	if len(ip) == net.IPv6len && ip[0] == 0x3f && ip[1] == 0xff && ip[2]&0xf0 == 0x00 {
		// Documentation (3fff::/20).
		return true
	}
	return false
}

func normalizeAllowlist(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, v := range values {
		entry := strings.ToLower(strings.TrimSpace(v))
		if entry == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(entry); err == nil {
			entry = host
		}
		normalized = append(normalized, entry)
	}
	return normalized
}

func isAllowedHost(host string, allowlist []string) bool {
	for _, entry := range allowlist {
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			suffix := strings.TrimPrefix(entry, "*.")
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// IsBlockedHost 报告 host 是否为 localhost、*.localhost，或回环、私网、链路本地、未指定地址的字面量 IP。
// 只判断字面量，域名的解析结果由 ValidateResolvedIP 校验。
func IsBlockedHost(host string) bool {
	return isBlockedHost(strings.ToLower(strings.TrimSpace(host)))
}

func isBlockedHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return true
		}
	}
	return false
}
