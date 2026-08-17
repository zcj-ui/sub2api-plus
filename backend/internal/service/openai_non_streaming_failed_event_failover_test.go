package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const nonStreamingCapacityFailedSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}

event: response.failed
data: {"type":"response.failed","error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}

`

func newNonStreamingFailoverTestService() *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
}

func newNonStreamingFailoverTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func newCapacityFailedSSEResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"rid-nonstream-capacity"},
		},
		Body: io.NopCloser(strings.NewReader(nonStreamingCapacityFailedSSE)),
	}
}

func TestNonStreamingSSEToJSONCapacityFailedReturnsFailover(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	c, rec := newNonStreamingFailoverTestContext()
	payload := []byte(`{"type":"response.failed","error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}`)
	require.True(t, isOpenAITransientProcessingError(http.StatusBadRequest, extractOpenAISSEErrorMessage(payload), payload))
	require.True(t, openAIStreamFailedEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
	_, err := svc.handleNonStreamingResponse(context.Background(), newCapacityFailedSSEResponse(), c, &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "capacity errors must fail over: %v", err)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.RequestScopedTransient)
	require.Empty(t, rec.Body.String())
}

func TestNonStreamingPassthroughSSEToJSONCapacityFailedReturnsFailover(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	c, rec := newNonStreamingFailoverTestContext()
	_, err := svc.handleNonStreamingResponsePassthrough(context.Background(), newCapacityFailedSSEResponse(), c, &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "passthrough capacity errors must fail over: %v", err)
	require.Empty(t, rec.Body.String())
}

func TestNonStreamingSSEToJSONInvalidRequestFailedStillWritesError(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	c, rec := newNonStreamingFailoverTestContext()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			`data: {"type":"response.failed","error":{"message":"Invalid value for 'temperature'","type":"invalid_request_error"}}`,
			"",
		}, "\n"))),
	}
	_, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream_error")
}

func TestNonStreamingSSEToJSONContextWindowFailedStillWritesError(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	c, rec := newNonStreamingFailoverTestContext()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			`data: {"type":"response.failed","error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error"}}`,
			"",
		}, "\n"))),
	}
	_, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestNonStreamingSSEToJSONSkipsFailoverAfterResponseCommitted(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	c, rec := newNonStreamingFailoverTestContext()
	MarkResponseCommitted(c)
	_, err := svc.handleNonStreamingResponse(context.Background(), newCapacityFailedSSEResponse(), c, &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestNonStreamingFailedEventFailoverGuards(t *testing.T) {
	svc := newNonStreamingFailoverTestService()
	payload := []byte(`{"type":"response.failed","error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}`)
	msg := "Selected model is at capacity. Please try a different model."
	resp := newCapacityFailedSSEResponse()

	t.Run("nil_account", func(t *testing.T) {
		c, _ := newNonStreamingFailoverTestContext()
		require.Nil(t, svc.nonStreamingFailedEventFailover(c, nil, false, resp, payload, msg))
	})
	t.Run("nil_response", func(t *testing.T) {
		c, _ := newNonStreamingFailoverTestContext()
		require.Nil(t, svc.nonStreamingFailedEventFailover(c, &Account{ID: 1}, false, nil, payload, msg))
	})
	t.Run("nil_context", func(t *testing.T) {
		require.Nil(t, svc.nonStreamingFailedEventFailover(nil, &Account{ID: 1}, false, resp, payload, msg))
	})
	t.Run("response_committed", func(t *testing.T) {
		c, _ := newNonStreamingFailoverTestContext()
		MarkResponseCommitted(c)
		require.Nil(t, svc.nonStreamingFailedEventFailover(c, &Account{ID: 1}, false, resp, payload, msg))
	})
	t.Run("clean_context_failovers", func(t *testing.T) {
		c, _ := newNonStreamingFailoverTestContext()
		require.NotNil(t, svc.nonStreamingFailedEventFailover(c, &Account{ID: 1}, false, resp, payload, msg))
	})
}
