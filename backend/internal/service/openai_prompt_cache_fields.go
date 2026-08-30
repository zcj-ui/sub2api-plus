package service

// Prompt-cache fields have two independent compatibility dimensions:
// the final model's cache schema and the actual upstream host.  Account type
// alone is not enough because an OpenAI API-key account may point at an
// arbitrary OpenAI-compatible relay. This file keeps the model/capability
// policy pure and applies it at the final OpenAI API-key egress boundary.

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

var openAIPromptCacheLegacyModels = map[string]struct{}{
	"gpt-5.5":             {},
	"gpt-5.5-pro":         {},
	"gpt-5.4":             {},
	"gpt-5.2":             {},
	"gpt-5.1-codex-max":   {},
	"gpt-5.1":             {},
	"gpt-5.1-codex":       {},
	"gpt-5.1-codex-mini":  {},
	"gpt-5.1-chat-latest": {},
	"gpt-5":               {},
	"gpt-5-codex":         {},
	"gpt-4.1":             {},
}

// isOpenAIPromptCacheGPT56OrLater recognizes numeric GPT-5.6+ versions while
// rejecting arbitrary aliases.  Our local -sol/-terra/-luna model suffixes and
// strict YYYY-MM-DD snapshots are accepted because they are stable catalog
// forms; free-form suffixes remain default-deny.
func isOpenAIPromptCacheGPT56OrLater(model string) bool {
	model = strings.ToLower(strings.TrimSpace(lastOpenAIModelSegment(model)))
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(model, "gpt-")
	dot := strings.IndexByte(version, '.')
	if dot <= 0 {
		return false
	}
	major, err := strconv.Atoi(version[:dot])
	if err != nil {
		return false
	}
	minorAndSuffix := version[dot+1:]
	minorText := minorAndSuffix
	suffix := ""
	if dash := strings.IndexByte(minorAndSuffix, '-'); dash >= 0 {
		minorText = minorAndSuffix[:dash]
		suffix = minorAndSuffix[dash:]
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || (suffix != "" && !isOpenAIPromptCacheStructuredSuffix(suffix)) {
		return false
	}
	return major > 5 || (major == 5 && minor >= 6)
}

func isOpenAIPromptCacheStructuredSuffix(suffix string) bool {
	if suffix == "-sol" || suffix == "-terra" || suffix == "-luna" {
		return true
	}
	if len(suffix) != len("-2026-08-01") || suffix[0] != '-' ||
		!isOpenAIPromptCacheDigits(suffix[1:5]) || suffix[5] != '-' ||
		!isOpenAIPromptCacheDigits(suffix[6:8]) || suffix[8] != '-' ||
		!isOpenAIPromptCacheDigits(suffix[9:11]) {
		return false
	}
	return true
}

func isOpenAIPromptCacheDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isOpenAIPromptCacheLegacyModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(lastOpenAIModelSegment(model)))
	if _, ok := openAIPromptCacheLegacyModels[model]; ok {
		return true
	}
	for legacy := range openAIPromptCacheLegacyModels {
		suffix := strings.TrimPrefix(model, legacy)
		if strings.HasPrefix(model, legacy) && len(suffix) == len("-2000-01-01") &&
			suffix[0] == '-' && isOpenAIPromptCacheDigits(suffix[1:5]) && suffix[5] == '-' &&
			isOpenAIPromptCacheDigits(suffix[6:8]) && suffix[8] == '-' &&
			isOpenAIPromptCacheDigits(suffix[9:11]) {
			return true
		}
	}
	return false
}

// supportsOpenAIPromptCacheRetention requires an explicit capability entry.
// The general endpoint capability helper intentionally treats missing values
// as "all capabilities" for routing compatibility, but this optional field is
// default-deny by design and therefore checks the raw presence directly.
func supportsOpenAIPromptCacheRetention(account *Account, upstreamModel string) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		isOpenAIPromptCacheGPT56OrLater(upstreamModel) || !isOpenAIPromptCacheLegacyModel(upstreamModel) {
		return false
	}
	capabilities, found := account.openAIEndpointCapabilitySet()
	return found && capabilities[string(OpenAIEndpointCapabilityPromptCacheRetention)]
}

