package service

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIWSResponseCreatePayload_MapsOnlyProtocolMetadata(t *testing.T) {
	originalMetadata := map[string]any{
		"x-codex-installation-id":               "client-installation",
		"session_id":                            "client-session",
		"thread_id":                             "client-thread",
		openAIWSTurnStateHeader:                 "body-turn-state",
		responsesLiteWSMetadataKey:              "false",
		openAIWSStreamRequestStartMSMetadataKey: "1",
		"x-codex-turn-metadata":                 "client-turn-metadata",
	}
	payload := map[string]any{
		"type":            "response.create",
		"client_metadata": originalMetadata,
	}

	changed := normalizeOpenAIWSResponseCreatePayload(payload, openAIWSResponseCreateProtocolOptions{
		TurnState:     "header-turn-state",
		ResponsesLite: true,
	})
	require.True(t, changed)

	metadata, ok := payload["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "client-installation", metadata["x-codex-installation-id"])
	require.Equal(t, "client-session", metadata["session_id"])
	require.Equal(t, "client-thread", metadata["thread_id"])
	require.Equal(t, "client-turn-metadata", metadata["x-codex-turn-metadata"])
	require.Equal(t, "body-turn-state", metadata[openAIWSTurnStateHeader], "body state is authoritative")
	require.Equal(t, "true", metadata[responsesLiteWSMetadataKey])
	stamp, err := strconv.ParseInt(metadata[openAIWSStreamRequestStartMSMetadataKey].(string), 10, 64)
	require.NoError(t, err)
	require.Greater(t, stamp, int64(1))
	require.LessOrEqual(t, stamp, time.Now().UnixMilli())
	// The shallow request copy must not mutate the original caller map.
	require.Equal(t, "false", originalMetadata[responsesLiteWSMetadataKey])
	require.Equal(t, "1", originalMetadata[openAIWSStreamRequestStartMSMetadataKey])
}

func TestNormalizeOpenAIWSResponseCreatePayloadBytes_LeavesNonCreateFramesUntouched(t *testing.T) {
	nonCreate := []byte(`{"type":"conversation.item.create","client_metadata":{"session_id":"client-session"}}`)
	unchanged, changed, err := normalizeOpenAIWSResponseCreatePayloadBytes(
		nonCreate,
		openAIWSResponseCreateProtocolOptions{TurnState: "header-turn-state", ResponsesLite: true},
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, nonCreate, unchanged)

	responseCreate := []byte(`{"type":"response.create","client_metadata":{"session_id":"client-session"}}`)
	normalized, changed, err := normalizeOpenAIWSResponseCreatePayloadBytes(
		responseCreate,
		openAIWSResponseCreateProtocolOptions{TurnState: "header-turn-state", ResponsesLite: true},
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client-session", gjson.GetBytes(normalized, "client_metadata.session_id").String())
	require.Equal(t, "header-turn-state", gjson.GetBytes(normalized, "client_metadata."+openAIWSTurnStateHeader).String())
	require.Equal(t, "true", gjson.GetBytes(normalized, "client_metadata."+responsesLiteWSMetadataKey).String())
	stamp := gjson.GetBytes(normalized, "client_metadata."+openAIWSStreamRequestStartMSMetadataKey).String()
	_, err = strconv.ParseInt(stamp, 10, 64)
	require.NoError(t, err)
}

func TestOpenAIWSV2HandshakeMovesStateAndLiteIntoResponseCreate(t *testing.T) {
	c, account, body := newCompleteOfficialCodexIdentityContext(t)
	c.Request.Header.Set("OpenAI-Beta", "responses=experimental")
	c.Request.Header.Set(responsesLiteHeaderKey, "true")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	headers, _, err := svc.buildOpenAIWSHeadersWithBody(
		t.Context(), c, account, "oauth-token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "fallback-turn-state", "", "", "gpt-5.6-codex", "", body,
	)
	require.NoError(t, err)
	require.Equal(t, openAIWSBetaV2Value, headers.Get("OpenAI-Beta"))
	require.Empty(t, headers.Get(openAIWSTurnStateHeader))
	require.Empty(t, headers.Get(responsesLiteHeaderKey))
	require.Equal(t, codexCLIUserAgent, headers.Get("User-Agent"))
	require.Equal(t, openai.CodexDefaultOriginator, headers.Get("Originator"))
	require.Equal(t, codexCLIVersion, headers.Get("Version"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	payload["type"] = "response.create"
	normalizeOpenAIWSResponseCreatePayload(
		payload,
		openAIWSResponseCreateProtocolOptionsFromHeaders(c.Request.Header, c.GetHeader(openAIWSTurnStateHeader)),
	)
	metadata := payload["client_metadata"].(map[string]any)
	require.Equal(t, "turn-state-1", metadata[openAIWSTurnStateHeader])
	require.Equal(t, "true", metadata[responsesLiteWSMetadataKey])
}

func TestOpenAICodexInferenceCallIDIsAllowedOnAllOpenAIPaths(t *testing.T) {
	const header = "x-codex-inference-call-id"
	require.True(t, openaiAllowedHeaders[header])
	require.True(t, openaiPassthroughAllowedHeaders[header])
	require.True(t, openaiOfficialCodexIdentityHeaders[header])

	headers := make(http.Header)
	headers.Set(responsesLiteHeaderKey, "true")
	options := openAIWSResponseCreateProtocolOptionsFromHeaders(headers, "state")
	require.True(t, options.ResponsesLite)
	require.Equal(t, "state", options.TurnState)
}
