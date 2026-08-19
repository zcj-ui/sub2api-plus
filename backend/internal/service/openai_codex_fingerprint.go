package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFingerprintIDsContextKey 是暂存在 gin context 的收敛 ID 集合键。
// 由 Forward（非透传）或 forwardOpenAIPassthrough（透传）解析后写入，请求
// 构造器读取用于出站头改写——请求体与出站头必须共享同一份 IDs，保证
// turn_id 等随机字段一致。
const codexFingerprintIDsContextKey = "codex_fingerprint_ids"

// codexClientIdentityPassthroughContextKey records the identity decision made
// at ingress, before any compatibility transform can rebuild the request body.
// A complete official CLI snapshot must retain that decision for the lifetime
// of the request: later model/tool normalization is allowed to change protocol
// fields, but must never make the gateway start synthesizing a second client
// identity.
const codexClientIdentityPassthroughContextKey = "codex_client_identity_passthrough"

// codexFingerprintClientClassificationContextKey freezes the client
// classification made at ingress for downstream Codex-specific transforms.
// Account-level fingerprint convergence itself is governed solely by the
// explicit OAuth account mode, so a reverse proxy may safely remove the client
// UA and lifecycle carriers without disabling the configured projection.
const codexFingerprintClientClassificationContextKey = "codex_fingerprint_client_classification"

func stageCodexFingerprintClientClassification(c *gin.Context, isCodexClient bool) {
	if c == nil || !isCodexClient {
		return
	}
	c.Set(codexFingerprintClientClassificationContextKey, true)
}

// shouldApplyCodexFingerprintForRequest follows the account-level opt-in
// contract used by the official gateway: every OpenAI OAuth request uses the
// selected non-off convergence mode, even when a reverse proxy has stripped
// the Codex UA or lifecycle carriers. This is required for relays whose only
// surviving signal is the selected OAuth account. Off remains a true pass-
// through mode.
func shouldApplyCodexFingerprintForRequest(c *gin.Context, account *Account, body []byte) bool {
	_ = c
	_ = body
	return account != nil && account.IsOpenAIOAuth() && account.GetCodexFingerprintMode() != codexFingerprintOff
}

func hasCodexFingerprintIdentityCarrier(headers http.Header, body []byte) bool {
	if headers != nil {
		for _, name := range []string{
			"x-codex-installation-id",
			"session-id", "session_id",
			"thread-id", "thread_id",
			"x-codex-turn-metadata",
			"x-codex-window-id",
			"x-codex-parent-thread-id",
		} {
			if strings.TrimSpace(headers.Get(name)) != "" {
				return true
			}
		}
	}
	projection := codexIdentityFromBody(body)
	if !projection.valid {
		return false
	}
	tuple := projection.tuple
	return strings.TrimSpace(tuple.installationID) != "" ||
		strings.TrimSpace(tuple.sessionID) != "" ||
		strings.TrimSpace(tuple.threadID) != "" ||
		strings.TrimSpace(tuple.turnID) != "" ||
		strings.TrimSpace(tuple.windowID) != "" ||
		strings.TrimSpace(tuple.parentThreadID) != ""
}

// stageCodexFingerprintIDs 将本 attempt 解析出的收敛 ID 暂存到 gin context。
// 必须无条件覆写（含 nil）：failover 从收敛账号切到 off 账号时，上一账号的
// IDs 不得残留并被误应用到新账号的出站头（typed-nil 由应用侧 nil 守卫吸收）。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
		// Always replace the snapshot, including nil. A retry may switch accounts;
		// retaining the previous account's IDs would leak its identity upstream.
		c.Set(codexFingerprintIDsContextKey, ids)
	}
}

// applyStagedCodexFingerprintHeaders 读取 context 暂存的收敛 ID 并改写出站头。
// 非透传与透传两个请求构造器共用本函数，防止应用语义漂移。仅 OAuth 账号
// 生效（stale 键在账号类型混合 failover 下由该门挡住）。
func stagedCodexFingerprintIDs(c *gin.Context, account *Account) *codexFingerprintIDs {
	if c == nil || account == nil || account.Type != AccountTypeOAuth {
		return nil
	}
	value, ok := c.Get(codexFingerprintIDsContextKey)
	if !ok {
		return nil
	}
	ids, ok := value.(*codexFingerprintIDs)
	if !ok || !codexFingerprintIDsBelongToAccount(ids, account) {
		return nil
	}
	return ids
}

// applyStagedCodexFingerprintHeaders reads the request-scoped identity only
// when it belongs to the selected OAuth account. Every HTTP/WS builder uses
// the same ownership gate so a stale snapshot cannot cross an account
// failover boundary.
func applyStagedCodexFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	applyCodexFingerprintHeaders(h, stagedCodexFingerprintIDs(c, account))
}

// applyStagedCodexCompactHeaders is retained as a source-compatible hook for
// older callers. The official compact protocol is outside fingerprint
// convergence, so this function intentionally performs no projection.
func applyStagedCodexCompactHeaders(c *gin.Context, account *Account, h http.Header, body []byte) {
	_ = c
	_ = account
	_ = h
	_ = body
}

