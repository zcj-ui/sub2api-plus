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

// stageCodexFingerprintIDs 将本 attempt 解析出的收敛 ID 暂存到 gin context。
// 必须无条件覆写（含 nil）：failover 从收敛账号切到 off 账号时，上一账号的
// IDs 不得残留并被误应用到新账号的出站头（typed-nil 由应用侧 nil 守卫吸收）。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
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

// applyStagedCodexCompactHeaders projects the request-scoped Codex identity
// onto the legacy /responses/compact transport. Compact is not a Responses
// turn: the official client sends installation/session/thread compatibility
// headers and keeps prompt_cache_key in the body, but does not add
// x-client-request-id or client_metadata to the compact payload.
//
// Canonical hyphenated headers are preferred. Legacy aliases are only used as
// a fallback so existing tenant-isolation behavior remains intact for older
// clients that do not send the Codex headers.
func applyStagedCodexCompactHeaders(c *gin.Context, account *Account, h http.Header, body []byte) {
	if h == nil || account == nil || account.Type != AccountTypeOAuth {
		return
	}

	var source http.Header
	if c != nil && c.Request != nil {
		source = c.Request.Header
	}

	var ids *codexFingerprintIDs
	if c != nil {
		if value, ok := c.Get(codexFingerprintIDsContextKey); ok {
			ids, _ = value.(*codexFingerprintIDs)
		}
	}
	// Compact projection is opt-in just like the normal fingerprint path. In
	// off mode, leave the client's headers untouched rather than manufacturing
	// canonical aliases on an otherwise generic request.
	if ids == nil || !codexFingerprintIDsBelongToAccount(ids, account) {
		return
	}
	if codexFingerprintProjectionMalformed(source, body) ||
		codexTurnMetadataMalformed(h.Get("x-codex-turn-metadata")) {
		return
	}

	// Standard Codex session/thread headers are authoritative. If a legacy
	// client omitted them, retain the already-isolated alias emitted by the
	// normal compact builder and establish the root relation when possible.
	sessionID := firstHeaderValue(source, "session-id", "session_id", "conversation_id")
	threadID := firstHeaderValue(source, "thread-id", "thread_id")
	if sessionID == "" {
		sessionID = firstBodyString(body, "client_metadata.session_id")
	}
	if threadID == "" {
		threadID = firstBodyString(body, "client_metadata.thread_id")
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(h.Get("session_id"))
	}
	if threadID == "" {
		threadID = strings.TrimSpace(h.Get("thread-id"))
	}
	if threadID == "" {
		threadID = sessionID
	}
	// In device mode the client owns session/thread/window state. When a
	// compact request has no explicit session header, Codex's default cache
	// domain is the prompt_cache_key, so use that value before falling back to
	// the already-built legacy alias. This keeps the two projections aligned.
	if codexFingerprintRuntimeMode(ids.mode) == codexFingerprintDevice {
		if explicitSession := firstHeaderValue(source, "session-id", "session_id", "conversation_id"); explicitSession == "" {
			if cacheKey := firstBodyString(body, "prompt_cache_key"); cacheKey != "" {
				sessionID = cacheKey
				if firstHeaderValue(source, "thread-id", "thread_id") == "" {
					threadID = cacheKey
				}
			}
		}
	}
	if sessionID != "" {
		h.Set("session-id", sessionID)
		h.Set("session_id", sessionID)
	}
	if threadID != "" {
		h.Set("thread-id", threadID)
		h.Set("thread_id", threadID)
	}

	// Compact compatibility headers are projections of the same metadata
	// snapshot. Do not overwrite a value already rewritten by the staged
	// fingerprint helper; copy from the client only when the builder has none.
	copyCompactHeaderIfEmpty(h, source, "x-codex-window-id")
	copyCompactHeaderIfEmpty(h, source, "x-codex-turn-metadata")
	copyCompactHeaderIfEmpty(h, source, "x-codex-parent-thread-id")
	copyCompactHeaderIfEmpty(h, source, "x-openai-subagent")
	copyCompactHeaderFromBodyIfEmpty(h, body, "x-codex-window-id", "client_metadata.x-codex-window-id")
	copyCompactHeaderFromBodyIfEmpty(h, body, "x-codex-turn-metadata", "client_metadata.x-codex-turn-metadata")
	copyCompactHeaderFromBodyIfEmpty(h, body, "x-codex-parent-thread-id", "client_metadata.x-codex-parent-thread-id")
	copyCompactHeaderFromBodyIfEmpty(h, body, "x-openai-subagent", "client_metadata.x-openai-subagent")

	if strings.TrimSpace(ids.installationID) != "" {
		h.Set("x-codex-installation-id", ids.installationID)
		// Device mode owns only installation_id. Keep the rest of the compact
		// metadata untouched and, when present, align its installation field.
		if codexFingerprintRuntimeMode(ids.mode) == codexFingerprintDevice {
			rewriteCodexTurnMetadataFields(h, map[string]any{
				"installation_id": ids.installationID,
			})
		}
	}

}

