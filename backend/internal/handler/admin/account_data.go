package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"log/slog"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	dataType       = "sub2api-data"
	legacyDataType = "sub2api-bundle"
	dataVersion    = 1
	dataPageCap    = 1000
)

type DataPayload struct {
	Type       string        `json:"type,omitempty"`
	Version    int           `json:"version,omitempty"`
	ExportedAt string        `json:"exported_at"`
	Proxies    []DataProxy   `json:"proxies"`
	Accounts   []DataAccount `json:"accounts"`
	// SkippedShadows 记录导出时被排除的 spark 影子账号数量(见 ExportData)。仅作可见性提示,
	// 导入侧忽略该字段;omitempty 保持向后兼容。
	SkippedShadows int `json:"skipped_shadows,omitempty"`
}

type DataProxy struct {
	ProxyKey        string `json:"proxy_key"`
	Name            string `json:"name"`
	Protocol        string `json:"protocol"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	Status          string `json:"status"`
	ExpiresAt       *int64 `json:"expires_at,omitempty"`        // unix 秒，与 DataAccount.ExpiresAt 风格一致
	FallbackMode    string `json:"fallback_mode,omitempty"`     // none/direct/proxy
	BackupProxyName string `json:"backup_proxy_name,omitempty"` // 旧备份按 name 反查
	BackupProxyKey  string `json:"backup_proxy_key,omitempty"`  // 规范端点键，优先于名称
	ExpiryWarnDays  int    `json:"expiry_warn_days,omitempty"`
}

// DataAccount 是管理员显式备份导出使用的账号结构，故意不走 dto.Account 的脱敏路径，
// Credentials 原文返回。这是"管理员备份"这一显式行为的一部分；如未来需要导出脱敏版本，
// 应新增独立结构而非修改这里。
// 注意:本结构不含 parent_account_id/quota_dimension——spark 影子账号在 ExportData 处被显式
// 排除(影子不持凭据、通用凭据型导入强制 credentials 非空无法重建父子链接),不在此表达。
// 影子的独立调度配置(priority/并发/分组/status 管理员可单独调)亦不在本备份范围,属已知局限
// (外审第6轮裁决:保持排除 + 前端警告,而非升级格式做完整往返)。
type DataAccount struct {
	Name               string         `json:"name"`
	Notes              *string        `json:"notes,omitempty"`
	Platform           string         `json:"platform"`
	Type               string         `json:"type"`
	Credentials        map[string]any `json:"credentials"`
	Extra              map[string]any `json:"extra,omitempty"`
	ProxyKey           *string        `json:"proxy_key,omitempty"`
	Concurrency        int            `json:"concurrency"`
	Priority           int            `json:"priority"`
	RateMultiplier     *float64       `json:"rate_multiplier,omitempty"`
	ExpiresAt          *int64         `json:"expires_at,omitempty"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired,omitempty"`
}

type DataImportRequest struct {
	Data                 DataPayload `json:"data"`
	SkipDefaultGroupBind *bool       `json:"skip_default_group_bind"`
	Codex429GuardEnabled *bool       `json:"codex_429_guard_enabled"`
	ConfirmOveragesRisk  *bool       `json:"confirm_overages_risk"`
}

type DataImportResult struct {
	ProxyCreated   int               `json:"proxy_created"`
	ProxyReused    int               `json:"proxy_reused"`
	ProxyFailed    int               `json:"proxy_failed"`
	AccountCreated int               `json:"account_created"`
	AccountFailed  int               `json:"account_failed"`
	Errors         []DataImportError `json:"errors,omitempty"`
}

type DataImportError struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	ProxyKey string `json:"proxy_key,omitempty"`
	Message  string `json:"message"`
}