func firstHeaderValue(h http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(h.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// normalizeOpenAIOAuthSessionHeadersForIsolation closes the alias gap for the
// opt-out path. Canonical Codex headers are allowed through the ingress layer,
// but an OAuth account in `off` mode still needs the same per-API-key session
// isolation as the legacy underscore aliases. Keeping both spellings on the
// same isolated value also prevents compact helpers from reintroducing the raw
// client session after the normal builder has already isolated it.
func normalizeOpenAIOAuthSessionHeadersForIsolation(
	h http.Header,
	apiKeyID int64,
	rawSessionID string,
	rawThreadID string,
	rawRequestID string,
	rawConversationID string,
) {
	if h == nil {
		return
	}

	sessionID := ""
	if rawSessionID = strings.TrimSpace(rawSessionID); rawSessionID != "" {
		sessionID = isolateOpenAISessionID(apiKeyID, rawSessionID)
	} else {
		sessionID = strings.TrimSpace(h.Get("session_id"))
		if sessionID == "" {
			sessionID = strings.TrimSpace(h.Get("session-id"))
		}
	}
	if sessionID != "" {
		h.Set("session-id", sessionID)
		h.Set("session_id", sessionID)
	}

	threadID := ""
	if rawThreadID = strings.TrimSpace(rawThreadID); rawThreadID != "" {
		threadID = isolateOpenAISessionID(apiKeyID, rawThreadID)
	} else {
		threadID = strings.TrimSpace(h.Get("thread_id"))
		if threadID == "" {
			threadID = sessionID
		}
	}
	if threadID != "" {
		h.Set("thread-id", threadID)
		h.Set("thread_id", threadID)
	}

	if rawRequestID = strings.TrimSpace(rawRequestID); rawRequestID != "" {
		h.Set("x-client-request-id", isolateOpenAISessionID(apiKeyID, rawRequestID))
	} else if threadID != "" {
		h.Set("x-client-request-id", threadID)
	}

	if rawConversationID = strings.TrimSpace(rawConversationID); rawConversationID != "" {
		h.Set("conversation_id", isolateOpenAISessionID(apiKeyID, rawConversationID))
	}
}

func firstBodyString(body []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	return ""
}

// restoreCodexIdentityHeadersFromBody reconstructs only the canonical
// compatibility projections that the official CLI emits alongside
// client_metadata. A reverse proxy may remove those headers while leaving the
// JSON envelope intact; restoring the same values is passthrough, not account
// fingerprint synthesis. Existing headers always win so a mixed snapshot can
// never be silently repaired here (the strict identity gate rejects it).
func restoreCodexIdentityHeadersFromBody(h http.Header, body []byte, includeInstallation, includeRequestID bool) {
	if h == nil || len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.Exists() || !metadata.IsObject() {
		return
	}

	// client_metadata.x-codex-turn-metadata is the canonical source in the
	// official CLI. Some relays strip its flat compatibility fields while
	// preserving that JSON string. Parse the whole snapshot first so nested
	// identity can restore the same headers, and reject contradictory flat and
	// nested values instead of producing a mixed fingerprint.
	projection := codexIdentityFromBody(body)
	if !projection.valid {
		return
	}
	identity := projection.tuple
	setHeaderIfEmpty := func(name, value string) {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(h.Get(name)) == "" {
			h.Set(name, value)
		}
	}
	// Canonical Codex uses hyphenated session/thread headers, while older
	// ChatGPT compatibility paths still read underscore aliases. If either
	// spelling already exists, copy that explicit value to the missing alias;
	// never let a body projection create a contradictory pair.
	setHeaderAliasesIfEmpty := func(names []string, value string) {
		existing := firstHeaderValue(h, names...)
		if existing == "" {
			existing = strings.TrimSpace(value)
		}
		if existing == "" {
			return
		}
		for _, name := range names {
			if strings.TrimSpace(h.Get(name)) == "" {
				h.Set(name, existing)
			}
		}
	}

	if includeInstallation {
		setHeaderIfEmpty("x-codex-installation-id", identity.installationID)
	}
	setHeaderAliasesIfEmpty([]string{"session-id", "session_id"}, identity.sessionID)
	setHeaderAliasesIfEmpty([]string{"thread-id", "thread_id"}, identity.threadID)
	setHeaderIfEmpty("x-codex-window-id", identity.windowID)
	setHeaderIfEmpty("x-codex-parent-thread-id", identity.parentThreadID)
	if includeRequestID {
		// codex-api's Responses endpoint derives this compatibility header from
		// thread_id when the caller did not send one explicitly.
		setHeaderIfEmpty("x-client-request-id", identity.threadID)
	}
	if raw := codexClientMetadataCompatibilityJSONValue(metadata.Get("x-codex-turn-metadata")); raw != "" {
		setHeaderIfEmpty("x-codex-turn-metadata", raw)
	}
	setHeaderIfEmpty("x-codex-turn-state", strings.TrimSpace(metadata.Get("x-codex-turn-state").String()))
	setHeaderIfEmpty("x-openai-subagent", strings.TrimSpace(metadata.Get("x-openai-subagent").String()))
	setHeaderIfEmpty(
		"x-openai-internal-codex-responses-lite",
		firstNonEmptyGJSONString(
			metadata.Get("x-openai-internal-codex-responses-lite"),
			metadata.Get("ws_request_header_x_openai_internal_codex_responses_lite"),
		),
	)
}

// codexIdentityLifecycleTuplesAgree compares only the client-owned lifecycle
// fields that are safe to project from a body to missing transport headers.
// Installation is deliberately excluded: an enabled device convergence mode
// rewrites it in client_metadata before request construction, while an inbound
// header can still contain the pre-convergence value. That expected difference
// must not prevent restoration of an otherwise coherent client session.
func codexIdentityLifecycleTuplesAgree(left, right codexClientIdentityTuple) bool {
	return (left.sessionID == "" || right.sessionID == "" || left.sessionID == right.sessionID) &&
		(left.threadID == "" || right.threadID == "" || left.threadID == right.threadID) &&
		(left.windowID == "" || right.windowID == "" || left.windowID == right.windowID) &&
		(left.parentThreadID == "" || right.parentThreadID == "" || left.parentThreadID == right.parentThreadID)
}

// restoreStagedCodexIdentityHeadersFromBody fills only the session/thread,
// window, and parent compatibility projections missing from a selected OAuth
// account's outbound request. It is intentionally gated by the request-scoped
// fingerprint snapshot: generic or opt-out clients keep normal tenant
// isolation, while an explicitly converged body-only request can survive a
// relay that stripped those headers. Flat and nested body metadata must agree,
// and any explicit ingress/outbound lifecycle value must agree with the body;
// no existing header is overwritten.
func restoreStagedCodexIdentityHeadersFromBody(c *gin.Context, account *Account, h http.Header, body []byte) {
	if h == nil || account == nil || !account.IsOpenAIOAuth() {
		return
	}
	ids := stagedCodexFingerprintIDs(c, account)
	if ids == nil {
		return
	}
	if ids.projectionMalformed {
		// A malformed projection cannot pass the strict agreement gate, but its
		// surviving raw carriers are still useful for keeping device-mode body
		// isolation aligned with the transport aliases. Never overwrite an
		// explicit header here; the selected account's normal isolation step will
		// decide the final value later.
		if ids.fallbackIdentitySet {
			restoreMissingCodexLifecycleHeaders(h, ids.fallbackIdentity)
		}
		return
	}
	bodyProjection := codexIdentityFromBody(body)
	if !bodyProjection.valid {
		return
	}
	var inbound http.Header
	if c != nil && c.Request != nil {
		inbound = c.Request.Header
	}
	inboundProjection := codexIdentityFromHeaders(inbound)
	outboundProjection := codexIdentityFromHeaders(h)
	if !inboundProjection.valid || !outboundProjection.valid ||
		!codexIdentityLifecycleTuplesAgree(inboundProjection.tuple, bodyProjection.tuple) ||
		!codexIdentityLifecycleTuplesAgree(outboundProjection.tuple, bodyProjection.tuple) {
		return
	}

	restoreMissingCodexLifecycleHeaders(h, bodyProjection.tuple)
}

func restoreMissingCodexLifecycleHeaders(h http.Header, identity codexClientIdentityTuple) {
	if h == nil {
		return
	}
	setAliasesIfEmpty := func(names []string, value string) {
		existing := firstHeaderValue(h, names...)
		if existing == "" {
			existing = strings.TrimSpace(value)
		}
		if existing == "" {
			return
		}
		for _, name := range names {
			if strings.TrimSpace(h.Get(name)) == "" {
				h.Set(name, existing)
			}
		}
	}
	setIfEmpty := func(name, value string) {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(h.Get(name)) == "" {
			h.Set(name, value)
		}
	}
	setAliasesIfEmpty([]string{"session-id", "session_id"}, identity.sessionID)
	setAliasesIfEmpty([]string{"thread-id", "thread_id"}, identity.threadID)
	setIfEmpty("x-codex-window-id", identity.windowID)
	setIfEmpty("x-codex-parent-thread-id", identity.parentThreadID)
}

func codexClientMetadataJSONValue(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.Type == gjson.JSON {
		return strings.TrimSpace(value.Raw)
	}
	return strings.TrimSpace(value.String())
}

// codexClientMetadataCompatibilityJSONValue mirrors the official CLI's
// compatibility_headers projection. The body carries the canonical metadata,
// which may include the unbounded tool_namespaces_info inventory; the direct
// header intentionally omits that field. Invalid or non-object values are not
// promoted to a header, so a malformed body cannot be turned into a second,
// contradictory identity projection.
func codexClientMetadataCompatibilityJSONValue(value gjson.Result) string {
	raw := codexClientMetadataJSONValue(value)
	if raw == "" {
		return ""
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		return ""
	}
	if _, oversized := metadata["tool_namespaces_info"]; !oversized {
		// Preserve the client's byte representation when no projection is needed;
		// this keeps compatibility headers stable and avoids needless reordering.
		return raw
	}
	delete(metadata, "tool_namespaces_info")
	projected, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(projected)
}

// codexFingerprintMode 控制 OAuth 账号出站请求的设备指纹收敛强度。
// 多人共享同一 OAuth 账号时，每个用户的 Codex 客户端会携带各自不同的
// installation_id / session_id / thread_id，上游据此判定设备数和会话数。
// 收敛模式将这些标识改写为账号级恒定值，减少上游可见的设备/会话指纹。
type codexFingerprintMode string

const (
	// codexFingerprintOff 不做任何收敛，原样透传客户端标识。
	// 这是默认值：收敛是显式 opt-in 的（见 GetCodexFingerprintMode）。
	codexFingerprintOff codexFingerprintMode = "off"
	// codexFingerprintDevice 仅收敛 installation_id 为账号级恒定值。
	// 上游看到 1 台设备 + 多会话（每用户各自的 session）。
	codexFingerprintDevice codexFingerprintMode = "device"
	// codexFingerprintSession keeps one installation per account and one stable
	// session/thread per client conversation when explicitly enabled.
	codexFingerprintSession codexFingerprintMode = "session"
	// codexFingerprintFull 收敛所有标识：installation_id + session_id + thread_id。
	// 上游看到 1 台设备 + 1 会话 + 1 线程，最激进。
	codexFingerprintFull codexFingerprintMode = "full"
)

// codexFingerprintRuntimeMode validates a mode supplied by a pure helper.
// The account setting remains the authority for production requests, and its
// explicit opt-in values (device/session/full) are intentionally preserved;
// this guard only rejects malformed values at the helper boundary.
func codexFingerprintRuntimeMode(mode codexFingerprintMode) codexFingerprintMode {
	switch mode {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return mode
	default:
		return codexFingerprintOff
	}
}

func shouldPreserveCodexClientSessionIdentity(account *Account) bool {
	if account == nil || !account.IsOpenAIOAuth() ||
		codexFingerprintRuntimeMode(account.GetCodexFingerprintMode()) != codexFingerprintDevice {
		return false
	}
	return strings.TrimSpace(account.GetOpenAIDeviceID()) != "" || account.getCodexFingerprintSeed() != ""
}

// shouldPreserveCodexClientSessionIdentityForRequest is the request-scoped
// version used by outbound builders. Device mode intentionally keeps the
// caller's session/thread, but only after a valid fingerprint snapshot was
// produced for this exact body. A malformed turn-metadata projection must use
// the normal API-key isolation path; otherwise a raw client session can cross
// users while only installation_id is rewritten.
func shouldPreserveCodexClientSessionIdentityForRequest(c *gin.Context, account *Account) bool {
	if !shouldPreserveCodexClientSessionIdentity(account) {
		return false
	}
	ids := stagedCodexFingerprintIDs(c, account)
	return ids != nil &&
		codexFingerprintRuntimeMode(ids.mode) == codexFingerprintDevice &&
		!ids.projectionMalformed &&
		strings.TrimSpace(ids.installationID) != ""
}

// hasValidStagedCodexFingerprint reports whether this request already has an
// account-owned lifecycle snapshot. Session/full modes still isolate their
// session and thread fields, but they must not manufacture a separate
// conversation_id from prompt_cache_key when the snapshot is authoritative.
func hasValidStagedCodexFingerprint(c *gin.Context, account *Account) bool {
	ids := stagedCodexFingerprintIDs(c, account)
	return ids != nil && !ids.projectionMalformed && strings.TrimSpace(ids.installationID) != ""
}

// captureCodexClientIdentityPassthrough freezes the result from the first
// complete request shape seen at ingress. Keep this separate from the pure
// detector so direct builders and unit tests can still evaluate a supplied
// header/body pair without a Gin context.
func captureCodexClientIdentityPassthrough(c *gin.Context, account *Account, headers http.Header, body []byte) bool {
	if c != nil {
		if value, exists := c.Get(codexClientIdentityPassthroughContextKey); exists {
			if preserve, ok := value.(bool); ok {
				return preserve
			}
		}
	}
	preserve := shouldPassThroughCodexClientIdentityWithBody(account, headers, body)
	// Do not cache a negative decision made for an API-key/non-OAuth account.
	// A later failover may select an OAuth account for the same official Codex
	// request; that account must still be able to evaluate the untouched ingress
	// identity instead of inheriting the first account's false result.
	if c != nil && account != nil && account.IsOpenAIOAuth() {
		c.Set(codexClientIdentityPassthroughContextKey, preserve)
	}
	return preserve
}

// shouldPreserveCodexClientIdentityForRequest returns the immutable ingress
// decision when present. A retry can switch between OAuth accounts, so retain
// the client-owned identity only for OAuth accounts; account credentials,
// proxy routing, and foreign turn-state validation remain selected-account
// concerns in the request builders.
func shouldPreserveCodexClientIdentityForRequestWithBody(c *gin.Context, account *Account, headers http.Header, body []byte) bool {
	if account == nil || !account.IsOpenAIOAuth() {
		return false
	}
	if c != nil {
		if value, exists := c.Get(codexClientIdentityPassthroughContextKey); exists {
			if preserve, ok := value.(bool); ok {
				return preserve
			}
		}
	}
	return shouldPassThroughCodexClientIdentityWithBody(account, headers, body)
}

// shouldPassThroughCodexClientIdentityWithBody recognizes the complete
// identity snapshot emitted by the official Codex client. Reverse proxies can
// legitimately drop the canonical session/thread headers while preserving the
// request body, so the body projection is considered only when the caller has
// also supplied a strict official Codex User-Agent. The real CLI always emits
// that header; accepting originator alone would preserve an incomplete request
// after a proxy strips the User-Agent. Generic clients therefore keep the
// normal tenant-isolation path even if they happen to send similarly named
// client_metadata keys.
func shouldPassThroughCodexClientIdentityWithBody(account *Account, headers http.Header, body []byte) bool {
	if account == nil || !account.IsOpenAIOAuth() || headers == nil {
		return false
	}
	if !codexClientIdentityHeaderTripletConsistent(headers) {
		return false
	}
	headerProjection := codexIdentityFromHeaders(headers)
	bodyProjection := codexIdentityFromBody(body)
	if !headerProjection.valid || !bodyProjection.valid {
		return false
	}

	// When both projections survive the relay, reject a mixed snapshot. Sending
	// a body from one CLI session with headers from another is precisely the
	// half-identity shape that causes upstream routing and risk checks to fail.
	if !codexIdentityTuplesAgree(headerProjection.tuple, bodyProjection.tuple) {
		return false
	}
	return headerProjection.complete || bodyProjection.complete
}

// codexClientIdentityHeaderTripletConsistent verifies the transport-level
// portion of a genuine Codex CLI identity before the request can bypass the
// normal compatibility path. The official client derives User-Agent and
// Originator from the same originator value; older clients that send Version
// derive it from that same User-Agent version. Accepting only a recognizable
// User-Agent would pass an internally contradictory partial identity upstream.
func codexClientIdentityHeaderTripletConsistent(headers http.Header) bool {
	userAgent, userAgentOK := consistentCodexIdentityHeaderValue(headers, "user-agent")
	originator, originatorOK := consistentCodexIdentityHeaderValue(headers, "originator")
	version, versionOK := consistentCodexIdentityHeaderValue(headers, "version")
	if !userAgentOK || !originatorOK || !versionOK ||
		!openai.IsCodexOfficialClientRequestStrict(userAgent) || originator == "" {
		return false
	}

	slash := strings.IndexByte(userAgent, '/')
	if slash <= 0 || originator != strings.TrimSpace(userAgent[:slash]) {
		return false
	}
	if version == "" {
		return true
	}
	userAgentVersion := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(userAgent))
	return userAgentVersion != "" && version == userAgentVersion
}

type codexClientIdentityTuple struct {
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
	parentThreadID string
}

type codexClientIdentityProjection struct {
	tuple    codexClientIdentityTuple
	complete bool
	valid    bool
}

func codexIdentityFromHeaders(headers http.Header) codexClientIdentityProjection {
	if headers == nil {
		return codexClientIdentityProjection{valid: true}
	}
	installationID, installationOK := consistentCodexIdentityHeaderValue(headers, "x-codex-installation-id")
	sessionID, sessionOK := consistentCodexIdentityHeaderValue(headers, "session-id", "session_id")
	threadID, threadOK := consistentCodexIdentityHeaderValue(headers, "thread-id", "thread_id")
	windowID, windowOK := consistentCodexIdentityHeaderValue(headers, "x-codex-window-id")
	parentThreadID, parentThreadOK := consistentCodexIdentityHeaderValue(headers, "x-codex-parent-thread-id")
	metadata, metadataOK := consistentCodexIdentityHeaderValue(headers, "x-codex-turn-metadata")
	if !installationOK || !sessionOK || !threadOK || !windowOK || !parentThreadOK || !metadataOK {
		return codexClientIdentityProjection{valid: false}
	}
	identity := codexClientIdentityTuple{
		installationID: installationID,
		sessionID:      sessionID,
		threadID:       threadID,
		turnID:         "",
		windowID:       windowID,
		parentThreadID: parentThreadID,
	}
	if metadata != "" {
		metadataIdentity, ok := codexIdentityFromTurnMetadata(metadata)
		if !ok {
			return codexClientIdentityProjection{valid: false}
		}
		var agree bool
		identity, agree = mergeCodexIdentityTuple(identity, metadataIdentity)
		if !agree {
			return codexClientIdentityProjection{valid: false}
		}
	}
	return codexClientIdentityProjection{
		tuple:    identity,
		complete: codexIdentityTupleComplete(identity),
		valid:    true,
	}
}

func codexIdentityFromBody(body []byte) codexClientIdentityProjection {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return codexClientIdentityProjection{valid: true}
	}
	if !gjson.ValidBytes(trimmed) {
		return codexClientIdentityProjection{valid: false}
	}
	if !gjson.ParseBytes(trimmed).IsObject() {
		// Arrays and scalar request bodies do not carry a client_metadata
		// projection. Leave their normal request validation to the upstream.
		return codexClientIdentityProjection{valid: true}
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.Exists() {
		return codexClientIdentityProjection{valid: true}
	}
	if !metadata.IsObject() {
		return codexClientIdentityProjection{valid: false}
	}
	installationID, installationOK := consistentCodexBodyMetadataValue(
		metadata,
		"x-codex-installation-id",
		"installation_id",
	)
	windowID, windowOK := consistentCodexBodyMetadataValue(
		metadata,
		"x-codex-window-id",
		"window_id",
	)
	parentThreadID, parentThreadOK := consistentCodexBodyMetadataValue(
		metadata,
		"x-codex-parent-thread-id",
		"parent_thread_id",
	)
	if !installationOK || !windowOK || !parentThreadOK {
		return codexClientIdentityProjection{valid: false}
	}
	sessionID, sessionOK := consistentCodexBodyMetadataValue(metadata, "session_id", "session-id")
	threadID, threadOK := consistentCodexBodyMetadataValue(metadata, "thread_id", "thread-id")
	turnID, turnOK := consistentCodexBodyMetadataValue(metadata, "turn_id", "turn-id")
	if !sessionOK || !threadOK || !turnOK {
		return codexClientIdentityProjection{valid: false}
	}
	identity := codexClientIdentityTuple{
		installationID: installationID,
		sessionID:      sessionID,
		threadID:       threadID,
		turnID:         turnID,
		windowID:       windowID,
		parentThreadID: parentThreadID,
	}

	// The current CLI also carries the canonical tuple as a JSON string under
	// x-codex-turn-metadata. Accept an object form as well for compatible
	// clients that decode the envelope before forwarding it.
	turnMetadata := metadata.Get("x-codex-turn-metadata")
	if turnMetadata.Exists() {
		metadataIdentity, ok := codexIdentityFromTurnMetadataValue(turnMetadata)
		if !ok {
			return codexClientIdentityProjection{valid: false}
		}
		var agree bool
		identity, agree = mergeCodexIdentityTuple(identity, metadataIdentity)
		if !agree {
			return codexClientIdentityProjection{valid: false}
		}
	}
	return codexClientIdentityProjection{
		tuple:    identity,
		complete: codexIdentityTupleComplete(identity),
		valid:    true,
	}
}

func firstNonEmptyGJSONString(values ...gjson.Result) string {
	for _, value := range values {
		if value.Exists() {
			if candidate := strings.TrimSpace(value.String()); candidate != "" {
				return candidate
			}
		}
	}
	return ""
}

func consistentCodexBodyMetadataValue(metadata gjson.Result, names ...string) (string, bool) {
	value := ""
	for _, name := range names {
		candidate := strings.TrimSpace(metadata.Get(name).String())
		if candidate == "" {
			continue
		}
		if value != "" && value != candidate {
			return "", false
		}
		value = candidate
	}
	return value, true
}

func consistentCodexIdentityHeaderValue(headers http.Header, names ...string) (string, bool) {
	value := ""
	for key, values := range headers {
		matches := false
		for _, name := range names {
			if strings.EqualFold(strings.TrimSpace(key), name) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		for _, candidate := range values {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if value != "" && value != candidate {
				return "", false
			}
			value = candidate
		}
	}
	return value, true
}

// codexIdentityTupleFillMissing copies only fields absent from dst. It is used
// for malformed ingress snapshots where a deterministic source is still
// needed to keep the eventual header/body projections on one identity. The
// strict parsers above continue to reject contradictions; this helper is only
// a fallback for the isolated repair path.
func codexIdentityTupleFillMissing(dst *codexClientIdentityTuple, supplement codexClientIdentityTuple) {
	if dst == nil {
		return
	}
	if dst.installationID == "" {
		dst.installationID = supplement.installationID
	}
	if dst.sessionID == "" {
		dst.sessionID = supplement.sessionID
	}
	if dst.threadID == "" {
		dst.threadID = supplement.threadID
	}
	if dst.turnID == "" {
		dst.turnID = supplement.turnID
	}
	if dst.windowID == "" {
		dst.windowID = supplement.windowID
	}
	if dst.parentThreadID == "" {
		dst.parentThreadID = supplement.parentThreadID
	}
}

// codexBestEffortIdentityFromHeaders reads direct header carriers first, then
// fills missing fields from a valid embedded turn-metadata object. Direct
// headers are deliberately authoritative when the two sources conflict: the
// outbound OAuth isolation path already uses these values for its aliases.
func codexBestEffortIdentityFromHeaders(headers http.Header) codexClientIdentityTuple {
	if headers == nil {
		return codexClientIdentityTuple{}
	}
	identity := codexClientIdentityTuple{
		installationID: firstHeaderValue(headers, "x-codex-installation-id"),
		sessionID:      firstHeaderValue(headers, "session-id", "session_id"),
		threadID:       firstHeaderValue(headers, "thread-id", "thread_id"),
		windowID:       firstHeaderValue(headers, "x-codex-window-id"),
		parentThreadID: firstHeaderValue(headers, "x-codex-parent-thread-id"),
	}
	if metadata := strings.TrimSpace(headers.Get("x-codex-turn-metadata")); metadata != "" {
		if nested, ok := codexIdentityFromTurnMetadata(metadata); ok {
			codexIdentityTupleFillMissing(&identity, nested)
		}
	}
	return identity
}

// codexBestEffortIdentityFromBody follows the official precedence rule for a
// body snapshot: the nested x-codex-turn-metadata object is canonical, while
// flat client_metadata fields only fill gaps. When the nested value is
// malformed, flat fields remain usable for the isolation fallback.
func codexBestEffortIdentityFromBody(body []byte) codexClientIdentityTuple {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) || !gjson.ParseBytes(trimmed).IsObject() {
		return codexClientIdentityTuple{}
	}
	metadata := gjson.GetBytes(trimmed, "client_metadata")
	if !metadata.Exists() || !metadata.IsObject() {
		return codexClientIdentityTuple{}
	}
	identity := codexClientIdentityTuple{
		installationID: firstNonEmptyGJSONString(
			metadata.Get("x-codex-installation-id"),
			metadata.Get("installation_id"),
		),
		sessionID: firstNonEmptyGJSONString(metadata.Get("session_id"), metadata.Get("session-id")),
		threadID:  firstNonEmptyGJSONString(metadata.Get("thread_id"), metadata.Get("thread-id")),
		turnID:    firstNonEmptyGJSONString(metadata.Get("turn_id"), metadata.Get("turn-id")),
		windowID: firstNonEmptyGJSONString(
			metadata.Get("x-codex-window-id"),
			metadata.Get("window_id"),
		),
		parentThreadID: firstNonEmptyGJSONString(
			metadata.Get("x-codex-parent-thread-id"),
			metadata.Get("parent_thread_id"),
		),
	}
	if nestedValue := metadata.Get("x-codex-turn-metadata"); nestedValue.Exists() {
		if nested, ok := codexIdentityFromTurnMetadataValue(nestedValue); ok {
			// Nested metadata is canonical, so overlay the flat projection with
			// the nested tuple before any missing-field fill.
			codexIdentityTupleFillMissing(&nested, identity)
			identity = nested
		}
	}
	return identity
}

func codexBestEffortIdentityFromRequest(headers http.Header, body []byte) codexClientIdentityTuple {
	identity := codexBestEffortIdentityFromHeaders(headers)
	// Header values win; body metadata only supplies carriers removed by a
	// relay. This makes the isolated body match the same header identity.
	codexIdentityTupleFillMissing(&identity, codexBestEffortIdentityFromBody(body))
	return identity
}

func codexIdentityFromTurnMetadata(raw string) (codexClientIdentityTuple, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || codexTurnMetadataMalformed(raw) {
		return codexClientIdentityTuple{}, false
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return codexClientIdentityTuple{}, false
	}
	installationID, installationOK := consistentCodexMapMetadataValue(metadata, "installation_id", "x-codex-installation-id")
	windowID, windowOK := consistentCodexMapMetadataValue(metadata, "x-codex-window-id", "window_id")
	parentThreadID, parentThreadOK := consistentCodexMapMetadataValue(metadata, "x-codex-parent-thread-id", "parent_thread_id")
	if !installationOK || !windowOK || !parentThreadOK {
		return codexClientIdentityTuple{}, false
	}
	return codexClientIdentityTuple{
		installationID: installationID,
		sessionID:      strings.TrimSpace(stringValue(metadata["session_id"])),
		threadID:       strings.TrimSpace(stringValue(metadata["thread_id"])),
		turnID:         strings.TrimSpace(stringValue(metadata["turn_id"])),
		windowID:       windowID,
		parentThreadID: parentThreadID,
	}, true
}

func consistentCodexMapMetadataValue(metadata map[string]any, names ...string) (string, bool) {
	value := ""
	for _, name := range names {
		candidate := strings.TrimSpace(stringValue(metadata[name]))
		if candidate == "" {
			continue
		}
		if value != "" && value != candidate {
			return "", false
		}
		value = candidate
	}
	return value, true
}

func codexIdentityFromTurnMetadataValue(value gjson.Result) (codexClientIdentityTuple, bool) {
	if !value.Exists() {
		return codexClientIdentityTuple{}, false
	}
	if value.Type == gjson.JSON {
		return codexIdentityFromTurnMetadata(value.Raw)
	}
	return codexIdentityFromTurnMetadata(value.String())
}

func mergeCodexIdentityTuple(base, overlay codexClientIdentityTuple) (codexClientIdentityTuple, bool) {
	if base.installationID != "" && overlay.installationID != "" && base.installationID != overlay.installationID {
		return codexClientIdentityTuple{}, false
	}
	if base.sessionID != "" && overlay.sessionID != "" && base.sessionID != overlay.sessionID {
		return codexClientIdentityTuple{}, false
	}
	if base.threadID != "" && overlay.threadID != "" && base.threadID != overlay.threadID {
		return codexClientIdentityTuple{}, false
	}
	if base.turnID != "" && overlay.turnID != "" && base.turnID != overlay.turnID {
		return codexClientIdentityTuple{}, false
	}
	if base.windowID != "" && overlay.windowID != "" && base.windowID != overlay.windowID {
		return codexClientIdentityTuple{}, false
	}
	if base.parentThreadID != "" && overlay.parentThreadID != "" && base.parentThreadID != overlay.parentThreadID {
		return codexClientIdentityTuple{}, false
	}
	if base.installationID == "" {
		base.installationID = overlay.installationID
	}
	if base.sessionID == "" {
		base.sessionID = overlay.sessionID
	}
	if base.threadID == "" {
		base.threadID = overlay.threadID
	}
	if base.turnID == "" {
		base.turnID = overlay.turnID
	}
	if base.windowID == "" {
		base.windowID = overlay.windowID
	}
	if base.parentThreadID == "" {
		base.parentThreadID = overlay.parentThreadID
	}
	return base, true
}

func codexIdentityTupleComplete(identity codexClientIdentityTuple) bool {
	return strings.TrimSpace(identity.installationID) != "" &&
		strings.TrimSpace(identity.sessionID) != "" &&
		strings.TrimSpace(identity.threadID) != ""
}

func codexIdentityTuplesAgree(left, right codexClientIdentityTuple) bool {
	return (left.installationID == "" || right.installationID == "" || left.installationID == right.installationID) &&
		(left.sessionID == "" || right.sessionID == "" || left.sessionID == right.sessionID) &&
		(left.threadID == "" || right.threadID == "" || left.threadID == right.threadID) &&
		(left.turnID == "" || right.turnID == "" || left.turnID == right.turnID) &&
		(left.windowID == "" || right.windowID == "" || left.windowID == right.windowID) &&
		(left.parentThreadID == "" || right.parentThreadID == "" || left.parentThreadID == right.parentThreadID)
}

// normalizeCodexFingerprintModeForStorage canonicalizes the opt-in mode while
// preserving the complete lifecycle values used by the official Codex client.
func normalizeCodexFingerprintModeForStorage(extra map[string]any) map[string]any {
	if extra == nil {
		return extra
	}
	raw, ok := extra[codexFingerprintModeExtraKey].(string)
	if !ok {
		return extra
	}
	mode := codexFingerprintMode(strings.ToLower(strings.TrimSpace(raw)))
	switch mode {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
	default:
		return extra
	}
	if raw == string(mode) {
		return extra
	}
	out := make(map[string]any, len(extra))
	for key, value := range extra {
		out[key] = value
	}
	out[codexFingerprintModeExtraKey] = string(mode)
	return out
}

// canonicalCodexFingerprintSeed accepts the UUID spellings used by exports
// and older clients, then returns the single lowercase, trimmed representation
// persisted by the gateway. The seed is opaque account state; UUID version is
// intentionally unrestricted, and only the nil UUID is rejected.
func canonicalCodexFingerprintSeed(seed string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(seed))
	id, err := uuid.Parse(trimmed)
	if err != nil || id == uuid.Nil || trimmed != id.String() {
		return "", false
	}
	return trimmed, true
}

