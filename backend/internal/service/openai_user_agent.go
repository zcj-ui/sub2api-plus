package service

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// OpenAIAccountUserAgentMaxBytes bounds the optional per-account Codex
// User-Agent override.  Real Codex identities are short; keeping this value
// bounded prevents accidental header amplification while leaving room for
// custom terminal/OS labels.
const OpenAIAccountUserAgentMaxBytes = 512

// NormalizeOpenAIUserAgentCredentials validates and normalizes the optional
// account-level User-Agent override.  It is deliberately scoped to OpenAI
// OAuth accounts: Claude/CC and other OpenAI credential types keep their
// existing credential semantics.  A missing key means "leave as-is" for
// update callers; an empty/whitespace value explicitly clears the override.
func NormalizeOpenAIUserAgentCredentials(platform, accountType string, credentials map[string]any) error {
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth || credentials == nil {
		return nil
	}
	raw, exists := credentials["user_agent"]
	if !exists {
		return nil
	}
	if raw == nil {
		delete(credentials, "user_agent")
		return nil
	}
	value, ok := raw.(string)
	if !ok {
		return infraerrors.BadRequest("INVALID_OPENAI_USER_AGENT", "user_agent must be a string")
	}
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		delete(credentials, "user_agent")
		return nil
	}
	if !utf8.ValidString(normalized) {
		return infraerrors.BadRequest("INVALID_OPENAI_USER_AGENT", "user_agent must be valid UTF-8")
	}
	if len(normalized) > OpenAIAccountUserAgentMaxBytes {
		return infraerrors.BadRequest("INVALID_OPENAI_USER_AGENT", fmt.Sprintf("user_agent must be at most %d bytes", OpenAIAccountUserAgentMaxBytes))
	}
	for _, r := range normalized {
		if unicode.IsControl(r) {
			return infraerrors.BadRequest("INVALID_OPENAI_USER_AGENT", "user_agent must not contain control characters")
		}
	}
	credentials["user_agent"] = normalized
	return nil
}