// applyCodexFingerprintCountTokensHeaders keeps the native input_tokens
// endpoint compatible with Codex's device projection. Unlike normal
// Responses/WS requests, this endpoint has no client_metadata envelope on the
// wire, so it still needs the explicit installation header. Session/thread
// aliases remain intentionally absent in device mode.
func applyCodexFingerprintCountTokensHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil || strings.TrimSpace(ids.installationID) == "" {
		return
	}
	h.Set("x-codex-installation-id", ids.installationID)
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

func copyCompactHeaderIfEmpty(dst, src http.Header, name string) {
	if strings.TrimSpace(dst.Get(name)) != "" {
		return
	}
	if value := strings.TrimSpace(src.Get(name)); value != "" {
		dst.Set(name, value)
	}
}

func copyCompactHeaderFromBodyIfEmpty(dst http.Header, body []byte, name, path string) {
	if strings.TrimSpace(dst.Get(name)) != "" {
		return
	}
	if value := firstBodyString(body, path); value != "" {
		dst.Set(name, value)
	}
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

// codexFingerprintRuntimeMode validates a mode supplied by an internal helper.
// Account configuration is normalized by GetCodexFingerprintMode below, so
// production requests never select the legacy stateless session/full modes;
// keeping them valid here preserves deterministic unit/helper behavior for
// migration and diagnostics.
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
	// An explicit device override is a complete installation identity. Without
	// it, require the persisted seed; missing/invalid seeds must fail closed to
	// the normal per-API-key isolation path instead of leaking raw aliases.
	return strings.TrimSpace(account.GetOpenAIDeviceID()) != "" || account.getCodexFingerprintSeed() != ""
}