func buildProxyKey(protocol, host string, port int, username, password string) string {
	// Length-prefix every field before hashing so delimiters in credentials can
	// never alias another endpoint. The digest also keeps proxy passwords out of
	// exported keys and import error responses.
	fields := []string{strings.TrimSpace(protocol), strings.TrimSpace(host), strconv.Itoa(port), strings.TrimSpace(username), strings.TrimSpace(password)}
	var canonical strings.Builder
	for _, field := range fields {
		_, _ = canonical.WriteString(strconv.Itoa(len(field)))
		_ = canonical.WriteByte(':')
		_, _ = canonical.WriteString(field)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// buildLegacyProxyKey is retained solely for importing backups written before
// proxy_key became a non-secret digest.
func buildLegacyProxyKey(protocol, host string, port int, username, password string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", strings.TrimSpace(protocol), strings.TrimSpace(host), port, strings.TrimSpace(username), strings.TrimSpace(password))
}

func proxyKeyMatches(item DataProxy, canonicalKey string) bool {
	if item.ProxyKey == "" {
		return true
	}
	return item.ProxyKey == canonicalKey || item.ProxyKey == buildLegacyProxyKey(item.Protocol, item.Host, item.Port, item.Username, item.Password)
}

func proxyKeyForError(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "sha256:") {
		return trimmed
	}
	sum := sha256.Sum256([]byte(trimmed))
	return "legacy-sha256:" + hex.EncodeToString(sum[:])
}

func addProxyKeyAliases(keys map[string]int64, p service.Proxy) {
	keys[buildProxyKey(p.Protocol, p.Host, p.Port, p.Username, p.Password)] = p.ID
	keys[buildLegacyProxyKey(p.Protocol, p.Host, p.Port, p.Username, p.Password)] = p.ID
}

func (h *AccountHandler) ExportData(c *gin.Context) {
	ctx := c.Request.Context()

	selectedIDs, err := parseAccountIDs(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	accounts, err := h.resolveExportAccounts(ctx, selectedIDs, c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 排除 spark 影子账号:影子不持凭据,通用凭据型导出无法表达父子链接、导入侧又强制 credentials
	// 非空——若混入会产出无法还原的坏备份(导入即失败)。影子的独立调度配置(priority/并发/分组/
	// status,管理员可单独调)随之不进备份,还原后需在重建的影子上重新调优;前端按 skipped_shadows
	// 提示用户(外审第5轮发现、第6轮裁决:保持排除 + 警告,不做完整往返)。
	skippedShadows := 0
	exportable := make([]service.Account, 0, len(accounts))
	for i := range accounts {
		if accounts[i].IsCredentialShadow() {
			skippedShadows++
			continue
		}
		exportable = append(exportable, accounts[i])
	}
	accounts = exportable
	if skippedShadows > 0 {
		slog.Info("export_skipped_spark_shadows", "count", skippedShadows)
	}

	includeProxies, err := parseIncludeProxies(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var proxies []service.Proxy
	if includeProxies {
		proxies, err = h.resolveExportProxies(ctx, accounts)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else {
		proxies = []service.Proxy{}
	}

	// Build all lookups first so a backup proxy can appear later in the export.
	proxyNameByID := make(map[int64]string, len(proxies))
	proxyKeyByID := make(map[int64]string, len(proxies))
	for i := range proxies {
		p := proxies[i]
		proxyNameByID[p.ID] = p.Name
		proxyKeyByID[p.ID] = buildProxyKey(p.Protocol, p.Host, p.Port, p.Username, p.Password)
	}

	dataProxies := make([]DataProxy, 0, len(proxies))
	for i := range proxies {
		p := proxies[i]
		key := proxyKeyByID[p.ID]

		var expiresAt *int64
		if p.ExpiresAt != nil {
			v := p.ExpiresAt.Unix()
			expiresAt = &v
		}
		var backupProxyName string
		var backupProxyKey string
		if p.BackupProxyID != nil {
			backupProxyName = proxyNameByID[*p.BackupProxyID]
			backupProxyKey = proxyKeyByID[*p.BackupProxyID]
		}
		dataProxies = append(dataProxies, DataProxy{
			ProxyKey:        key,
			Name:            p.Name,
			Protocol:        p.Protocol,
			Host:            p.Host,
			Port:            p.Port,
			Username:        p.Username,
			Password:        p.Password,
			Status:          p.Status,
			ExpiresAt:       expiresAt,
			FallbackMode:    p.FallbackMode,
			BackupProxyName: backupProxyName,
			BackupProxyKey:  backupProxyKey,
			ExpiryWarnDays:  p.ExpiryWarnDays,
		})
	}

	dataAccounts := make([]DataAccount, 0, len(accounts))
	for i := range accounts {
		acc := accounts[i]
		var proxyKey *string
		if acc.ProxyID != nil {
			if key, ok := proxyKeyByID[*acc.ProxyID]; ok {
				proxyKey = &key
			}
		}
		var expiresAt *int64
		if acc.ExpiresAt != nil {
			v := acc.ExpiresAt.Unix()
			expiresAt = &v
		}
		dataAccounts = append(dataAccounts, DataAccount{
			Name:               acc.Name,
			Notes:              acc.Notes,
			Platform:           acc.Platform,
			Type:               acc.Type,
			Credentials:        acc.Credentials,
			Extra:              acc.Extra,
			ProxyKey:           proxyKey,
			Concurrency:        acc.Concurrency,
			Priority:           acc.Priority,
			RateMultiplier:     acc.RateMultiplier,
			ExpiresAt:          expiresAt,
			AutoPauseOnExpired: &acc.AutoPauseOnExpired,
		})
	}

	payload := DataPayload{
		ExportedAt:     time.Now().UTC().Format(time.RFC3339),
		Proxies:        dataProxies,
		Accounts:       dataAccounts,
		SkippedShadows: skippedShadows,
	}

	response.Success(c, payload)
}

func (h *AccountHandler) ImportData(c *gin.Context) {
	var req DataImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if err := validateDataHeader(req.Data); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	executeAdminIdempotentJSON(c, "admin.accounts.import_data", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.importData(ctx, req)
	})
}

func (h *AccountHandler) importData(ctx context.Context, req DataImportRequest) (DataImportResult, error) {
	skipDefaultGroupBind := true
	if req.SkipDefaultGroupBind != nil {
		skipDefaultGroupBind = *req.SkipDefaultGroupBind
	}

	dataPayload := req.Data
	result := DataImportResult{}

	existingProxies, err := h.listAllProxies(ctx)
	if err != nil {
		return result, err
	}

	proxyKeyToID := make(map[string]int64, len(existingProxies))
	proxyByID := make(map[int64]*service.Proxy, len(existingProxies)+len(dataPayload.Proxies))
	// proxyNameToID 用于 backup_proxy_name 反查：DB 已有 + 本批次新建均会写入
	proxyNameToID := make(map[string]int64, len(existingProxies))
	for i := range existingProxies {
		p := existingProxies[i]
		addProxyKeyAliases(proxyKeyToID, p)
		proxyCopy := p
		proxyByID[p.ID] = &proxyCopy
		if p.Name != "" {
			proxyNameToID[p.Name] = p.ID
		}
	}
	type pendingProxyConfig struct {
		ID   int64
		Item DataProxy
		Key  string
	}
	pendingProxyConfigs := make([]pendingProxyConfig, 0, len(dataPayload.Proxies))

	for i := range dataPayload.Proxies {
		item := dataPayload.Proxies[i]
		key := buildProxyKey(item.Protocol, item.Host, item.Port, item.Username, item.Password)
		if err := validateDataProxy(item); err != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:     "proxy",
				Name:     item.Name,
				ProxyKey: key,
				Message:  err.Error(),
			})
			continue
		}
		if !proxyKeyMatches(item, key) {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:     "proxy",
				Name:     item.Name,
				ProxyKey: proxyKeyForError(item.ProxyKey),
				Message:  "proxy_key does not match the proxy endpoint",
			})
			continue
		}
		if existingID, ok := proxyKeyToID[key]; ok {
			result.ProxyReused++
			pendingProxyConfigs = append(pendingProxyConfigs, pendingProxyConfig{ID: existingID, Item: item, Key: key})
			continue
		}

		// 解析 expires_at（unix 秒 → *time.Time）
		var expiresAt *time.Time
		if item.ExpiresAt != nil {
			t := time.Unix(*item.ExpiresAt, 0).UTC()
			expiresAt = &t
		}

		created, createErr := h.adminService.CreateProxy(ctx, &service.CreateProxyInput{
			Name:           defaultProxyName(item.Name),
			Protocol:       item.Protocol,
			Host:           item.Host,
			Port:           item.Port,
			Username:       item.Username,
			Password:       item.Password,
			ExpiresAt:      expiresAt,
			FallbackMode:   service.FallbackModeNone,
			BackupProxyID:  nil,
			ExpiryWarnDays: item.ExpiryWarnDays,
		})
		if createErr != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:     "proxy",
				Name:     item.Name,
				ProxyKey: key,
				Message:  createErr.Error(),
			})
			continue
		}
		proxyKeyToID[key] = created.ID
		proxyKeyToID[buildLegacyProxyKey(item.Protocol, item.Host, item.Port, item.Username, item.Password)] = created.ID
		created.Protocol = item.Protocol
		created.Host = item.Host
		created.Port = item.Port
		created.Username = item.Username
		created.Password = item.Password
		created.ExpiresAt = expiresAt
		proxyByID[created.ID] = created
		// 把新建代理的 name 也加入反查表，供后续批内代理引用
		if created.Name != "" {
			proxyNameToID[created.Name] = created.ID
		}
		result.ProxyCreated++
		pendingProxyConfigs = append(pendingProxyConfigs, pendingProxyConfig{ID: created.ID, Item: item, Key: key})
	}

	// Apply status, expiry, and backup references only after every proxy has an
	// ID. This supports forward references and makes update failures visible.
	for _, pending := range pendingProxyConfigs {
		proxy := proxyByID[pending.ID]
		if proxy == nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{Kind: "proxy", Name: pending.Item.Name, ProxyKey: pending.Key, Message: "proxy disappeared before configuration"})
			continue
		}
		status := normalizeProxyStatus(pending.Item.Status)
		if status == "" {
			status = proxy.Status
		}
		fallbackMode := strings.TrimSpace(pending.Item.FallbackMode)
		if fallbackMode == "" {
			fallbackMode = proxy.FallbackMode
		}
		if fallbackMode == "" {
			fallbackMode = service.FallbackModeNone
		}
		expiresAt := proxy.ExpiresAt
		if pending.Item.ExpiresAt != nil {
			t := time.Unix(*pending.Item.ExpiresAt, 0).UTC()
			expiresAt = &t
		}

		var backupProxyID *int64
		if fallbackMode == service.FallbackModeProxy {
			var backupID int64
			var found bool
			if pending.Item.BackupProxyKey != "" {
				backupID, found = proxyKeyToID[pending.Item.BackupProxyKey]
			} else if pending.Item.BackupProxyName != "" {
				backupID, found = proxyNameToID[pending.Item.BackupProxyName]
			}
			if !found {
				result.ProxyFailed++
				result.Errors = append(result.Errors, DataImportError{Kind: "proxy", Name: pending.Item.Name, ProxyKey: pending.Key, Message: "backup proxy reference was not found"})
				continue
			}
			backupProxyID = &backupID
		}

		if _, updateErr := h.adminService.UpdateProxy(ctx, pending.ID, &service.UpdateProxyInput{
			Status:         status,
			ExpiresAt:      expiresAt,
			FallbackMode:   fallbackMode,
			BackupProxyID:  backupProxyID,
			ExpiryWarnDays: pending.Item.ExpiryWarnDays,
			Name:           proxy.Name,
			Protocol:       proxy.Protocol,
			Host:           proxy.Host,
			Port:           proxy.Port,
			Username:       proxy.Username,
			Password:       proxy.Password,
		}); updateErr != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{Kind: "proxy", Name: pending.Item.Name, ProxyKey: pending.Key, Message: updateErr.Error()})
		}
	}

	// 收集需要异步设置隐私的 Antigravity OAuth 账号
	var privacyAccounts []*service.Account

	for i := range dataPayload.Accounts {
		item := dataPayload.Accounts[i]
		if err := validateDataAccount(item); err != nil {
			result.AccountFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "account",
				Name:    item.Name,
				Message: err.Error(),
			})
			continue
		}

		var proxyID *int64
		if item.ProxyKey != nil && *item.ProxyKey != "" {
			if id, ok := proxyKeyToID[*item.ProxyKey]; ok {
				proxyID = &id
			} else {
				result.AccountFailed++
				result.Errors = append(result.Errors, DataImportError{
					Kind:     "account",
					Name:     item.Name,
					ProxyKey: proxyKeyForError(*item.ProxyKey),
					Message:  "proxy_key not found",
				})
				continue
			}
		}

		enrichCredentialsFromIDToken(&item)
		// The guard is meaningful only for a normal OpenAI OAuth account. Exported
		// bundles from older/custom builds can carry arbitrary extra keys; do not
		// let that stale setting make an unrelated account fail creation or alter
		// a Claude/API-key import.
		if item.Platform != service.PlatformOpenAI || item.Type != service.AccountTypeOAuth {
			delete(item.Extra, service.OpenAICodex429GuardEnabledExtraKey)
		}
		if req.Codex429GuardEnabled != nil && item.Platform == service.PlatformOpenAI && item.Type == service.AccountTypeOAuth {
			if item.Extra == nil {
				item.Extra = make(map[string]any)
			}
			item.Extra[service.OpenAICodex429GuardEnabledExtraKey] = *req.Codex429GuardEnabled
		}

		accountInput := &service.CreateAccountInput{
			Name:                         item.Name,
			Notes:                        item.Notes,
			Platform:                     item.Platform,
			Type:                         item.Type,
			Credentials:                  item.Credentials,
			Extra:                        item.Extra,
			ProxyID:                      proxyID,
			Concurrency:                  item.Concurrency,
			Priority:                     item.Priority,
			RateMultiplier:               item.RateMultiplier,
			GroupIDs:                     nil,
			ExpiresAt:                    item.ExpiresAt,
			AutoPauseOnExpired:           item.AutoPauseOnExpired,
			PreserveCodexFingerprintSeed: true,
			SkipDefaultGroupBind:         skipDefaultGroupBind,
			ConfirmOveragesRisk:          req.ConfirmOveragesRisk != nil && *req.ConfirmOveragesRisk,
		}

		created, err := h.adminService.CreateAccount(ctx, accountInput)
		if err != nil {
			result.AccountFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "account",
				Name:    item.Name,
				Message: err.Error(),
			})
			continue
		}
		// 收集 Antigravity OAuth 账号，稍后异步设置隐私
		if created.Platform == service.PlatformAntigravity && created.Type == service.AccountTypeOAuth {
			privacyAccounts = append(privacyAccounts, created)
		}
		h.scheduleGrokImportProbe(created)
		result.AccountCreated++
	}

	// 异步设置 Antigravity 隐私，避免大量导入时阻塞请求
	if len(privacyAccounts) > 0 {
		adminSvc := h.adminService
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("import_antigravity_privacy_panic", "recover", r)
				}
			}()
			bgCtx := context.Background()
			for _, acc := range privacyAccounts {
				adminSvc.ForceAntigravityPrivacy(bgCtx, acc)
			}
			slog.Info("import_antigravity_privacy_done", "count", len(privacyAccounts))
		}()
	}

	return result, nil
}

