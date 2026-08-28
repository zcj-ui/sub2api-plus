package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"

// applyOpenAIResponsesSamplingModelPolicy reapplies the client sampling
// values after a compatibility bridge has resolved the final upstream model.
// The bridge may have parsed/filtered using the inbound alias (for example
// gpt-5.4 mapped to gpt-5.6-sol), so capability decisions must be made against
// the model that will actually be sent. Older GPT-5 reasoning models reject
// both fields; GPT-5.6 and non-GPT-5 models retain them.
func applyOpenAIResponsesSamplingModelPolicy(
	req *apicompat.ResponsesRequest,
	upstreamModel string,
	temperature, topP *float64,
) {
	if req == nil {
		return
	}
	if isOpenAICodexSamplingUnsupportedModel(upstreamModel) {
		req.Temperature = nil
		req.TopP = nil
		return
	}
	req.Temperature = temperature
	req.TopP = topP
}