func normalizeCodexFingerprintSeedForStorage(extra map[string]any) map[string]any {
	if extra == nil {
		return extra
	}
	raw, ok := extra[codexFingerprintSeedExtraKey].(string)
	if !ok {
		return extra
	}
	canonical, ok := canonicalCodexFingerprintSeed(raw)
	if !ok || raw == canonical {
		return extra
	}
	out := make(map[string]any, len(extra))
	for key, value := range extra {
		out[key] = value
	}
	out[codexFingerprintSeedExtraKey] = canonical
	return out
}

// RetireCodexFingerprintExtra is retained as a compatibility name for callers
// from the previous migration. It now performs canonicalization only; the
// account seed and lifecycle mode must survive imports and edits when the
// administrator explicitly opted in.
func RetireCodexFingerprintExtra(extra map[string]any) map[string]any {
	return normalizeCodexFingerprintSeedForStorage(normalizeCodexFingerprintModeForStorage(extra))
}

const (
	codexFingerprintModeExtraKey = "codex_fingerprint_mode"
	// codexFingerprintSeedExtraKey is generated once for newly-created OAuth
	// accounts. It is deliberately kept in account extra so imports/backups keep
	// the same account identity without exposing a deployment secret.
	codexFingerprintSeedExtraKey = "codex_fingerprint_seed"
)