// normalizeOpenAIPromptCacheFields applies the model/account policy without
// making a host decision.  Callers handling arbitrary relays must use the
// *ForEgress wrapper below so custom API-key relays remain untouched.
func normalizeOpenAIPromptCacheFields(body map[string]any, account *Account, upstreamModel string) bool {
	if body == nil {
		return false
	}
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel, _ = body["model"].(string)
	}
	keepOptions := account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		isOpenAIPromptCacheGPT56OrLater(upstreamModel)
	keepRetention := supportsOpenAIPromptCacheRetention(account, upstreamModel)
	changed := false
	if !keepRetention {
		if _, ok := body["prompt_cache_retention"]; ok {
			delete(body, "prompt_cache_retention")
			changed = true
		}
	}
	if !keepOptions {
		if _, ok := body["prompt_cache_options"]; ok {
			delete(body, "prompt_cache_options")
			changed = true
		}
	}
	return changed
}

func openAIPromptCacheFieldsPresent(body []byte) bool {
	values := gjson.GetManyBytes(body, "prompt_cache_retention", "prompt_cache_options")
	if values[0].Exists() || values[1].Exists() {
		return true
	}
	// WebSocket session.update frames carry the same optional fields under a
	// nested `session` object. Detect that shape so the final-model pass is not
	// skipped just because the root frame itself has no cache hint.
	nested := gjson.GetManyBytes(body, "session.prompt_cache_retention", "session.prompt_cache_options")
	return nested[0].Exists() || nested[1].Exists()
}

// normalizeOpenAIPromptCacheFieldsForEgress applies the capability policy to
// OpenAI API-key Responses egress regardless of the configured base host. The
// final model/capability decision is what matters for compatible relays: a
// legacy model without the explicit retention capability must not receive the
// field, while GPT-5.6+ may keep prompt_cache_options. OAuth/Codex traffic is
// handled by the ChatGPT-internal guard and is intentionally excluded here.
func normalizeOpenAIPromptCacheFieldsForEgress(body []byte, account *Account, targetURL string) ([]byte, bool, error) {
	return normalizeOpenAIPromptCacheFieldsForEgressWithModel(body, account, targetURL, strings.TrimSpace(gjson.GetBytes(body, "model").String()), "")
}

// normalizeOpenAIPromptCacheFieldsForEgressWithModel is used by WebSocket
// frames whose model may be inherited from a session-level mapping.  The URL
// is retained as an explicit boundary argument so future host policy changes
// cannot accidentally require call-site rewrites.
func normalizeOpenAIPromptCacheFieldsForEgressWithModel(body []byte, account *Account, targetURL, upstreamModel, sessionUpstreamModel string) ([]byte, bool, error) {
	_ = targetURL
	if len(body) == 0 || !openAIPromptCacheFieldsPresent(body) || account == nil ||
		account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return body, false, nil
	}
	return normalizeOpenAIPromptCacheFieldsRawWithSessionModel(body, account, strings.TrimSpace(upstreamModel), strings.TrimSpace(sessionUpstreamModel))
}

func normalizeOpenAIPromptCacheFieldsRawWithSessionModel(body []byte, account *Account, upstreamModel, sessionUpstreamModel string) ([]byte, bool, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return body, false, err
	}
	changed := normalizeOpenAIPromptCacheFields(decoded, account, upstreamModel)
	if session, ok := decoded["session"].(map[string]any); ok {
		sessionModel := strings.TrimSpace(sessionUpstreamModel)
		if sessionModel == "" {
			sessionModel, _ = session["model"].(string)
		}
		if sessionModel == "" {
			sessionModel = upstreamModel
		}
		if normalizeOpenAIPromptCacheFields(session, account, sessionModel) {
			changed = true
		}
	}
	if !changed {
		return body, false, nil
	}
	normalized, err := json.Marshal(decoded)
	return normalized, true, err
}

func applyOpenAIPromptCacheFieldsForEgress(account *Account, targetURL string, body []byte) []byte {
	normalized, changed, err := normalizeOpenAIPromptCacheFieldsForEgress(body, account, targetURL)
	if err != nil || !changed {
		return body
	}
	return normalized
}
