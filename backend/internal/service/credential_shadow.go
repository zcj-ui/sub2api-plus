package service

import (
	"context"
	"fmt"
	"maps"
)

// openAIShadowUpstreamProfileExtraKeys are account-level settings that affect
// how an OpenAI/Codex request is authenticated, transformed, or routed. Spark
// shadows keep their own model mappings/quota state, while these upstream
// controls are inherited from the parent at request time. Keeping this list
// explicit prevents accidental credential/quota sharing as new Extra fields
// are added.
var openAIShadowUpstreamProfileExtraKeys = [...]string{
	openAILongContextBillingEnabledKey,
	"openai_device_id",
	codexFingerprintSeedExtraKey,
	"codex_fingerprint_mode",
	"openai_oauth_responses_websockets_v2_mode",
	"openai_oauth_responses_websockets_v2_enabled",
	"responses_websockets_v2_enabled",
	"openai_ws_enabled",
	"openai_ws_force_http",
	"openai_passthrough",
	"openai_oauth_passthrough",
	"openai_responses_flatten_namespaces",
	"codex_cli_only",
	"codex_cli_only_allowed_clients",
	"codex_cli_only_allow_app_server",
	"codex_image_generation_bridge",
	"codex_image_generation_bridge_enabled",
	"codex_image_generation_explicit_tool_policy",
	"openai_compact_mode",
	"openai_responses_mode",
}

var openAIShadowUpstreamProfileNestedExtraKeys = [...]string{
	"codex_image_generation_bridge",
	"codex_image_generation_bridge_enabled",
	"codex_image_generation_explicit_tool_policy",
}

// resolveCredentialAccount 解析影子账号到其母账号，用于凭据/Token 透传。
// - 普通账号（非影子）：直接返回自身。
// - 影子账号：通过 repo 取母账号，校验母账号存在且为 OpenAI OAuth 类型，否则返回错误。
// 设计为包级函数（非任何 service 的方法），以便 OpenAIGatewayService / OpenAIQuotaService /
// AccountUsageService 等不同接收者共享同一实现。
func resolveCredentialAccount(ctx context.Context, repo AccountRepository, account *Account) (*Account, error) {
	if account == nil || !account.IsShadow() {
		return account, nil
	}
	parent, err := repo.GetByID(ctx, *account.ParentAccountID)
	if err != nil {
		return nil, fmt.Errorf("resolve spark shadow parent %d: %w", *account.ParentAccountID, err)
	}
	if parent == nil {
		return nil, fmt.Errorf("spark shadow parent %d not found", *account.ParentAccountID)
	}
	// 防御:创建路径已禁二级影子(G6),此处再挡一层——畸形数据/手工 DB 写出的影子→影子链
	// 会让凭据解析停在无凭据的一级影子(只解一层),fail-closed 比静默返回坏母更安全(外审第6轮)。
	if parent.IsShadow() {
		return nil, fmt.Errorf("spark shadow parent %d is itself a shadow", parent.ID)
	}
	if !parent.IsOpenAIOAuth() {
		return nil, fmt.Errorf("spark shadow parent %d is not OpenAI OAuth", parent.ID)
	}
	return parent, nil
}

// InheritOpenAIShadowUpstreamProfile returns an internal, request-scoped view
// of a Spark shadow with the parent's OpenAI credentials and upstream profile.
// The shadow's model mappings and identity/quota fields remain authoritative.
// Neither input is mutated, and the returned view must never be persisted.
func InheritOpenAIShadowUpstreamProfile(shadow, parent *Account) *Account {
	if shadow == nil || parent == nil {
		return nil
	}
	projected := *shadow
	// The parent is authoritative for egress identity.  Persisted proxy
	// propagation normally keeps the shadow row aligned, but a request can race
	// that propagation; using the parent snapshot here prevents a single turn
	// from bypassing the configured proxy.
	projected.ProxyID = parent.ProxyID
	projected.Proxy = parent.Proxy
	projected.ProxyFallbackOriginID = parent.ProxyFallbackOriginID
	projected.ProxyFallbackOriginName = parent.ProxyFallbackOriginName
	projected.Credentials = maps.Clone(parent.Credentials)
	if projected.Credentials == nil {
		projected.Credentials = make(map[string]any, 2)
	}
	for _, key := range []string{"model_mapping", "compact_model_mapping"} {
		delete(projected.Credentials, key)
		if value, ok := shadow.Credentials[key]; ok {
			projected.Credentials[key] = value
		}
	}

	projected.Extra = maps.Clone(shadow.Extra)
	if projected.Extra == nil {
		projected.Extra = make(map[string]any, len(openAIShadowUpstreamProfileExtraKeys))
	}
	for _, key := range openAIShadowUpstreamProfileExtraKeys {
		delete(projected.Extra, key)
		if value, ok := parent.Extra[key]; ok {
			projected.Extra[key] = value
		}
	}

	// A few legacy clients store image-generation switches under an "openai"
	// nested object. Merge only the known OpenAI keys and preserve unrelated
	// shadow-local values.
	shadowOpenAI, shadowHasOpenAI := shadow.Extra[PlatformOpenAI].(map[string]any)
	parentOpenAI, parentHasOpenAI := parent.Extra[PlatformOpenAI].(map[string]any)
	if shadowHasOpenAI || parentHasOpenAI {
		projectedOpenAI := maps.Clone(shadowOpenAI)
		if projectedOpenAI == nil {
			projectedOpenAI = make(map[string]any, len(openAIShadowUpstreamProfileNestedExtraKeys))
		}
		for _, key := range openAIShadowUpstreamProfileNestedExtraKeys {
			delete(projectedOpenAI, key)
			if value, ok := parentOpenAI[key]; ok {
				projectedOpenAI[key] = value
			}
		}
		if len(projectedOpenAI) == 0 {
			delete(projected.Extra, PlatformOpenAI)
		} else {
			projected.Extra[PlatformOpenAI] = projectedOpenAI
		}
	}
	return &projected
}

func effectiveOpenAIShadowUpstreamProfile(account *Account, lookup func(int64) *Account) *Account {
	if account == nil || !account.IsShadow() {
		return account
	}
	if lookup == nil || account.ParentAccountID == nil {
		return nil
	}
	parent := lookup(*account.ParentAccountID)
	if parent == nil || parent.IsShadow() || !parent.IsOpenAIOAuth() {
		return nil
	}
	return InheritOpenAIShadowUpstreamProfile(account, parent)
}