// codexFingerprintDeploymentSeed is retained for source compatibility with
// callers that still pass the deployment secret. Persisted account seeds are
// deliberately independent of it: restoring an account database must preserve
// the same Codex identity, while random per-account seeds already prevent
// collisions between independent deployments.
func codexFingerprintDeploymentSeed(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.JWT.Secret)
}

func (a *Account) getCodexFingerprintSeed() string {
	if a == nil || !a.IsOpenAIOAuth() {
		return ""
	}
	seed, ok := canonicalCodexFingerprintSeed(a.GetExtraString(codexFingerprintSeedExtraKey))
	if !ok {
		return ""
	}
	return seed
}

func isCodexFingerprintSeed(seed string) bool {
	_, ok := canonicalCodexFingerprintSeed(seed)
	return ok
}

func codexFingerprintModeEnabledExtra(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	mode, _ := extra[codexFingerprintModeExtraKey].(string)
	switch codexFingerprintMode(strings.ToLower(strings.TrimSpace(mode))) {
	case codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return true
	default:
		return false
	}
}

// ShouldEnsureCodexFingerprintSeedForExtraUpdates reports whether an extra
// patch explicitly enables a Codex convergence mode. Repositories use this
// narrow predicate to generate a missing seed in the same SQL transaction as
// the mode update, rather than performing a separate per-account write.
func ShouldEnsureCodexFingerprintSeedForExtraUpdates(extra map[string]any) bool {
	return codexFingerprintModeEnabledExtra(extra)
}

