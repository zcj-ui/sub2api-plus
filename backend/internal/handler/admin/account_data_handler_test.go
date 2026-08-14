package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBuildProxyKeyIsSecretFreeAndDelimiterSafe(t *testing.T) {
	firstLegacy := buildLegacyProxyKey("http", "proxy.example", 8080, "a|b", "c")
	secondLegacy := buildLegacyProxyKey("http", "proxy.example", 8080, "a", "b|c")
	require.Equal(t, firstLegacy, secondLegacy, "fixture must reproduce the legacy delimiter collision")

	first := buildProxyKey("http", "proxy.example", 8080, "a|b", "c")
	second := buildProxyKey("http", "proxy.example", 8080, "a", "b|c")
	require.NotEqual(t, first, second)
	require.Contains(t, first, "sha256:")
	require.NotContains(t, first, "a|b")
	legacyErrorKey := proxyKeyForError(buildLegacyProxyKey("http", "proxy.example", 8080, "user", "super-secret"))
	require.Contains(t, legacyErrorKey, "legacy-sha256:")
	require.NotContains(t, legacyErrorKey, "super-secret")
}

type dataImportResponse struct {
	Code int              `json:"code"`
	Data DataImportResult `json:"data"`
}

type dataResponse struct {
	Code int         `json:"code"`
	Data dataPayload `json:"data"`
}

type dataPayload struct {
	Type           string        `json:"type"`
	Version        int           `json:"version"`
	Proxies        []dataProxy   `json:"proxies"`
	Accounts       []dataAccount `json:"accounts"`
	SkippedShadows int           `json:"skipped_shadows"`
}

