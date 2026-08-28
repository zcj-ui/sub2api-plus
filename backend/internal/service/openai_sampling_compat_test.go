//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestApplyOpenAIResponsesSamplingModelPolicyUsesFinalMappedModel(t *testing.T) {
	temp := 0.2
	topP := 0.8
	req := &apicompat.ResponsesRequest{}

	applyOpenAIResponsesSamplingModelPolicy(req, "gpt-5.6-sol", &temp, &topP)
	require.NotNil(t, req.Temperature)
	require.NotNil(t, req.TopP)
	require.InDelta(t, temp, *req.Temperature, 1e-9)
	require.InDelta(t, topP, *req.TopP, 1e-9)

	applyOpenAIResponsesSamplingModelPolicy(req, "gpt-5.4", &temp, &topP)
	require.Nil(t, req.Temperature)
	require.Nil(t, req.TopP)
}