func (h *AccountHandler) listAllProxies(ctx context.Context) ([]service.Proxy, error) {
	page := 1
	pageSize := dataPageCap
	var out []service.Proxy
	for {
		items, total, err := h.adminService.ListProxies(ctx, page, pageSize, "", "", "", "created_at", "desc")
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) >= int(total) || len(items) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (h *AccountHandler) listAccountsFiltered(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode, sortBy, sortOrder string) ([]service.Account, error) {
	page := 1
	pageSize := dataPageCap
	var out []service.Account
	for {
		items, total, err := h.adminService.ListAccounts(ctx, page, pageSize, platform, accountType, status, search, groupID, privacyMode, sortBy, sortOrder)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) >= int(total) || len(items) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (h *AccountHandler) resolveExportAccounts(ctx context.Context, ids []int64, c *gin.Context) ([]service.Account, error) {
	if len(ids) > 0 {
		accounts, err := h.adminService.GetAccountsByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		out := make([]service.Account, 0, len(accounts))
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			out = append(out, *acc)
		}
		return out, nil
	}

	platform := c.Query("platform")
	accountType := c.Query("type")
	status := c.Query("status")
	privacyMode := strings.TrimSpace(c.Query("privacy_mode"))
	search := strings.TrimSpace(c.Query("search"))
	sortBy := c.DefaultQuery("sort_by", "name")
	sortOrder := c.DefaultQuery("sort_order", "asc")
	if len(search) > 100 {
		search = search[:100]
	}

	groupID := int64(0)
	if groupIDStr := c.Query("group"); groupIDStr != "" {
		if groupIDStr == accountListGroupUngroupedQueryValue {
			groupID = service.AccountListGroupUngrouped
		} else {
			parsedGroupID, parseErr := strconv.ParseInt(groupIDStr, 10, 64)
			if parseErr != nil || parsedGroupID <= 0 {
				return nil, infraerrors.BadRequest("INVALID_GROUP_FILTER", "invalid group filter")
			}
			groupID = parsedGroupID
		}
	}

	return h.listAccountsFiltered(ctx, platform, accountType, status, search, groupID, privacyMode, sortBy, sortOrder)
}

func (h *AccountHandler) resolveExportProxies(ctx context.Context, accounts []service.Account) ([]service.Proxy, error) {
	if len(accounts) == 0 {
		return []service.Proxy{}, nil
	}

	requested := make(map[int64]struct{})
	ids := make([]int64, 0)
	for i := range accounts {
		if accounts[i].ProxyID == nil {
			continue
		}
		id := *accounts[i].ProxyID
		if id <= 0 {
			continue
		}
		if _, ok := requested[id]; ok {
			continue
		}
		requested[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []service.Proxy{}, nil
	}

	proxies := make([]service.Proxy, 0, len(ids))
	for len(ids) > 0 {
		batch, err := h.adminService.GetProxiesByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		loaded := make(map[int64]struct{}, len(batch))
		next := make([]int64, 0)
		for i := range batch {
			proxy := batch[i]
			loaded[proxy.ID] = struct{}{}
			proxies = append(proxies, proxy)
			if proxy.BackupProxyID == nil || *proxy.BackupProxyID <= 0 {
				continue
			}
			backupID := *proxy.BackupProxyID
			if _, ok := requested[backupID]; ok {
				continue
			}
			requested[backupID] = struct{}{}
			next = append(next, backupID)
		}
		for _, id := range ids {
			if _, ok := loaded[id]; !ok {
				return nil, fmt.Errorf("proxy %d referenced by account backup data was not found", id)
			}
		}
		ids = next
	}
	return proxies, nil
}

func parseAccountIDs(c *gin.Context) ([]int64, error) {
	values := c.QueryArray("ids")
	if len(values) == 0 {
		raw := strings.TrimSpace(c.Query("ids"))
		if raw != "" {
			values = []string{raw}
		}
	}
	if len(values) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(values))
	for _, item := range values {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("invalid account id: %s", part)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func parseIncludeProxies(c *gin.Context) (bool, error) {
	raw := strings.TrimSpace(strings.ToLower(c.Query("include_proxies")))
	if raw == "" {
		return true, nil
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return true, fmt.Errorf("invalid include_proxies value: %s", raw)
	}
}

func validateDataHeader(payload DataPayload) error {
	if payload.Type != "" && payload.Type != dataType && payload.Type != legacyDataType {
		return fmt.Errorf("unsupported data type: %s", payload.Type)
	}
	if payload.Version != 0 && payload.Version != dataVersion {
		return fmt.Errorf("unsupported data version: %d", payload.Version)
	}
	if payload.Proxies == nil {
		return errors.New("proxies is required")
	}
	if payload.Accounts == nil {
		return errors.New("accounts is required")
	}
	return nil
}

func validateDataProxy(item DataProxy) error {
	if strings.TrimSpace(item.Protocol) == "" {
		return errors.New("proxy protocol is required")
	}
	if strings.TrimSpace(item.Host) == "" {
		return errors.New("proxy host is required")
	}
	if item.Port <= 0 || item.Port > 65535 {
		return errors.New("proxy port is invalid")
	}
	switch item.Protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy protocol is invalid: %s", item.Protocol)
	}
	if item.Status != "" {
		normalizedStatus := normalizeProxyStatus(item.Status)
		if normalizedStatus != service.StatusActive && normalizedStatus != "inactive" {
			return fmt.Errorf("proxy status is invalid: %s", item.Status)
		}
	}
	if mode := strings.TrimSpace(item.FallbackMode); mode != "" && mode != service.FallbackModeNone && mode != service.FallbackModeProxy && mode != service.FallbackModeDirect {
		return fmt.Errorf("proxy fallback_mode is invalid: %s", item.FallbackMode)
	}
	return nil
}

func validateDataAccount(item DataAccount) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("account name is required")
	}
	if strings.TrimSpace(item.Platform) == "" {
		return errors.New("account platform is required")
	}
	if item.Platform != strings.TrimSpace(item.Platform) || !service.IsAllowedQuotaPlatform(item.Platform) {
		return fmt.Errorf("account platform is invalid: %s", item.Platform)
	}
	if strings.TrimSpace(item.Type) == "" {
		return errors.New("account type is required")
	}
	if item.Type != strings.TrimSpace(item.Type) {
		return fmt.Errorf("account type is invalid: %s", item.Type)
	}
	if len(item.Credentials) == 0 {
		return errors.New("account credentials is required")
	}
	switch item.Type {
	case service.AccountTypeOAuth, service.AccountTypeSetupToken, service.AccountTypeAPIKey, service.AccountTypeUpstream, service.AccountTypeBedrock, service.AccountTypeServiceAccount:
	default:
		return fmt.Errorf("account type is invalid: %s", item.Type)
	}
	if raw, exists := item.Extra["allow_overages"]; exists {
		enabled, ok := raw.(bool)
		if !ok {
			return errors.New("allow_overages must be a boolean")
		}
		if enabled && item.Platform != service.PlatformAntigravity {
			return errors.New("allow_overages is only supported for Antigravity accounts")
		}
	}
	if err := service.ValidateCodexFingerprintExtra(item.Platform, item.Type, item.Extra); err != nil {
		return err
	}
	if item.RateMultiplier != nil && *item.RateMultiplier < 0 {
		return errors.New("rate_multiplier must be >= 0")
	}
	if item.Concurrency < 0 {
		return errors.New("concurrency must be >= 0")
	}
	if item.Priority < 0 {
		return errors.New("priority must be >= 0")
	}
	return nil
}

