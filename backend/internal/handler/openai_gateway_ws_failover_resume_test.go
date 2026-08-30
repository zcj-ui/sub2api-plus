package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSNextAttemptMessageUsesCurrentTurnPayload(t *testing.T) {
	firstMessage := []byte(`{"type":"response.create","input":"first"}`)
	currentTurn := []byte(`{"type":"response.create","input":"turn-281"}`)

	next, ok := openAIWSNextAttemptMessage(firstMessage, currentTurn, true)

	require.True(t, ok)
	require.Equal(t, currentTurn, next)
	next[0] = 'x'
	require.Equal(t, byte('{'), currentTurn[0], "retry payload must be cloned")
}

func TestOpenAIWSNextAttemptMessageRejectsMissingCurrentTurnPayload(t *testing.T) {
	next, ok := openAIWSNextAttemptMessage([]byte(`{"type":"response.create"}`), nil, true)

	require.False(t, ok)
	require.Nil(t, next)
}

func TestOpenAIWSNextAttemptMessageKeepsInitialMessageForFirstTurnFailover(t *testing.T) {
	firstMessage := []byte(`{"type":"response.create","input":"first"}`)

	next, ok := openAIWSNextAttemptMessage(firstMessage, nil, false)

	require.True(t, ok)
	require.Equal(t, firstMessage, next)
}

func TestOpenAIWSCurrentTurnRetryPayloadStateUsesReplayModel(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-second","input":[{"type":"message","role":"user","content":"retry"}]}`)

	next, model, coverage, ok := openAIWSCurrentTurnRetryPayloadState(payload)

	require.True(t, ok)
	require.Equal(t, "gpt-second", model)
	require.False(t, coverage.HasFunctionCallOutput)
	next[0] = 'x'
	require.Equal(t, byte('{'), payload[0], "retry payload must be cloned")
}

func TestOpenAIWSCurrentTurnRetryPayloadStateRejectsUnsafePayload(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"type":"response.create","input":"retry"}`),
		[]byte(`{"type":"response.create","model":123}`),
		[]byte(`{"type":"response.cancel","model":"gpt-second"}`),
		[]byte(`{"type":"response.create","model":"gpt-second","input":[{"type":"function_call_output","call_id":"missing","output":"done"}]}`),
		[]byte(`{"type":"response.create","model":"gpt-second","previous_response_id":123}`),
		[]byte(`{"type":"response.create","model":"gpt-second","previous_response_id":"resp_a","previous_response_id":"resp_b"}`),
		[]byte(`{`),
	}

	for _, payload := range tests {
		_, _, _, ok := openAIWSCurrentTurnRetryPayloadState(payload)
		require.False(t, ok, string(payload))
	}
}

func TestOpenAIWSSubscriptionTurnLeaseEnabledTracksCurrentRetryPlatform(t *testing.T) {
	production := &config.Config{}
	require.True(t, openAIWSSubscriptionTurnLeaseEnabled(true, service.PlatformOpenAI, production))
	require.False(t, openAIWSSubscriptionTurnLeaseEnabled(true, service.PlatformGrok, production))
	require.False(t, openAIWSSubscriptionTurnLeaseEnabled(false, service.PlatformOpenAI, production))

	simple := &config.Config{RunMode: config.RunModeSimple}
	require.False(t, openAIWSSubscriptionTurnLeaseEnabled(true, service.PlatformOpenAI, simple))
}
