package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestCodexTLSProfileContract(t *testing.T) {
	require.Equal(t, []uint16{
		0x1302, 0x1301, 0x1303,
		0xc02c, 0xc02b, 0xcca9,
		0xc030, 0xc02f, 0xcca8, 0x00ff,
	}, codexTLSProfile.CipherSuites)
	require.Equal(t, []uint16{0x11ec, 0x001d, 0x0017, 0x0018}, codexTLSProfile.Curves)
	require.Equal(t, codexTLSProfile.Curves, codexTLSProfile.KeyShareGroups)
	require.Equal(t, []uint16{0}, codexTLSProfile.PointFormats)
	require.Empty(t, codexTLSProfile.ALPNProtocols)
	require.True(t, codexTLSProfile.RandomizeExtensionOrder)
	require.False(t, codexTLSProfile.EnableGREASE)
	require.Equal(t, []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}, codexTLSProfile.Extensions)
}

func TestResolveOpenAICodexTLSProfile(t *testing.T) {
	oauth := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	explicit := &tlsfingerprint.Profile{Name: "operator-profile"}
	require.Same(t, explicit, resolveOpenAICodexTLSProfile(explicit, oauth))
	require.Same(t, codexTLSProfile, resolveOpenAICodexTLSProfile(nil, oauth))
	require.Nil(t, resolveOpenAICodexTLSProfile(nil, apiKey))
	require.Nil(t, resolveOpenAICodexTLSProfile(nil, nil))
}

func TestShuffleExtensionOrderPreservesProfile(t *testing.T) {
	original := []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}
	// Keep this contract test in the service package as a sentinel for the
	// profile's source slice.  The actual permutation mechanics are tested in
	// internal/pkg/tlsfingerprint, where the unexported builder is available.
	require.Equal(t, original, codexTLSProfile.Extensions)
}

type codexTLSUpstreamRecorder struct {
	doCalls        int
	doWithTLSCalls int
	lastProfile    *tlsfingerprint.Profile
}

func (r *codexTLSUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	r.doCalls++
	return r.response(), nil
}

func (r *codexTLSUpstreamRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	r.doWithTLSCalls++
	r.lastProfile = profile
	return r.response(), nil
}

func (r *codexTLSUpstreamRecorder) response() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(httptest.NewRequest(http.MethodGet, "http://example.test", nil).Body)}
}

func TestDoOpenAIUpstreamUsesCodexTLSProfileOnlyForOAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	for _, tc := range []struct {
		name        string
		account     *Account
		wantProfile *tlsfingerprint.Profile
	}{
		{name: "oauth", account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, wantProfile: codexTLSProfile},
		{name: "api-key", account: &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &codexTLSUpstreamRecorder{}
			svc := &OpenAIGatewayService{httpUpstream: recorder}
			resp, err := svc.doOpenAIUpstream(req.Clone(req.Context()), "", tc.account)
			require.NoError(t, err)
			require.NotNil(t, resp)
			expectedDoCalls := 0
			expectedDoWithTLSCalls := 1
			if tc.wantProfile == nil {
				expectedDoCalls = 1
				expectedDoWithTLSCalls = 0
			}
			require.Equal(t, expectedDoCalls, recorder.doCalls)
			require.Equal(t, expectedDoWithTLSCalls, recorder.doWithTLSCalls)
			require.Same(t, tc.wantProfile, recorder.lastProfile)
			_ = resp.Body.Close()
		})
	}
}

func TestDoOpenAIAccountTestUpstreamUsesSameCodexTLSProfile(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	for _, tc := range []struct {
		name           string
		account        *Account
		wantDo         int
		wantDoWithTLS  int
		wantProfile    *tlsfingerprint.Profile
		useTLSFallback bool
	}{
		{
			name:           "oauth without profile service uses built-in Codex profile",
			account:        &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			wantDoWithTLS:  1,
			wantProfile:    codexTLSProfile,
			useTLSFallback: true,
		},
		{
			name:           "api key keeps ordinary transport",
			account:        &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			wantDo:         1,
			useTLSFallback: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &codexTLSUpstreamRecorder{}
			svc := &AccountTestService{httpUpstream: recorder}
			resp, err := svc.doOpenAIAccountTestUpstream(req.Clone(req.Context()), "", tc.account, tc.useTLSFallback)
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, tc.wantDo, recorder.doCalls)
			require.Equal(t, tc.wantDoWithTLS, recorder.doWithTLSCalls)
			require.Same(t, tc.wantProfile, recorder.lastProfile)
			_ = resp.Body.Close()
		})
	}
}
