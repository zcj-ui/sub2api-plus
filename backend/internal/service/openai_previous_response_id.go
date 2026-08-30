package service

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const (
	OpenAIPreviousResponseIDKindEmpty      = "empty"
	OpenAIPreviousResponseIDKindResponseID = "response_id"
	OpenAIPreviousResponseIDKindMessageID  = "message_id"
	OpenAIPreviousResponseIDKindUnknown    = "unknown"
	OpenAIPreviousResponseIDMaxBytes       = 512
)

var (
	openAIResponseIDPattern = regexp.MustCompile(`^resp_[A-Za-z0-9_-]{1,256}$`)
	openAIMessageIDPattern  = regexp.MustCompile(`^(msg|message|item|chatcmpl)_[A-Za-z0-9_-]{1,256}$`)
)

// ClassifyOpenAIPreviousResponseIDKind classifies previous_response_id to improve diagnostics.
func ClassifyOpenAIPreviousResponseIDKind(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return OpenAIPreviousResponseIDKindEmpty
	}
	if openAIResponseIDPattern.MatchString(trimmed) {
		return OpenAIPreviousResponseIDKindResponseID
	}
	if openAIMessageIDPattern.MatchString(strings.ToLower(trimmed)) {
		return OpenAIPreviousResponseIDKindMessageID
	}
	return OpenAIPreviousResponseIDKindUnknown
}

func IsOpenAIPreviousResponseIDLikelyMessageID(id string) bool {
	return ClassifyOpenAIPreviousResponseIDKind(id) == OpenAIPreviousResponseIDKindMessageID
}

// ParseOpenAIPreviousResponseIDField enforces the Responses wire contract at
// ingress: the field is omitted/null or a bounded string.  Rejecting numbers,
// objects, control characters, and oversized values prevents accidental
// scheduler/Redis key coercion and keeps HTTP and WebSocket entry points in
// agreement.
func ParseOpenAIPreviousResponseIDField(body []byte) (string, error) {
	value := gjson.GetBytes(body, "previous_response_id")
	if root := gjson.ParseBytes(body); root.IsObject() {
		occurrences := 0
		root.ForEach(func(key, _ gjson.Result) bool {
			if key.String() == "previous_response_id" {
				occurrences++
			}
			return true
		})
		if occurrences > 1 {
			return "", fmt.Errorf("previous_response_id must appear only once")
		}
	}
	if !value.Exists() || value.Type == gjson.Null {
		return "", nil
	}
	if value.Type != gjson.String {
		return "", fmt.Errorf("previous_response_id must be a string or null")
	}
	rawID := value.String()
	if !utf8.ValidString(rawID) {
		return "", fmt.Errorf("previous_response_id must be valid UTF-8")
	}
	// Validate the raw JSON string before trimming.  A trailing newline or other
	// control byte would otherwise disappear under TrimSpace and silently turn
	// into a different response id.
	if len(rawID) > OpenAIPreviousResponseIDMaxBytes {
		return "", fmt.Errorf("previous_response_id must be at most %d bytes", OpenAIPreviousResponseIDMaxBytes)
	}
	for _, r := range rawID {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("previous_response_id must not contain control characters")
		}
	}
	id := strings.TrimSpace(rawID)
	if id == "" {
		return "", nil
	}
	return id, nil
}
