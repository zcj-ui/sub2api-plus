package service

import (
	"errors"
	"testing"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSPassthroughResponseCreateFrame_AcceptsTextAndBinary(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"type":"response.create","model":"gpt-5.6"}`)
	require.True(t, isOpenAIWSPassthroughJSONFrame(coderws.MessageText))
	require.True(t, isOpenAIWSPassthroughJSONFrame(coderws.MessageBinary))
	require.True(t, isOpenAIWSPassthroughResponseCreateFrame(coderws.MessageText, payload))
	require.True(t, isOpenAIWSPassthroughResponseCreateFrame(coderws.MessageBinary, payload))
	require.False(t, isOpenAIWSPassthroughResponseCreateFrame(coderws.MessageText, []byte(`{"type":"session.update"}`)))
	require.False(t, isOpenAIWSPassthroughResponseCreateFrame(coderws.MessageType(0), payload))
}

func TestOpenAIWSPassthroughInitialPayloadMarksAuditOnly(t *testing.T) {
	frames := []OpenAIWSPassthroughInitialFrame{{
		MessageType: coderws.MessageText,
		Payload:     []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":"staged"}}`),
	}}
	first := []byte(`{"type":"response.create","model":"gpt-5.6","input":"tail"}`)

	auditPayload, err := BuildOpenAIWSPassthroughInitialAuditPayload(first, frames)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(auditPayload, OpenAIPendingConversationItemsAuditMarker).Bool())
	require.Len(t, gjson.GetBytes(auditPayload, "input").Array(), 2)

	mergedPayload, err := MergeOpenAIWSPassthroughInitialPayload(first, frames)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(mergedPayload, OpenAIPendingConversationItemsAuditMarker).Bool())
	require.Len(t, gjson.GetBytes(mergedPayload, "input").Array(), 2)
}

func TestOpenAIWSPassthroughPendingItemsJoinNextResponseAudit(t *testing.T) {
	t.Parallel()

	itemPayload := []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":"staged"}}`)
	responsePayload := []byte(`{"type":"response.create","model":"gpt-5.6"}`)
	pending := &openAIWSPassthroughPendingAuditItems{}
	require.NoError(t, pending.addConversationItem(itemPayload))
	var audited []byte
	turnCalls := 0
	hooks := &OpenAIWSIngressHooks{
		BeforeRequest: func(_ int, got []byte, _ string) error {
			turnCalls++
			audited = append([]byte(nil), got...)
			return nil
		},
	}

	require.True(t, isOpenAIWSPassthroughConversationItemCreateFrame(coderws.MessageText, itemPayload))
	require.True(t, isOpenAIWSPassthroughConversationItemCreateFrame(coderws.MessageBinary, itemPayload))
	require.NoError(t, admitOpenAIWSPassthroughResponseCreate(hooks, pending, 2, responsePayload, "gpt-5.6"))
	require.Equal(t, 1, turnCalls, "staged auxiliary frame must not consume a turn hook")
	require.Empty(t, pending.items, "successful response admission clears the connection-local buffer")
	require.Contains(t, string(audited), "staged")
	require.Equal(t, "response.create", gjson.GetBytes(audited, "type").String())
}

func TestOpenAIWSPassthroughPendingItemsRemainOnAuditFailure(t *testing.T) {
	pending := &openAIWSPassthroughPendingAuditItems{}
	require.NoError(t, pending.addConversationItem([]byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":"staged"}}`)))
	wantErr := errors.New("blocked")
	err := admitOpenAIWSPassthroughResponseCreate(&OpenAIWSIngressHooks{
		BeforeRequest: func(_ int, _ []byte, _ string) error { return wantErr },
	}, pending, 2, []byte(`{"type":"response.create"}`), "gpt-5.6")
	require.ErrorIs(t, err, wantErr)
	require.Len(t, pending.items, 1, "failed audit keeps staged content until the connection closes")
}