// ValidateCodexFingerprintExtra validates the explicit opt-in fields and keeps
// them limited to OpenAI OAuth accounts.
func ValidateCodexFingerprintExtra(platform, accountType string, extra map[string]any) error {
	if extra == nil {
		return nil
	}
	if rawMode, present := extra[codexFingerprintModeExtraKey]; present && rawMode != nil {
		mode, ok := rawMode.(string)
		mode = strings.ToLower(strings.TrimSpace(mode))
		if !ok || (mode != string(codexFingerprintOff) && mode != string(codexFingerprintDevice) && mode != string(codexFingerprintSession) && mode != string(codexFingerprintFull)) {
			return fmt.Errorf("codex_fingerprint_mode must be one of off, device, session, or full")
		}
		if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
			return fmt.Errorf("codex_fingerprint_mode only applies to OpenAI OAuth accounts")
		}
	}
	if rawSeed, present := extra[codexFingerprintSeedExtraKey]; present && rawSeed != nil {
		seed, ok := rawSeed.(string)
		if !ok || !isCodexFingerprintSeed(seed) {
			return fmt.Errorf("codex_fingerprint_seed must be a canonical non-nil UUID")
		}
		if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
			return fmt.Errorf("codex_fingerprint_seed only applies to OpenAI OAuth accounts")
		}
	}
	return nil
}

// ensureCodexFingerprintSeed adds a random seed only when convergence is
// explicitly enabled, preserving an existing seed across edits/imports.
func ensureCodexFingerprintSeed(platform, accountType string, extra map[string]any) map[string]any {
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
		return extra
	}
	if !codexFingerprintModeEnabledExtra(extra) {
		return extra
	}
	if rawSeed := extraStringValue(extra, codexFingerprintSeedExtraKey); rawSeed != "" {
		if seed, ok := canonicalCodexFingerprintSeed(rawSeed); ok {
			if rawSeed == seed {
				return extra
			}
			out := make(map[string]any, len(extra))
			for key, value := range extra {
				out[key] = value
			}
			out[codexFingerprintSeedExtraKey] = seed
			return out
		}
	}
	out := make(map[string]any, len(extra)+1)
	for key, value := range extra {
		out[key] = value
	}
	out[codexFingerprintSeedExtraKey] = uuid.NewString()
	return out
}

