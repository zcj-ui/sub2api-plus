package service

// This file is the final body guard for the ChatGPT internal Codex HTTP
// endpoint.  Request normalization is intentionally spread across a few
// compatibility paths; doing one last, account-scoped pass immediately before
// http.NewRequest prevents a later reconstruction from reintroducing fields
// known to be rejected by chatgpt.com.

import (
	"context"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	openAIEgressForward     = "forward"
	openAIEgressPassthrough = "passthrough"
)

// stripChatGPTInternalUnsupportedFieldsAtEgress applies only to an OpenAI
// OAuth account.  API-key accounts may target api.openai.com or an arbitrary
// compatible provider, where the same top-level fields can be valid; they must
// never be rewritten by this ChatGPT-only guard.
func stripChatGPTInternalUnsupportedFieldsAtEgress(account *Account, body []byte) ([]byte, []string) {
	return stripChatGPTInternalUnsupportedFieldsAtEgressForURL(account, chatgptCodexURL, body)
}

func stripChatGPTInternalUnsupportedFieldsAtEgressForURL(account *Account, targetURL string, body []byte) ([]byte, []string) {
	if !isChatGPTInternalCodexEndpointForAccount(account, targetURL) || len(body) == 0 {
		return body, nil
	}

	result := body
	stripped := make([]string, 0, len(openAIChatGPTInternalUnsupportedFields))
	for _, field := range openAIChatGPTInternalUnsupportedFields {
		if !gjson.GetBytes(result, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(result, field)
		if err != nil {
			// Keep the original body on a per-field failure.  A malformed body
			// should be handled by the existing request validator, not silently
			// replaced with a partial serialization.
			continue
		}
		result = next
		stripped = append(stripped, field)
	}
	// The current ChatGPT internal endpoint accepts the normal truncation
	// policies (notably "auto").  Only the legacy disabled value is rejected,
	// so keep accepted policies intact at the final egress boundary as well.
	if truncation := gjson.GetBytes(result, "truncation"); truncation.Exists() &&
		truncation.Type == gjson.String && truncation.String() == "disabled" {
		if next, err := sjson.DeleteBytes(result, "truncation"); err == nil {
			result = next
			stripped = append(stripped, "truncation")
		}
	}
	return result, stripped
}

// isChatGPTInternalCodexEndpointForAccount is deliberately stricter than an
// account-type check.  OAuth credentials normally use chatgpt.com, but a
// deployment may route them through a custom relay/base URL.  Such a relay is
// an OpenAI-compatible endpoint from this layer's perspective and must retain
// the caller's optional fields rather than receiving ChatGPT-specific cleanup.
func isChatGPTInternalCodexEndpointForAccount(account *Account, targetURL string) bool {
	if account == nil || account.Platform != PlatformOpenAI || !account.IsOpenAIOAuth() {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	if !isOfficialChatGPTInternalHost(parsed.String()) {
		return false
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	return path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/")
}

func isOfficialChatGPTInternalHost(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), "."))
	return host == "chatgpt.com" || host == "chat.openai.com"
}

func logChatGPTInternalEgressStrip(ctx context.Context, account *Account, egress string, fields []string) {
	if len(fields) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("egress", strings.TrimSpace(egress)),
		zap.Strings("stripped_fields", fields),
	).Warn("OpenAI ChatGPT internal egress removed unsupported fields")
}

func applyChatGPTInternalEgressStrip(ctx context.Context, account *Account, body []byte, egress string) []byte {
	return applyChatGPTInternalEgressStripForURL(ctx, account, chatgptCodexURL, body, egress)
}

func applyChatGPTInternalEgressStripForURL(ctx context.Context, account *Account, targetURL string, body []byte, egress string) []byte {
	result, fields := stripChatGPTInternalUnsupportedFieldsAtEgressForURL(account, targetURL, body)
	if len(fields) > 0 {
		logChatGPTInternalEgressStrip(ctx, account, egress, fields)
		return result
	}
	return body
}