func defaultProxyName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "imported-proxy"
	}
	return name
}

// enrichCredentialsFromIDToken performs best-effort extraction of user info fields
// (email, plan_type, chatgpt_account_id, etc.) from id_token in credentials.
// Only applies to OpenAI OAuth accounts. Skips expired token errors silently.
// Existing credential values are never overwritten — only missing fields are filled.
func enrichCredentialsFromIDToken(item *DataAccount) {
	if item.Credentials == nil {
		return
	}
	// Only enrich OpenAI OAuth accounts
	platform := strings.ToLower(strings.TrimSpace(item.Platform))
	if platform != service.PlatformOpenAI {
		return
	}
	if strings.ToLower(strings.TrimSpace(item.Type)) != service.AccountTypeOAuth {
		return
	}

	idToken, _ := item.Credentials["id_token"].(string)
	if strings.TrimSpace(idToken) == "" {
		return
	}

	// DecodeIDToken skips expiry validation — safe for imported data
	claims, err := openai.DecodeIDToken(idToken)
	if err != nil {
		slog.Debug("import_enrich_id_token_decode_failed", "account", item.Name, "error", err)
		return
	}

	userInfo := claims.GetUserInfo()
	if userInfo == nil {
		return
	}

	// Fill missing fields only (never overwrite existing values)
	setIfMissing := func(key, value string) {
		if value == "" {
			return
		}
		if existing, _ := item.Credentials[key].(string); existing == "" {
			item.Credentials[key] = value
		}
	}

	setIfMissing("email", userInfo.Email)
	setIfMissing("plan_type", userInfo.PlanType)
	setIfMissing("chatgpt_account_id", userInfo.ChatGPTAccountID)
	setIfMissing("chatgpt_user_id", userInfo.ChatGPTUserID)
	setIfMissing("organization_id", userInfo.OrganizationID)
}

func normalizeProxyStatus(status string) string {
	normalized := strings.TrimSpace(strings.ToLower(status))
	switch normalized {
	case "":
		return ""
	case service.StatusActive:
		return service.StatusActive
	case "inactive", service.StatusDisabled:
		return "inactive"
	case "expired":
		// 导入 expired 代理按 inactive 处理，避免导入即触发到期改投逻辑
		return "inactive"
	default:
		return normalized
	}
}