func extraStringValue(extra map[string]any, key string) string {
	if extra == nil {
		return ""
	}
	switch value := extra[key].(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

// GetCodexFingerprintMode 从账号 extra JSON 读取历史指纹收敛模式。
//
// **收敛是显式 opt-in**：未设置、空值或非法值一律按 off 处理，只有管理员
// 明确配置 device / session / full 才收敛。
//
// 历史：v0.1.175（#5553）把缺省值当作 session，导致升级后存量 OAuth 账号
// （普遍没有这个 extra 键）的每个非透传请求都被静默改写 installation /
// session / thread / turn / window 五类标识；#5555、#5556、#5582 报告的额度
// 缩水都卡在该版本边界，并有"回退 v0.1.173 即恢复"与"新账号开收敛后降额"
// 的 A/B 实测。上游的配额判定策略不可观测，因此这里取兼容安全的一侧：
// 不显式 opt-in 就保持 v0.1.175 之前的客户端身份（#5610）。
func (a *Account) GetCodexFingerprintMode() codexFingerprintMode {
	if a == nil || !a.IsOpenAIOAuth() {
		return codexFingerprintOff
	}
	raw := strings.ToLower(strings.TrimSpace(a.GetExtraString(codexFingerprintModeExtraKey)))
	switch codexFingerprintMode(raw) {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull:
		return codexFingerprintMode(raw)
	default:
		return codexFingerprintOff
	}
}

// deriveStableUUIDv4 从种子确定性派生一个 UUIDv4 格式的字符串。
// 同一种子永远返回同一值。
func deriveStableUUIDv4(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 优先使用管理员配置的真实 device_id，无则使用持久化的随机种子。
func resolveConvergedInstallationID(account *Account) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" {
		return deviceID
	}
	// The persisted seed is server-side state, not the client-visible
	// installation_id. Match the official sub2api derivation so an account
	// export/import produces the same stable UUID without exposing the seed.
	seed := account.getCodexFingerprintSeed()
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-install-id:v2:" + seed)
}

// resolveConvergedSessionID returns the account-wide session used by the
// official session and full convergence modes.
func resolveConvergedSessionID(account *Account) string {
	if account == nil {
		return ""
	}
	seed := account.getCodexFingerprintSeed()
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-session-id:v2:" + seed)
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(account *Account, clientSessionID string) string {
	if account == nil || clientSessionID == "" {
		return ""
	}
	seed := account.getCodexFingerprintSeed()
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-thread-id:v2:" + seed + ":" + clientSessionID)
}

// codexFingerprintIDs 收敛后的完整 ID 集合。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 等随机字段一致。
type codexFingerprintIDs struct {
	accountID           int64
	mode                codexFingerprintMode
	installationID      string
	sessionID           string
	threadID            string
	turnID              string
	windowID            string
	turnStartedAtUnixMS int64
	// projectionMalformed records whether the ingress request carried a
	// malformed client_metadata projection. The IDs can still be generated so
	// the outbound metadata is rebuilt into a legal shape, but device mode must
	// not preserve raw session/thread aliases for that request.
	projectionMalformed bool
	// fallbackIdentity is captured from the ingress snapshot before account
	// isolation. It is used only for malformed device projections, where the
	// body still needs to follow the same per-API-key values as the isolated
	// transport headers.
	fallbackIdentity    codexClientIdentityTuple
	fallbackAPIKeyID    int64
	fallbackIdentitySet bool
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的 session-id 头值（连字符形式），用于 session 模式下
// 的 thread_id 派生——每个真实 Codex 会话得到一个独立线程。
// 返回 nil 表示 off 模式，不需要改写。
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode, deploymentSeed ...string) *codexFingerprintIDs {
	// The old call surface carried the deployment JWT secret. Official
	// convergence is keyed solely by the persisted per-account seed, so it is
	// deliberately ignored to keep an import stable across deployments.
	_ = deploymentSeed
	if account == nil {
		return nil
	}
	mode = codexFingerprintRuntimeMode(mode)
	if mode == codexFingerprintOff {
		return nil
	}
	// A convergence snapshot is account state, not a best-effort derivation
	// from the legacy openai_device_id.  The official implementation requires
	// the persisted canonical non-nil UUID seed for every opt-in mode; without it, session/full
	// would otherwise emit empty lifecycle IDs and overwrite the client's
	// session/thread headers.  Fail closed until the account write path creates
	// the seed atomically.
	if account.getCodexFingerprintSeed() == "" {
		return nil
	}

	ids := &codexFingerprintIDs{accountID: account.ID, mode: mode}

	ids.installationID = resolveConvergedInstallationID(account)
	if ids.installationID == "" {
		return nil
	}

	switch mode {
	case codexFingerprintDevice:
		return ids

	case codexFingerprintSession:
		// Official session mode keeps one upstream session per account and
		// derives a separate thread for each real client conversation.
		ids.sessionID = resolveConvergedSessionID(account)
		ids.threadID = resolveConvergedThreadID(account, clientSessionID)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		ids.turnStartedAtUnixMS = time.Now().UnixMilli()
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(account)
		ids.threadID = ids.sessionID
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		ids.turnStartedAtUnixMS = time.Now().UnixMilli()
		return ids
	}

	return nil
}

func codexFingerprintIDsBelongToAccount(ids *codexFingerprintIDs, account *Account) bool {
	if ids == nil || account == nil || ids.accountID <= 0 || account.ID <= 0 {
		return false
	}
	// Do not let an invalid/stale snapshot become "off" through the runtime
	// normalizer and then pass an account that is currently off. Only the three
	// explicit convergence modes can ever be staged for an outbound request.
	snapshotMode := ids.mode
	if snapshotMode != codexFingerprintDevice &&
		snapshotMode != codexFingerprintSession &&
		snapshotMode != codexFingerprintFull {
		return false
	}
	if ids.accountID != account.ID || snapshotMode != account.GetCodexFingerprintMode() {
		return false
	}
	// Failover can change the account configuration while a request is in
	// flight. Bind the staged snapshot to the selected account's current
	// installation identity before applying it to an outbound request. Session
	// and full modes also carry an account-wide session derived from the
	// persisted seed. Comparing that projection is important when an
	// administrator keeps an explicit openai_device_id but rotates the seed:
	// the installation would remain equal while the lifecycle snapshot would be
	// stale. Device mode has no account-owned session projection, so the
	// installation comparison is the complete ownership check there.
	expectedInstallation := strings.TrimSpace(resolveConvergedInstallationID(account))
	if expectedInstallation == "" || expectedInstallation != strings.TrimSpace(ids.installationID) {
		return false
	}
	mode := snapshotMode
	if mode == codexFingerprintSession || mode == codexFingerprintFull {
		expectedSession := strings.TrimSpace(resolveConvergedSessionID(account))
		return expectedSession != "" && expectedSession == strings.TrimSpace(ids.sessionID)
	}
	return true
}

// extractClientSessionID 从请求头中提取客户端原始的会话标识。
// 优先取 session-id（连字符形式，Codex CLI 标准），回退到 session_id（下划线形式）。
// 返回的值尚未被 isolateOpenAISessionID 改写，是客户端的真实标识。
func extractClientSessionID(h http.Header) string {
	if v := strings.TrimSpace(h.Get("session-id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(h.Get("session_id")); v != "" {
		return v
	}
	return ""
}

// resolveCodexConversationSeed extracts only a client-owned conversation
// identity. A prompt cache key or request content is not a lifecycle identity:
// both can legitimately change between turns, and using either as a thread
// seed makes one real Codex session appear as many upstream threads. The
// account-level session is the documented fallback when the client omitted
// all lifecycle carriers.
func resolveCodexConversationSeed(clientHeaders http.Header, body []byte, apiKeyID int64) string {
	_ = apiKeyID // retained for callers compiled against the older helper signature
	if clientHeaders != nil {
		if explicit := extractClientSessionID(clientHeaders); explicit != "" {
			return explicit
		}
	}
	// Some reverse proxies remove the canonical headers while preserving the
	// official client_metadata envelope. Read both the flat fields and the
	// nested x-codex-turn-metadata projection. The shared body parser rejects a
	// contradictory/malformed projection instead of selecting one half of a
	// mixed identity. Never infer a lifecycle identity from cache/content.
	if projection := codexIdentityFromBody(body); projection.valid {
		if sessionID := strings.TrimSpace(projection.tuple.sessionID); sessionID != "" {
			return sessionID
		}
		if threadID := strings.TrimSpace(projection.tuple.threadID); threadID != "" {
			return threadID
		}
	}
	return ""
}

// resolveCodexFingerprintIDsFromRequest 从客户端原始请求头中提取 session-id，
// 结合账号配置一次性解析收敛 ID 集合。调用方应将返回的 ids 同时传给
// applyCodexFingerprintHeaders 和 applyCodexFingerprintClientMetadata。
func resolveCodexFingerprintIDsFromRequest(account *Account, clientHeaders http.Header, deploymentSeed ...string) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	clientSessionID := ""
	if clientHeaders != nil {
		clientSessionID = extractClientSessionID(clientHeaders)
	}
	ids := resolveCodexFingerprintIDs(account, clientSessionID, mode, deploymentSeed...)
	if ids != nil {
		ids.projectionMalformed = codexFingerprintProjectionMalformed(clientHeaders, nil)
		if ids.projectionMalformed {
			ids.fallbackIdentity = codexBestEffortIdentityFromRequest(clientHeaders, nil)
			ids.fallbackIdentitySet = true
		}
	}
	return ids
}

func resolveCodexFingerprintIDsForRequest(account *Account, clientHeaders http.Header, body []byte, apiKeyID int64, deploymentSeed ...string) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	clientSessionID := resolveCodexConversationSeed(clientHeaders, body, apiKeyID)
	ids := resolveCodexFingerprintIDs(account, clientSessionID, mode, deploymentSeed...)
	if ids != nil {
		ids.projectionMalformed = codexFingerprintProjectionMalformed(clientHeaders, body)
		if ids.projectionMalformed {
			ids.fallbackIdentity = codexBestEffortIdentityFromRequest(clientHeaders, body)
			ids.fallbackAPIKeyID = apiKeyID
			ids.fallbackIdentitySet = true
		}
	}
	return ids
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 的白名单透传之后、enforceCodexIdentityHeaders 之前调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}
	// Keep the transport-level projection in sync with client_metadata. The
	// official Codex gateway sends x-codex-installation-id on ordinary
	// Responses, passthrough, and WebSocket requests as well as compact.
	// Set it before rebuilding embedded metadata so a stale client value cannot
	// survive when the embedded turn metadata cannot be decoded.
	h.Set("x-codex-installation-id", ids.installationID)

	// 所有非 off 模式都收敛 installation_id
	// The direct installation projection is shared by regular HTTP/WS and
	// compact; session/full lifecycle fields are handled below.
	// A request ID identifies one upstream attempt, not a conversation. Keeping
	// it stable across a session makes unrelated turns look like replays.

	if ids.mode == codexFingerprintDevice {
		fields := map[string]any{
			"installation_id": ids.installationID,
		}
		if ids.projectionMalformed && ids.fallbackIdentitySet {
			// The normal builder has already isolated direct session/thread
			// headers. Rebuild an existing embedded snapshot from the same raw
			// source so it cannot reintroduce the client's unisolated aliases.
			fallback := ids.fallbackIdentity
			if fallback.sessionID != "" {
				fields["session_id"] = isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.sessionID)
			}
			if fallback.threadID != "" {
				fields["thread_id"] = isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.threadID)
			}
			if fallback.turnID != "" {
				fields["turn_id"] = fallback.turnID
			}
			if fallback.windowID != "" {
				fields["window_id"] = fallback.windowID
			}
			if fallback.parentThreadID != "" {
				fields["parent_thread_id"] = fallback.parentThreadID
			}
		}
		rewriteCodexTurnMetadataFields(h, fields)
		return
	}

	// session / full 模式：改写所有相关头
	// Defensive guard for snapshots produced by legacy callers. Do not emit a
	// second stateless session graph after the runtime mode gate above.
	if ids.mode != codexFingerprintSession && ids.mode != codexFingerprintFull {
		return
	}
	h.Set("x-codex-window-id", ids.windowID)
	// Codex ResponsesClient uses the thread id as the request identity.
	h.Set("x-client-request-id", ids.threadID)
	// 连字符形式和下划线形式都改写，保证一致
	h.Set("session-id", ids.sessionID)
	h.Set("session_id", ids.sessionID)
	h.Set("thread-id", ids.threadID)
	h.Set("thread_id", ids.threadID)

	rewriteCodexTurnMetadataFields(h, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	})
	if ids.projectionMalformed && ids.fallbackIdentitySet && ids.fallbackIdentity.parentThreadID != "" {
		// Parent threads are client-owned in session/full modes. When the
		// ingress projections disagree, choose the same header-priority value
		// that the body repair path uses instead of leaving a split graph.
		h.Set("x-codex-parent-thread-id", ids.fallbackIdentity.parentThreadID)
	}
}