// normalizeCodexFingerprintModeForStorage migrates the historical session and
// full values when an account is written through an older API client. Existing
// backups remain readable, while new writes cannot reintroduce an unsupported
// stateless identity graph.
func normalizeCodexFingerprintModeForStorage(extra map[string]any) map[string]any {
	if extra == nil {
		return extra
	}
	raw, ok := extra[codexFingerprintModeExtraKey].(string)
	if !ok {
		return extra
	}
	mode := codexFingerprintMode(strings.ToLower(strings.TrimSpace(raw)))
	var canonical codexFingerprintMode
	switch mode {
	case codexFingerprintSession, codexFingerprintFull:
		// The old stateless modes cannot reproduce Codex's stateful
		// session/thread lifecycle. Persist the protocol-compatible device
		// projection when an older client still sends either value.
		canonical = codexFingerprintDevice
	case codexFingerprintOff, codexFingerprintDevice:
		canonical = mode
	default:
		// Validation is performed at the API boundary. Preserve an invalid
		// value here so callers that only read legacy data still see it and
		// can report the original validation error.
		return extra
	}
	if raw == string(canonical) {
		return extra
	}
	out := make(map[string]any, len(extra))
	for key, value := range extra {
		out[key] = value
	}
	out[codexFingerprintModeExtraKey] = string(canonical)
	return out
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

func codexFingerprintSeed(account *Account) string {
	if account == nil {
		return ""
	}
	if seed := account.getCodexFingerprintSeed(); seed != "" {
		return "extra:" + seed
	}
	return ""
}

func codexFingerprintDerivationSeed(account *Account, purpose, deploymentSeed string, extra ...string) string {
	identity := codexFingerprintSeed(account)
	if identity == "" {
		return ""
	}
	if len(extra) > 0 {
		identity += "\x00" + strings.Join(extra, "\x00")
	}
	label := "sub2api:codex-fingerprint:v2:" + purpose + "\x00" + identity
	// Keep the variadic deploymentSeed parameter for old call sites, but do not
	// mix it into persisted identities (see codexFingerprintDeploymentSeed).
	_ = deploymentSeed
	return label
}

func (a *Account) getCodexFingerprintSeed() string {
	if a == nil || !a.IsOpenAIOAuth() {
		return ""
	}
	seed := strings.TrimSpace(a.GetExtraString(codexFingerprintSeedExtraKey))
	if !isCodexFingerprintSeed(seed) {
		return ""
	}
	return seed
}

func isCodexFingerprintSeed(seed string) bool {
	id, err := uuid.Parse(strings.TrimSpace(seed))
	return err == nil && id.Version() == uuid.Version(4) && id.Variant() == uuid.RFC4122
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

// ValidateCodexFingerprintExtra rejects identity settings on channels that do
// not own an OpenAI OAuth installation. Legacy session/full values remain
// accepted for import compatibility and are normalized to device on write.
func ValidateCodexFingerprintExtra(platform, accountType string, extra map[string]any) error {
	if extra == nil {
		return nil
	}
	rawMode, modePresent := extra[codexFingerprintModeExtraKey]
	if modePresent && rawMode != nil {
		mode, ok := rawMode.(string)
		mode = strings.ToLower(strings.TrimSpace(mode))
		if !ok || (mode != string(codexFingerprintOff) && mode != string(codexFingerprintDevice) &&
			mode != string(codexFingerprintSession) && mode != string(codexFingerprintFull)) {
			return fmt.Errorf("codex_fingerprint_mode must be one of off, device, session, or full")
		}
		if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
			return fmt.Errorf("codex_fingerprint_mode only applies to OpenAI OAuth accounts")
		}
	}
	if rawSeed, seedPresent := extra[codexFingerprintSeedExtraKey]; seedPresent && rawSeed != nil {
		seed, ok := rawSeed.(string)
		if !ok || !isCodexFingerprintSeed(seed) {
			return fmt.Errorf("codex_fingerprint_seed must be an RFC4122 UUIDv4")
		}
		if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
			return fmt.Errorf("codex_fingerprint_seed only applies to OpenAI OAuth accounts")
		}
	}
	return nil
}

// ensureCodexFingerprintSeed adds an opaque random seed only when convergence
// is explicitly enabled. Existing extra values are preserved and callers
// receive their original map when no change is needed.
func ensureCodexFingerprintSeed(platform, accountType string, extra map[string]any) map[string]any {
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
		return extra
	}
	if !codexFingerprintModeEnabledExtra(extra) {
		return extra
	}
	if seed := strings.TrimSpace(extraStringValue(extra, codexFingerprintSeedExtraKey)); seed != "" {
		if isCodexFingerprintSeed(seed) {
			return extra
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

// GetCodexFingerprintMode 从账号 extra JSON 读取指纹收敛模式。
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
	case codexFingerprintOff, codexFingerprintDevice:
		return codexFingerprintMode(raw)
	case codexFingerprintSession, codexFingerprintFull:
		// Legacy values are read as the only stateless projection that matches
		// the official client: stable installation, real client session/thread.
		return codexFingerprintDevice
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
func resolveConvergedInstallationID(account *Account, deploymentSeed ...string) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" {
		return deviceID
	}
	// installation_id is itself the persisted identity. Hashing it again
	// changes the client-visible value and diverges from the official Codex
	// seed persistence contract.
	return account.getCodexFingerprintSeed()
}

