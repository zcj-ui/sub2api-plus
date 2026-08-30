package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/imroc/req/v3"
)

const openAIImageURLResolveTimeout = 3 * time.Second

// validateOpenAIImageDownloadURL validates an image URL before the gateway
// asks an upstream account's HTTP client to fetch it.  Image download URLs are
// partly supplied by clients (and may also be returned by an upstream), so a
// scheme/extension check alone is not enough: private, loopback, link-local,
// and DNS-rebinding targets must never become an outbound request from the
// gateway.
//
// The returned string is the trimmed original URL.  We intentionally do not
// rewrite its path or query because signed image URLs can include a
// path-sensitive signature.
func validateOpenAIImageDownloadURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("image download URL is empty")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil || parsed.Host == "" {
		return "", errors.New("image download URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("image download URL scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("image download URL must not contain credentials or a fragment")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("image download URL host is invalid")
	}

	// Validate the literal host and port first.  AllowPrivate=false blocks
	// localhost and all private/link-local IP literals, including cloud
	// metadata addresses such as 169.254.169.254.
	if _, err := urlvalidator.ValidateHTTPURL(trimmed, true, urlvalidator.ValidationOptions{
		AllowPrivate: false,
	}); err != nil {
		return "", fmt.Errorf("image download URL is unsafe: %w", err)
	}

	if ip := net.ParseIP(host); ip != nil {
		if isOpenAIImageBlockedIP(ip) {
			return "", fmt.Errorf("image download URL resolves to a blocked address")
		}
		return trimmed, nil
	}

	// Resolve every address and reject the hostname if *any* answer is private.
	// Rejecting mixed public/private answers prevents an attacker from winning a
	// DNS race by returning a different address on a later lookup.
	ctx, cancel := context.WithTimeout(context.Background(), openAIImageURLResolveTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		if err == nil {
			err = errors.New("no addresses returned")
		}
		return "", fmt.Errorf("image download URL host cannot be resolved: %w", err)
	}
	for _, ip := range ips {
		if isOpenAIImageBlockedIP(ip) {
			return "", errors.New("image download URL resolves to a blocked address")
		}
	}
	return trimmed, nil
}

func isOpenAIImageBlockedIP(ip net.IP) bool {
	return urlvalidator.IsBlockedIP(ip)
}

// openAIImageDownloadRedirectPolicy applies the same validation to every
// redirect hop.  Without this callback a public image URL could redirect to
// a private target after the initial validation.
func openAIImageDownloadRedirectPolicy(next *http.Request, _ []*http.Request) error {
	if next == nil || next.URL == nil {
		return errors.New("image download redirect has no URL")
	}
	_, err := validateOpenAIImageDownloadURL(next.URL.String())
	return err
}

// safeOpenAIImageClient clones the account client before installing a
// redirect policy.  Mutating the shared req.Client would race with concurrent
// image requests and could affect unrelated upstream calls.
func safeOpenAIImageClient(client *req.Client) (*req.Client, error) {
	if client == nil {
		return nil, errors.New("image download client is not configured")
	}
	clone := client.Clone()
	if clone == nil {
		return nil, errors.New("image download client clone failed")
	}
	if clone.GetTransport() == nil {
		return nil, errors.New("image download client transport is not configured")
	}
	clone.SetRedirectPolicy(openAIImageDownloadRedirectPolicy)
	// Re-check immediately before each actual round trip as well as in the
	// redirect callback. This narrows the DNS-rebinding window between the
	// initial validation and the transport's own name resolution, without
	// mutating the shared account client.
	clone.GetTransport().WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
		return func(request *http.Request) (*http.Response, error) {
			if request == nil || request.URL == nil {
				return nil, errors.New("image download request has no URL")
			}
			if _, err := validateOpenAIImageDownloadURL(request.URL.String()); err != nil {
				return nil, err
			}
			return next.RoundTrip(request)
		}
	})
	return clone, nil
}