type dataProxy struct {
	ProxyKey string `json:"proxy_key"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Status   string `json:"status"`
}

type dataAccount struct {
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Credentials map[string]any `json:"credentials"`
	Extra       map[string]any `json:"extra"`
	ProxyKey    *string        `json:"proxy_key"`
	Concurrency int            `json:"concurrency"`
	Priority    int            `json:"priority"`
}

func setupAccountDataRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()

	h := NewAccountHandler(
		adminSvc,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	router.GET("/api/v1/admin/accounts/data", h.ExportData)
	router.POST("/api/v1/admin/accounts/data", h.ImportData)
	return router, adminSvc
}

func TestExportDataIncludesSecrets(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	proxyID := int64(11)
	adminSvc.proxies = []service.Proxy{
		{
			ID:       proxyID,
			Name:     "proxy",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
		{
			ID:       12,
			Name:     "orphan",
			Protocol: "https",
			Host:     "10.0.0.1",
			Port:     443,
			Username: "o",
			Password: "p",
			Status:   service.StatusActive,
		},
	}
	adminSvc.accounts = []service.Account{
		{
			ID:          21,
			Name:        "account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			Extra:       map[string]any{"note": "x"},
			ProxyID:     &proxyID,
			Concurrency: 3,
			Priority:    50,
			Status:      service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Empty(t, resp.Data.Type)
	require.Equal(t, 0, resp.Data.Version)
	require.Len(t, resp.Data.Proxies, 1)
	require.Equal(t, "pass", resp.Data.Proxies[0].Password)
	require.Contains(t, resp.Data.Proxies[0].ProxyKey, "sha256:")
	require.NotContains(t, resp.Data.Proxies[0].ProxyKey, "pass")
	require.Len(t, resp.Data.Accounts, 1)
	require.Equal(t, "secret", resp.Data.Accounts[0].Credentials["token"])
}

func TestAccountExportIncludesCompleteBackupProxyChainAndRoundTrips(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	backupID := int64(12)
	tailID := int64(13)
	primaryID := int64(11)
	adminSvc.proxies = []service.Proxy{
		{ID: primaryID, Name: "primary", Protocol: "http", Host: "127.0.0.1", Port: 8080, Username: "u1", Password: "p1", Status: service.StatusActive, FallbackMode: service.FallbackModeProxy, BackupProxyID: &backupID},
		{ID: backupID, Name: "backup", Protocol: "http", Host: "127.0.0.2", Port: 8081, Username: "u2", Password: "p2", Status: service.StatusActive, FallbackMode: service.FallbackModeProxy, BackupProxyID: &tailID},
		{ID: tailID, Name: "tail", Protocol: "socks5", Host: "127.0.0.3", Port: 1080, Username: "u3", Password: "p3", Status: service.StatusActive, FallbackMode: service.FallbackModeNone},
		{ID: 99, Name: "orphan", Protocol: "http", Host: "127.0.0.99", Port: 8099, Status: service.StatusActive},
	}
	adminSvc.accounts = []service.Account{{ID: 21, Name: "account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"token": "secret"}, ProxyID: &primaryID}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var exported struct {
		Data DataPayload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &exported))
	require.Len(t, exported.Data.Proxies, 3)

	byName := make(map[string]DataProxy, len(exported.Data.Proxies))
	for _, proxy := range exported.Data.Proxies {
		byName[proxy.Name] = proxy
	}
	require.Equal(t, byName["backup"].ProxyKey, byName["primary"].BackupProxyKey)
	require.Equal(t, byName["tail"].ProxyKey, byName["backup"].BackupProxyKey)
	require.NotContains(t, byName, "orphan")

	importRouter, importSvc := setupAccountDataRouter()
	skipDefaultGroupBind := true
	body, err := json.Marshal(DataImportRequest{Data: exported.Data, SkipDefaultGroupBind: &skipDefaultGroupBind})
	require.NoError(t, err)
	importRec := httptest.NewRecorder()
	importReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	importReq.Header.Set("Content-Type", "application/json")
	importRouter.ServeHTTP(importRec, importReq)
	require.Equal(t, http.StatusOK, importRec.Code)
	require.Len(t, importSvc.createdProxies, 3)
	require.Len(t, importSvc.createdAccounts, 1)
	createdIDByName := make(map[string]int64, len(importSvc.createdProxies))
	for index, input := range importSvc.createdProxies {
		createdIDByName[input.Name] = int64(400 + index)
	}
	updateByName := make(map[string]*service.UpdateProxyInput, len(importSvc.updatedProxies))
	for _, update := range importSvc.updatedProxies {
		updateByName[update.Name] = update
	}
	primaryUpdate := updateByName["primary"]
	require.NotNil(t, primaryUpdate)
	require.NotNil(t, primaryUpdate.BackupProxyID)
	require.Equal(t, createdIDByName["backup"], *primaryUpdate.BackupProxyID)
	backupUpdate := updateByName["backup"]
	require.NotNil(t, backupUpdate)
	require.NotNil(t, backupUpdate.BackupProxyID)
	require.Equal(t, createdIDByName["tail"], *backupUpdate.BackupProxyID)
	require.NotNil(t, importSvc.createdAccounts[0].ProxyID)
	require.Equal(t, createdIDByName["primary"], *importSvc.createdAccounts[0].ProxyID)
}

func TestExportDataWithoutProxies(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	proxyID := int64(11)
	adminSvc.proxies = []service.Proxy{
		{
			ID:       proxyID,
			Name:     "proxy",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
	}
	adminSvc.accounts = []service.Account{
		{
			ID:          21,
			Name:        "account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			ProxyID:     &proxyID,
			Concurrency: 3,
			Priority:    50,
			Status:      service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Proxies, 0)
	require.Len(t, resp.Data.Accounts, 1)
	require.Nil(t, resp.Data.Accounts[0].ProxyKey)
}

// TestExportDataExcludesSparkShadow 验证外审第5轮 P1/P2:导出时排除 spark 影子账号
// (影子无凭据、导入侧强制 credentials 非空,混入会产出无法还原的坏备份),并透出跳过计数。
func TestExportDataExcludesSparkShadow(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	parentID := int64(21)
	adminSvc.accounts = []service.Account{
		{
			ID:          parentID,
			Name:        "mother",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			Status:      service.StatusActive,
		},
		{
			ID:              22,
			Name:            "mother (Spark)",
			Platform:        service.PlatformOpenAI,
			Type:            service.AccountTypeOAuth,
			Credentials:     map[string]any{}, // 影子恒空凭据
			ParentAccountID: &parentID,        // 影子标记
			QuotaDimension:  service.QuotaDimensionSpark,
			Status:          service.StatusActive,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Accounts, 1, "影子应被排除,仅导出母账号")
	require.Equal(t, "mother", resp.Data.Accounts[0].Name)
	require.Equal(t, 1, resp.Data.SkippedShadows, "跳过的影子数量应透出")
}

func TestExportDataPassesAccountFiltersAndSort(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	adminSvc.accounts = []service.Account{
		{ID: 1, Name: "acc-1", Status: service.StatusActive},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/accounts/data?platform=openai&type=oauth&status=active&group=12&privacy_mode=blocked&search=keyword&sort_by=priority&sort_order=desc",
		nil,
	)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Equal(t, 1, adminSvc.lastListAccounts.calls)
	require.Equal(t, "openai", adminSvc.lastListAccounts.platform)
	require.Equal(t, "oauth", adminSvc.lastListAccounts.accountType)
	require.Equal(t, "active", adminSvc.lastListAccounts.status)
	require.Equal(t, int64(12), adminSvc.lastListAccounts.groupID)
	require.Equal(t, "blocked", adminSvc.lastListAccounts.privacyMode)
	require.Equal(t, "keyword", adminSvc.lastListAccounts.search)
	require.Equal(t, "priority", adminSvc.lastListAccounts.sortBy)
	require.Equal(t, "desc", adminSvc.lastListAccounts.sortOrder)
}

func TestExportDataSelectedIDsOverrideFilters(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/accounts/data?ids=1,2&platform=openai&search=keyword&sort_by=priority&sort_order=desc",
		nil,
	)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Accounts, 2)
	require.Equal(t, 0, adminSvc.lastListAccounts.calls)
}

func TestImportDataReusesProxyAndSkipsDefaultGroup(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy",
			Protocol: "socks5",
			Host:     "1.2.3.4",
			Port:     1080,
			Username: "u",
			Password: "p",
			Status:   service.StatusActive,
		},
	}

	dataPayload := map[string]any{
		"data": map[string]any{
			"type":    dataType,
			"version": dataVersion,
			"proxies": []map[string]any{
				{
					"proxy_key": "socks5|1.2.3.4|1080|u|p",
					"name":      "proxy",
					"protocol":  "socks5",
					"host":      "1.2.3.4",
					"port":      1080,
					"username":  "u",
					"password":  "p",
					"status":    "active",
				},
			},
			"accounts": []map[string]any{
				{
					"name":        "acc",
					"platform":    service.PlatformOpenAI,
					"type":        service.AccountTypeOAuth,
					"credentials": map[string]any{"token": "x"},
					"proxy_key":   "socks5|1.2.3.4|1080|u|p",
					"concurrency": 3,
					"priority":    50,
				},
			},
		},
		"skip_default_group_bind": true,
	}

	body, _ := json.Marshal(dataPayload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, adminSvc.createdProxies, 0)
	require.Len(t, adminSvc.createdAccounts, 1)
	require.True(t, adminSvc.createdAccounts[0].SkipDefaultGroupBind)
}

func TestImportDataRejectsNonCanonicalProxyKey(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	payload := map[string]any{"data": map[string]any{
		"type": dataType, "version": dataVersion,
		"proxies": []map[string]any{{
			"proxy_key": "forged-key", "name": "proxy", "protocol": "http",
			"host": "127.0.0.1", "port": 8080, "status": "active",
		}},
		"accounts": []any{},
	}}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response dataImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, 1, response.Data.ProxyFailed)
	require.Empty(t, adminSvc.createdProxies)
}

func TestImportDataRejectsInvalidProxyFallbackMode(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	payload := map[string]any{"data": map[string]any{
		"type": dataType, "version": dataVersion,
		"proxies": []map[string]any{{
			"name": "proxy", "protocol": "http", "host": "127.0.0.1", "port": 8080,
			"status": "active", "fallback_mode": "random",
		}},
		"accounts": []any{},
	}}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response dataImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, 1, response.Data.ProxyFailed)
	require.Empty(t, adminSvc.createdProxies)
	require.Contains(t, response.Data.Errors[0].Message, "fallback_mode")
}

func TestImportDataResolvesForwardBackupProxyByCanonicalKey(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	backupKey := buildProxyKey("http", "127.0.0.2", 8081, "", "")
	payload := map[string]any{"data": map[string]any{
		"type": dataType, "version": dataVersion,
		"proxies": []map[string]any{
			{
				"proxy_key": buildProxyKey("http", "127.0.0.1", 8080, "", ""),
				"name":      "primary", "protocol": "http", "host": "127.0.0.1", "port": 8080,
				"status": "active", "fallback_mode": service.FallbackModeProxy, "backup_proxy_key": backupKey,
			},
			{
				"proxy_key": backupKey, "name": "backup", "protocol": "http",
				"host": "127.0.0.2", "port": 8081, "status": "active",
			},
		},
		"accounts": []any{},
	}}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Lenf(t, adminSvc.createdProxies, 1, "response: %s", rec.Body.String())
	var primaryUpdate *service.UpdateProxyInput
	for index, id := range adminSvc.updatedProxyIDs {
		if id == 4 {
			primaryUpdate = adminSvc.updatedProxies[index]
		}
	}
	require.NotNil(t, primaryUpdate)
	require.NotNil(t, primaryUpdate.BackupProxyID)
	require.Equal(t, int64(400), *primaryUpdate.BackupProxyID)
}

func TestImportDataReportsProxyUpdateFailure(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	adminSvc.updateProxyErr = errors.New("update failed")
	payload := map[string]any{"data": map[string]any{
		"type": dataType, "version": dataVersion,
		"proxies": []map[string]any{{
			"proxy_key": buildProxyKey("http", "127.0.0.1", 8080, "", ""),
			"name":      "proxy", "protocol": "http", "host": "127.0.0.1", "port": 8080, "status": "active",
		}},
		"accounts": []any{},
	}}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response dataImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, 1, response.Data.ProxyFailed)
	require.Contains(t, response.Data.Errors[0].Message, "update failed")
}

func TestImportDataAppliesCodex429GuardOnlyToOpenAIOAuth(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	payload := map[string]any{
		"data": map[string]any{
			"type":    dataType,
			"version": dataVersion,
			"proxies": []any{},
			"accounts": []map[string]any{
				{"name": "codex", "platform": service.PlatformOpenAI, "type": service.AccountTypeOAuth, "credentials": map[string]any{"token": "x"}, "concurrency": 1, "priority": 1},
				{"name": "openai-key", "platform": service.PlatformOpenAI, "type": service.AccountTypeAPIKey, "credentials": map[string]any{"api_key": "x"}, "extra": map[string]any{"keep": true}, "concurrency": 1, "priority": 1},
				{"name": "claude", "platform": service.PlatformAnthropic, "type": service.AccountTypeOAuth, "credentials": map[string]any{"token": "x"}, "extra": map[string]any{"keep": true}, "concurrency": 1, "priority": 1},
			},
		},
		"skip_default_group_bind": true,
		"codex_429_guard_enabled": false,
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, adminSvc.createdAccounts, 3)
	require.Equal(t, false, adminSvc.createdAccounts[0].Extra[service.OpenAICodex429GuardEnabledExtraKey])
	require.NotContains(t, adminSvc.createdAccounts[1].Extra, service.OpenAICodex429GuardEnabledExtraKey)
	require.NotContains(t, adminSvc.createdAccounts[2].Extra, service.OpenAICodex429GuardEnabledExtraKey)
}