// resolveConvergedSessionID returns the account-wide session used only by full
// convergence mode.
func resolveConvergedSessionID(account *Account, deploymentSeed ...string) string {
	if account == nil {
		return ""
	}
	seed := codexFingerprintDerivationSeed(account, "session", firstCodexFingerprintSeed(deploymentSeed))
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4(seed)
}

// resolveCodexConversationSessionID derives a stable upstream session for one
// client conversation while keeping different conversations isolated.
func resolveCodexConversationSessionID(account *Account, clientSessionSeed string, deploymentSeed ...string) string {
	if account == nil {
		return ""
	}
	clientSessionSeed = strings.TrimSpace(clientSessionSeed)
	if clientSessionSeed == "" {
		clientSessionSeed = "default"
	}
	seed := codexFingerprintDerivationSeed(account, "conversation-session", firstCodexFingerprintSeed(deploymentSeed), clientSessionSeed)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4(seed)
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(account *Account, clientSessionID string, deploymentSeed ...string) string {
	if account == nil || clientSessionID == "" {
		return ""
	}
	seed := codexFingerprintDerivationSeed(account, "thread", firstCodexFingerprintSeed(deploymentSeed), clientSessionID)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4(seed)
}

func firstCodexFingerprintSeed(seeds []string) string {
	if len(seeds) == 0 {
		return ""
	}
	return seeds[0]
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
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的 session-id 头值（连字符形式），用于 session 模式下
// 的 thread_id 派生——每个真实 Codex 会话得到一个独立线程。
// 返回 nil 表示 off 模式，不需要改写。
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode, deploymentSeed ...string) *codexFingerprintIDs {
	mode = codexFingerprintRuntimeMode(mode)
	if mode == codexFingerprintOff {
		return nil
	}

	ids := &codexFingerprintIDs{accountID: account.ID, mode: mode}

	ids.installationID = resolveConvergedInstallationID(account, firstCodexFingerprintSeed(deploymentSeed))
	if ids.installationID == "" {
		return nil
	}

	switch mode {
	case codexFingerprintDevice:
		return ids

	case codexFingerprintSession:
		seed := firstCodexFingerprintSeed(deploymentSeed)
		ids.sessionID = resolveCodexConversationSessionID(account, clientSessionID, seed)
		ids.threadID = resolveConvergedThreadID(account, clientSessionID, seed)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		ids.turnStartedAtUnixMS = time.Now().UnixMilli()
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(account, firstCodexFingerprintSeed(deploymentSeed))
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
	if ids.accountID != account.ID || codexFingerprintRuntimeMode(ids.mode) != account.GetCodexFingerprintMode() {
		return false
	}
	// Account configuration can change while an in-flight attempt is being
	// failed over. Bind the staged snapshot to the current installation too, so
	// a mode/seed rotation cannot reuse a half-old identity tuple.
	expectedInstallation := strings.TrimSpace(resolveConvergedInstallationID(account))
	return expectedInstallation != "" && expectedInstallation == strings.TrimSpace(ids.installationID)
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
	if v := strings.TrimSpace(h.Get("conversation_id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(h.Get("thread-id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(h.Get("x-codex-window-id")); v != "" {
		return strings.TrimSuffix(v, ":0")
	}
	return ""
}

// resolveCodexConversationSeed prefers explicit client identity, then stable
// body metadata/cache identity, and finally a content-derived conversation
// anchor. apiKeyID prevents identical prompts from different downstream users
// sharing an upstream session.
func resolveCodexConversationSeed(clientHeaders http.Header, body []byte, apiKeyID int64) string {
	if clientHeaders != nil {
		if explicit := extractClientSessionID(clientHeaders); explicit != "" {
			return fmt.Sprintf("apikey:%d:header:%s", apiKeyID, explicit)
		}
	}
	for _, path := range []string{
		"client_metadata.session_id",
		"client_metadata.thread_id",
		"prompt_cache_key",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return fmt.Sprintf("apikey:%d:body:%s", apiKeyID, value)
		}
	}
	if contentSeed := deriveOpenAIAnchoredContentSessionSeed(body); contentSeed != "" {
		return fmt.Sprintf("apikey:%d:content:%s", apiKeyID, contentSeed)
	}
	return fmt.Sprintf("apikey:%d:default", apiKeyID)
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
	return resolveCodexFingerprintIDs(account, clientSessionID, mode, deploymentSeed...)
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
	return resolveCodexFingerprintIDs(account, clientSessionID, mode, deploymentSeed...)
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 的白名单透传之后、enforceCodexIdentityHeaders 之前调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}
	// Regular Responses and Responses-WebSocket requests carry the converged
	// installation identity in body `client_metadata`. The direct
	// x-codex-installation-id header belongs only to legacy /responses/compact.
	// Remove a client/stale projection before any other rewrite.
	h.Del("x-codex-installation-id")
	if codexTurnMetadataMalformed(h.Get("x-codex-turn-metadata")) {
		// Do not partially rewrite a request whose embedded metadata cannot be
		// parsed. Keeping the original identity tuple is safer than mixing a
		// converged installation with stale turn fields.
		return
	}

	// 所有非 off 模式都收敛 installation_id
	// The regular HTTP/WS path deliberately does not emit the direct
	// x-codex-installation-id header; compact adds it in its dedicated helper.
	// A request ID identifies one upstream attempt, not a conversation. Keeping
	// it stable across a session makes unrelated turns look like replays.

	if ids.mode == codexFingerprintDevice {
		rewriteCodexTurnMetadataFields(h, map[string]any{
			"installation_id": ids.installationID,
		})
		return
	}

	// session / full 模式：改写所有相关头
	// Defensive guard for snapshots produced by pre-device-only callers. Do not
	// emit a second stateless session graph after the runtime gate above.
	if ids.mode != codexFingerprintSession && ids.mode != codexFingerprintFull {
		return
	}
	h.Set("x-codex-window-id", ids.windowID)
	// Codex ResponsesClient uses the thread id as the request identity.
	h.Set("x-client-request-id", ids.threadID)
	// 连字符形式和下划线形式都改写，保证一致
	h.Set("session-id", ids.sessionID)
	h.Set("session_id", ids.sessionID)
	h.Set("conversation_id", ids.sessionID)
	h.Set("thread-id", ids.threadID)

	rewriteCodexTurnMetadataFields(h, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	})
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
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
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

// applyCodexFingerprintToClientMetadataMap 是 client_metadata 改写的共享核心，
// map 版（非透传，body 已解码）与 raw 字节版（透传热路径）都经由它，保证两条
// 路径的收敛语义永不漂移。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *codexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}
	if raw, ok := existing["x-codex-turn-metadata"].(string); ok && codexTurnMetadataMalformed(raw) {
		return false
	}
	mode := codexFingerprintRuntimeMode(ids.mode)
	if mode == codexFingerprintOff {
		return false
	}

	modified := false

	if ids.installationID != "" {
		existing["x-codex-installation-id"] = ids.installationID
		modified = true
	}

	if mode == codexFingerprintDevice {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id": ids.installationID,
		})
		return modified
	}

	// session / full 模式
	existing["session_id"] = ids.sessionID
	existing["thread_id"] = ids.threadID
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID

	rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	})
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
	raw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	if !ok || raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
	}
	for k, v := range fields {
		metadata[k] = v
	}
	if rebuilt, err := json.Marshal(metadata); err == nil {
		clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
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

func codexBodyTurnMetadataMalformed(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return codexTurnMetadataMalformed(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
}

func codexFingerprintProjectionMalformed(headers http.Header, body []byte) bool {
	return (headers != nil && codexTurnMetadataMalformed(headers.Get("x-codex-turn-metadata"))) ||
		codexBodyTurnMetadataMalformed(body)
}
