//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type cnAdaptiveProxyRecorder struct {
	calls    int
	proxyURL string
}

func (r *cnAdaptiveProxyRecorder) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, context.Canceled
}

func (r *cnAdaptiveProxyRecorder) DoWithTLS(_ *http.Request, proxyURL string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	r.calls++
	r.proxyURL = proxyURL
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func TestAccountTestCNAdaptiveRequestFailsClosedWhenConfiguredProxyRelationMissing(t *testing.T) {
	proxyID := int64(901)
	recorder := &cnAdaptiveProxyRecorder{}
	svc := &AccountTestService{httpUpstream: recorder}
	account := &Account{ID: 77, Platform: PlatformKimi, ProxyID: &proxyID}
	req, err := http.NewRequest(http.MethodGet, "https://provider.example.test/v1/responses", nil)
	require.NoError(t, err)

	resp, err := svc.doCNProviderAdaptiveRequest(req, account)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Zero(t, recorder.calls, "a configured but unavailable proxy must not fall back to direct egress")
}

func TestAccountTestCNAdaptiveRequestUsesConfiguredProxy(t *testing.T) {
	proxyID := int64(902)
	recorder := &cnAdaptiveProxyRecorder{}
	svc := &AccountTestService{httpUpstream: recorder}
	account := &Account{
		ID:       78,
		Platform: PlatformKimi,
		ProxyID:  &proxyID,
		Proxy:    &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example.test", Port: 8080},
	}
	req, err := http.NewRequest(http.MethodGet, "https://provider.example.test/v1/responses", nil)
	require.NoError(t, err)

	resp, err := svc.doCNProviderAdaptiveRequest(req, account)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "http://proxy.example.test:8080", recorder.proxyURL)
	require.Equal(t, 1, recorder.calls)
	_ = resp.Body.Close()
}
