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

// zhipuTeamQuotaUpstream records the fully built request while returning a
// minimal successful payload.  Keeping this at the HTTPUpstream boundary also
// verifies that URL/header validation happens before any network adapter call.
type zhipuTeamQuotaUpstream struct {
	req   *http.Request
	proxy string
	calls int
}

func (u *zhipuTeamQuotaUpstream) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.req = req.Clone(req.Context())
	u.proxy = proxyURL
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"success":true,"data":{"level":"team","limits":[]}}`)),
	}, nil
}

func (u *zhipuTeamQuotaUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

type zhipuTeamQuotaRepo struct {
	AccountRepository
	account *Account
}

func (r *zhipuTeamQuotaRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *zhipuTeamQuotaRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

type zhipuTeamProxyRepo struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r *zhipuTeamProxyRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return r.proxy, r.err
}

func newZhipuCodingAccount(credentials map[string]any) *Account {
	return &Account{
		ID:          901,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: credentials,
	}
}

func TestZhipuTeamQuotaRequestIncludesContext(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-team",
		"zhipu_organization": "org-0E486bA654cF4ceBbA31873c4432D407",
		"zhipu_project":      "proj_D9637f2f1DE74e57980C70E47d1ea61d",
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit?type=2", upstream.req.URL.String())
	require.Equal(t, "sk-team", upstream.req.Header.Get("Authorization"))
	require.Equal(t, "org-0E486bA654cF4ceBbA31873c4432D407", upstream.req.Header.Get("bigmodel-organization"))
	require.Equal(t, "proj_D9637f2f1DE74e57980C70E47d1ea61d", upstream.req.Header.Get("bigmodel-project"))
	require.Equal(t, "application/json", upstream.req.Header.Get("Content-Type"))
	require.Equal(t, "en-US,en", upstream.req.Header.Get("Accept-Language"))
}

func TestZhipuQuotaFallsBackToPersonalWhenTeamOrganizationIsCleared(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-personal",
		"zhipu_organization": "",
		// A stale project value must not force a team request by itself.
		"zhipu_project": "proj_D9637f2f1DE74e57980C70E47d1ea61d",
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit", upstream.req.URL.String())
	require.Empty(t, upstream.req.Header.Get("bigmodel-organization"))
	require.Empty(t, upstream.req.Header.Get("bigmodel-project"))
}

func TestZhipuPersonalQuotaIgnoresStaleProjectOnlyValue(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":  AccountModeCoding,
		"api_key":       "sk-personal",
		"zhipu_project": "malformed-project-value",
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Zero(t, upstream.req.Header.Get("bigmodel-project"))
}

func TestZhipuPersonalQuotaDropsTeamHeadersFromOverrides(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":            AccountModeCoding,
		"api_key":                 "sk-personal",
		"header_override_enabled": true,
		"header_overrides": map[string]any{
			"bigmodel-organization": "org-injected",
			"bigmodel-project":      "proj-injected",
		},
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	_, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.Empty(t, upstream.req.Header.Get("bigmodel-organization"))
	require.Empty(t, upstream.req.Header.Get("bigmodel-project"))
}

func TestZhipuQuotaRejectsUnsafeTeamIDsBeforeRequest(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
	}{
		{name: "organization newline", field: "zhipu_organization", value: "org-good\r\nX-Injected: yes"},
		{name: "organization url", field: "zhipu_organization", value: "org-good/path"},
		{name: "project newline", field: "zhipu_project", value: "proj-good\nX-Injected: yes"},
		{name: "project wrong prefix", field: "zhipu_project", value: "org-good"},
		{name: "organization too long", field: "zhipu_organization", value: "org-" + strings.Repeat("a", zhipuTeamIDMaxLength)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &zhipuTeamQuotaUpstream{}
			credentials := map[string]any{
				"account_mode": AccountModeCoding,
				"api_key":      "sk-test",
			}
			// A project is validated only when it opts into team mode; keep
			// these malformed-project cases paired with an organization so
			// they exercise the validation boundary rather than the personal
			// route's intentional project-only ignore behavior.
			if tc.field == "zhipu_project" {
				credentials["zhipu_organization"] = "org-valid"
			}
			credentials[tc.field] = tc.value
			repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(credentials)}
			svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

			result, err := svc.QueryUsage(context.Background(), repo.account.ID)
			require.Error(t, err)
			require.Nil(t, result)
			require.Contains(t, err.Error(), "CN_QUOTA_INVALID_ZHIPU_TEAM_ID")
			require.Zero(t, upstream.calls)
		})
	}
}

func TestZhipuTeamHeadersOverrideCannotReplaceValidatedIDs(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":            AccountModeCoding,
		"api_key":                 "sk-team",
		"zhipu_organization":      "org-valid",
		"zhipu_project":           "proj-valid",
		"header_override_enabled": true,
		"header_overrides": map[string]any{
			"bigmodel-organization": "org-attacker",
			"bigmodel-project":      "proj-attacker",
		},
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	_, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.Equal(t, "org-valid", upstream.req.Header.Get("bigmodel-organization"))
	require.Equal(t, "proj-valid", upstream.req.Header.Get("bigmodel-project"))
}

func TestZhipuTeamQuotaKeepsConfiguredProxyPinned(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	proxyID := int64(73)
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-team",
		"zhipu_organization": "org-valid",
		"zhipu_project":      "proj-valid",
	})}
	repo.account.ProxyID = &proxyID
	proxyRepo := &zhipuTeamProxyRepo{proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080}}
	svc := NewCNProviderQuotaService(repo, proxyRepo, upstream, nil)

	_, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", upstream.proxy)
}

func TestZhipuTeamQuotaFailsClosedWhenConfiguredProxyIsUnavailable(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	proxyID := int64(74)
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-team",
		"zhipu_organization": "org-valid",
		"zhipu_project":      "proj-valid",
	})}
	repo.account.ProxyID = &proxyID
	svc := NewCNProviderQuotaService(repo, &zhipuTeamProxyRepo{}, upstream, nil)

	_, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CN_QUOTA_PROXY_UNAVAILABLE")
	require.Zero(t, upstream.calls)
}

func TestZhipuTeamQuotaAlwaysUsesDomesticControlPlane(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-team",
		"base_url":           "https://api.z.ai/api/coding/paas/v4",
		"zhipu_organization": "org-valid",
		"zhipu_project":      "proj-valid",
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	_, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.NoError(t, err)
	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit?type=2", upstream.req.URL.String())
}

func TestZhipuTeamQuotaRequiresProjectWhenOrganizationIsPresent(t *testing.T) {
	upstream := &zhipuTeamQuotaUpstream{}
	repo := &zhipuTeamQuotaRepo{account: newZhipuCodingAccount(map[string]any{
		"account_mode":       AccountModeCoding,
		"api_key":            "sk-team",
		"zhipu_organization": "org-valid",
	})}
	svc := NewCNProviderQuotaService(repo, nil, upstream, nil)

	result, err := svc.QueryUsage(context.Background(), repo.account.ID)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "project id is required")
	require.Zero(t, upstream.calls)
}

func TestZhipuQuotaParserAcceptsCreditLimitAndArrayEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "data object credit limits",
			body: `{"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"percentage":12.5},{"type":"CREDIT_LIMIT","unit":6,"percentage":22.5}]}}`,
			want: 2,
		},
		{
			name: "data array",
			body: `{"data":[{"type":"TOKENS_LIMIT","unit":3,"percentage":31}]}`,
			want: 1,
		},
		{
			name: "top-level array",
			body: `[{"type":"TOKENS_LIMIT","unit":6,"percentage":41}]`,
			want: 1,
		},
		{
			name: "nested data array",
			body: `{"data":{"data":[{"type":"CREDIT_LIMIT","unit":3,"percentage":51}]}}`,
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := zhipuQuotaData([]byte(tc.body))
			tiers := parseZhipuTokenTiers(data)
			require.Len(t, tiers, tc.want)
		})
	}
}
