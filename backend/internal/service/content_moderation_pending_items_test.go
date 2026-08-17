package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractContentModerationInput_PendingResponsesItemsScanEarlyItemAndToolFields(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"__sub2api_pending_conversation_items":true,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"early staged content"}]},
			{"type":"tool_use","input":{"query":"staged object input"}},
			{"type":"function_call","arguments":"{\"prompt\":\"staged tool secret\"}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"benign tail"}]}
		]
	}`)

	input := ExtractContentModerationInput(ContentModerationProtocolOpenAIResponses, body)

	require.Contains(t, input.Text, "early staged content")
	require.Contains(t, input.Text, "staged object input")
	require.Contains(t, input.Text, "staged tool secret")
	require.Contains(t, input.Text, "benign tail")
}

func TestExtractContentModerationInput_OrdinaryResponsesKeepsLatestUserSemantics(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"latest user"}]}
		]
	}`)

	input := ExtractContentModerationInput(ContentModerationProtocolOpenAIResponses, body)

	require.Equal(t, "latest user", input.Text)
}

func TestPendingResponsesAuditEnvelopeReachesLegacyExtractorThroughBeforeRequest(t *testing.T) {
	pending := &openAIWSPassthroughPendingAuditItems{}
	require.NoError(t, pending.addConversationItem([]byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"early malicious staged item"}]}}`)))

	var moderated ContentModerationInput
	hooks := &OpenAIWSIngressHooks{
		BeforeRequest: func(_ int, payload []byte, _ string) error {
			moderated = ExtractContentModerationInput(ContentModerationProtocolOpenAIResponses, payload)
			return nil
		},
	}
	require.NoError(t, admitOpenAIWSPassthroughResponseCreate(
		hooks,
		pending,
		2,
		[]byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"benign tail"}]}]}`),
		"gpt-5.6",
	))
	require.Contains(t, moderated.Text, "early malicious staged item")
	require.Contains(t, moderated.Text, "benign tail")
}