func applyCodexFingerprintToBodyBytes(body []byte, ids *codexFingerprintIDs) ([]byte, bool) {
	if len(body) == 0 || ids == nil {
		return body, false
	}
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return body, false
	}
	if !applyCodexFingerprintClientMetadata(decoded, ids) {
		return body, false
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(decoded)
	if err != nil {
		return body, false
	}
	return rebuilt, true
}

func nextCodexFingerprintTurn(ids *codexFingerprintIDs) *codexFingerprintIDs {
	if ids == nil {
		return nil
	}
	next := *ids
	if next.mode == codexFingerprintSession || next.mode == codexFingerprintFull {
		next.turnID = uuid.Must(uuid.NewV7()).String()
		next.turnStartedAtUnixMS = time.Now().UnixMilli()
	}
	return &next
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写。保留未指定字段原样（如 sandbox、thread_source 等）。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	raw := strings.TrimSpace(h.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		// Match the official Codex compatibility projection: a malformed
		// embedded snapshot is replaced with a minimal legal object instead of
		// leaving only the flat installation header rewritten.
		metadata = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		metadata[k] = v
	}
	rebuilt, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	h.Set("x-codex-turn-metadata", string(rebuilt))
}

// applyCodexFingerprintClientMetadata 按预计算的收敛 ID 改写请求体中的 client_metadata。
// 使用与头改写相同的 ids 实例，确保 turn_id 等随机字段一致。
func applyCodexFingerprintClientMetadata(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil || ids == nil {
		return false
	}

	var existing map[string]any
	createdMetadata := false
	replaceMetadata := false
	switch raw := reqBody["client_metadata"].(type) {
	case map[string]any:
		existing = raw
	case map[string]string:
		existing = make(map[string]any, len(raw))
		replaceMetadata = true
		for key, value := range raw {
			existing[key] = value
		}
	case nil:
		createdMetadata = true
	default:
		// Preserve an explicitly malformed/non-object value rather than
		// replacing caller data with a synthetic object.
		return false
	}
	if createdMetadata {
		// An opted-in account must carry the installation projection even when
		// an otherwise generic Responses request omitted client_metadata. Keep
		// this creation behind the non-nil fingerprint snapshot so off-mode
		// requests remain byte-for-byte untouched.
		existing = make(map[string]any, 1)
	}

	originalSessionID := strings.TrimSpace(stringValue(existing["session_id"]))
	modified := applyCodexFingerprintToClientMetadataMap(existing, ids)
	if !modified {
		return false
	}
	if createdMetadata || replaceMetadata {
		reqBody["client_metadata"] = existing
	}
	if rewriteCodexPromptCacheKey(reqBody, ids, originalSessionID) {
		modified = true
	}
	return modified
}

// rewriteCodexPromptCacheKey keeps the Responses body cache key aligned with
// the converged session header. Codex emits these as one conversation identity;
// leaving the client key untouched would split cache affinity after convergence.
func rewriteCodexPromptCacheKey(reqBody map[string]any, ids *codexFingerprintIDs, originalSessionID string) bool {
	if reqBody == nil || ids == nil ||
		codexFingerprintRuntimeMode(ids.mode) != codexFingerprintSession &&
			codexFingerprintRuntimeMode(ids.mode) != codexFingerprintFull ||
		strings.TrimSpace(ids.sessionID) == "" {
		return false
	}
	current, ok := reqBody["prompt_cache_key"].(string)
	if !ok || strings.TrimSpace(current) == "" || strings.TrimSpace(originalSessionID) == "" ||
		strings.TrimSpace(current) != strings.TrimSpace(originalSessionID) || current == ids.sessionID {
		return false
	}
	reqBody["prompt_cache_key"] = ids.sessionID
	return true
}

func setExistingClientMetadataAliases(metadata map[string]any, names []string, value string) {
	value = strings.TrimSpace(value)
	if metadata == nil || value == "" {
		return
	}
	for _, name := range names {
		if _, exists := metadata[name]; exists {
			metadata[name] = value
		}
	}
}

// applyCodexFingerprintToClientMetadataMap 是 client_metadata 改写的共享核心，
// map 版（非透传，body 已解码）与 raw 字节版（透传热路径）都经由它，保证两条
// 路径的收敛语义永不漂移。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *codexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}
	mode := codexFingerprintRuntimeMode(ids.mode)
	if mode == codexFingerprintOff {
		return false
	}

	modified := false

	if ids.installationID != "" {
		existing["x-codex-installation-id"] = ids.installationID
		// A few compatible clients use the unprefixed body alias alongside the
		// canonical x-codex key. Keep an existing alias in lockstep instead of
		// forwarding two contradictory installation identities.
		if _, exists := existing["installation_id"]; exists {
			existing["installation_id"] = ids.installationID
		}
		modified = true
	}

	if mode == codexFingerprintDevice {
		if ids.projectionMalformed && ids.fallbackIdentitySet {
			fallback := ids.fallbackIdentity
			// Only rewrite fields that were already present in the flat
			// projection. A body with no lifecycle carriers must not acquire a
			// synthetic session graph merely because device convergence is on.
			setExistingClientMetadataAliases(existing, []string{"session_id", "session-id"}, isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.sessionID))
			setExistingClientMetadataAliases(existing, []string{"thread_id", "thread-id"}, isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.threadID))
			setExistingClientMetadataAliases(existing, []string{"turn_id", "turn-id"}, fallback.turnID)
			setExistingClientMetadataAliases(existing, []string{"x-codex-window-id", "window_id"}, fallback.windowID)
			setExistingClientMetadataAliases(existing, []string{"x-codex-parent-thread-id", "parent_thread_id"}, fallback.parentThreadID)
		}
		fields := map[string]any{
			"installation_id":         ids.installationID,
			"x-codex-installation-id": ids.installationID,
		}
		if ids.projectionMalformed && ids.fallbackIdentitySet {
			fallback := ids.fallbackIdentity
			if fallback.sessionID != "" {
				fields["session_id"] = isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.sessionID)
			}
			if fallback.threadID != "" {
				fields["thread_id"] = isolateOpenAISessionID(ids.fallbackAPIKeyID, fallback.threadID)
			}
			if fallback.turnID != "" {
				fields["turn_id"] = fallback.turnID
			}
			if fallback.windowID != "" {
				fields["window_id"] = fallback.windowID
			}
			if fallback.parentThreadID != "" {
				fields["parent_thread_id"] = fallback.parentThreadID
			}
		}
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id":         fields["installation_id"],
			"x-codex-installation-id": fields["x-codex-installation-id"],
		})
		// Reapply the fallback lifecycle only when an embedded projection was
		// present. The helper above intentionally leaves a missing envelope
		// absent, preserving generic request shape in device mode.
		if _, exists := existing["x-codex-turn-metadata"]; exists && len(fields) > 2 {
			rewriteClientMetadataEmbeddedTurnMetadata(existing, fields)
		}
		return modified
	}

	// session / full 模式
	existing["session_id"] = ids.sessionID
	existing["thread_id"] = ids.threadID
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID
	if _, exists := existing["session-id"]; exists {
		existing["session-id"] = ids.sessionID
	}
	if _, exists := existing["thread-id"]; exists {
		existing["thread-id"] = ids.threadID
	}
	if _, exists := existing["window_id"]; exists {
		existing["window_id"] = ids.windowID
	}
	if ids.projectionMalformed && ids.fallbackIdentitySet && ids.fallbackIdentity.parentThreadID != "" {
		setExistingClientMetadataAliases(
			existing,
			[]string{"x-codex-parent-thread-id", "parent_thread_id"},
			ids.fallbackIdentity.parentThreadID,
		)
	}

	rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
		"installation_id":         ids.installationID,
		"x-codex-installation-id": ids.installationID,
		"session_id":              ids.sessionID,
		"session-id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"thread-id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"x-codex-window-id":       ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	})
	if ids.projectionMalformed && ids.fallbackIdentitySet && ids.fallbackIdentity.parentThreadID != "" {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"parent_thread_id":         ids.fallbackIdentity.parentThreadID,
			"x-codex-parent-thread-id": ids.fallbackIdentity.parentThreadID,
		})
	}
	return true
}

// applyCodexFingerprintClientMetadataRaw 在原始 JSON 字节上改写 client_metadata，
// 供透传路径使用——透传是热路径，禁止对可能高达数十 MB 的 body 做全量
// Unmarshal（见 forwardOpenAIPassthrough 的轻量提取注释）。实现为：gjson 提取
// client_metadata 小对象单独解码，经共享核心改写后 sjson 一次性拼回，body
// 其余字节原样保留。语义与 applyCodexFingerprintClientMetadata 逐点一致
// （含"非对象值整体替换为收敛集合"的行为）。
func applyCodexFingerprintClientMetadataRaw(body []byte, ids *codexFingerprintIDs) ([]byte, bool, error) {
	if len(body) == 0 || ids == nil {
		return body, false, nil
	}
	// 非 JSON 对象的 body（数组/标量/畸形）没有 client_metadata 语义，
	// sjson 在这类根上写字段会改写整体结构，直接放行保持原样。
	if !gjson.ParseBytes(body).IsObject() {
		return body, false, nil
	}

	cm := gjson.GetBytes(body, "client_metadata")
	createdMetadata := !cm.Exists() || cm.Type == gjson.Null
	existing := map[string]any{}
	if !createdMetadata {
		if !cm.IsObject() {
			// Do not replace a caller-provided non-object value. It has no valid
			// client_metadata semantics and rewriting it would change the request
			// shape beyond the opted-in fingerprint projection.
			return body, false, nil
		}
		if err := json.Unmarshal([]byte(cm.Raw), &existing); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for fingerprint: %w", err)
		}
	}

	originalSessionID := strings.TrimSpace(stringValue(existing["session_id"]))
	if !applyCodexFingerprintToClientMetadataMap(existing, ids) {
		return body, false, nil
	}

	raw, err := json.Marshal(existing)
	if err != nil {
		return body, false, fmt.Errorf("encode converged client_metadata: %w", err)
	}
	next, err := sjson.SetRawBytes(body, "client_metadata", raw)
	if err != nil {
		return body, false, fmt.Errorf("splice converged client_metadata: %w", err)
	}
	if ids.mode == codexFingerprintSession || ids.mode == codexFingerprintFull {
		if current := strings.TrimSpace(gjson.GetBytes(next, "prompt_cache_key").String()); current != "" && current != ids.sessionID {
			if strings.TrimSpace(originalSessionID) != "" && current == strings.TrimSpace(originalSessionID) {
				updated, setErr := sjson.SetBytes(next, "prompt_cache_key", ids.sessionID)
				if setErr != nil {
					return body, false, fmt.Errorf("splice converged prompt_cache_key: %w", setErr)
				}
				next = updated
			}
		}
	}
	return next, true, nil
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	value, exists := clientMetadata["x-codex-turn-metadata"]
	if !exists || value == nil {
		return
	}
	apply := func(metadata map[string]any) {
		for k, v := range fields {
			metadata[k] = v
		}
	}
	switch raw := value.(type) {
	case string:
		if strings.TrimSpace(raw) == "" {
			metadata := make(map[string]any, len(fields))
			apply(metadata)
			if rebuilt, err := json.Marshal(metadata); err == nil {
				clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
			}
			return
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
			metadata = make(map[string]any, len(fields))
		}
		apply(metadata)
		if rebuilt, err := json.Marshal(metadata); err == nil {
			clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
		}
	case map[string]any:
		apply(raw)
	case map[string]string:
		for key, value := range fields {
			if text, ok := value.(string); ok {
				raw[key] = text
			}
		}
	default:
		metadata := make(map[string]any, len(fields))
		apply(metadata)
		clientMetadata["x-codex-turn-metadata"] = metadata
	}
}

func codexTurnMetadataMalformed(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return true
	}
	return metadata == nil
}

func codexFingerprintProjectionMalformed(headers http.Header, body []byte) bool {
	headerProjection := codexIdentityFromHeaders(headers)
	bodyProjection := codexIdentityFromBody(body)
	if !headerProjection.valid || !bodyProjection.valid {
		return true
	}
	// Installation and turn IDs are intentionally excluded here. Device mode
	// rewrites installation_id, and a pooled WS connection legitimately rotates
	// turn_id on each response.create frame. The stable lifecycle tuple must
	// still agree across every surviving carrier.
	return !codexIdentityLifecycleTuplesAgree(headerProjection.tuple, bodyProjection.tuple)
}
