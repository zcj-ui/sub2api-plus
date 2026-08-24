package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *OpenAIGatewayService) isOpenAIWSGeneratePrewarmEnabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled
}

// performOpenAIWSGeneratePrewarm 在 WSv2 下执行可选的 generate=false 预热。
// 预热默认关闭，仅在配置开启后生效；失败时按可恢复错误回退到 HTTP。
func (s *OpenAIGatewayService) performOpenAIWSGeneratePrewarm(
	ctx context.Context,
	lease *openAIWSConnLease,
	decision OpenAIWSProtocolDecision,
	payload map[string]any,
	previousResponseID string,
	reqBody map[string]any,
	account *Account,
	stateStore OpenAIWSStateStore,
	groupID int64,
) error {
	if s == nil {
		return nil
	}
	if lease == nil || account == nil {
		logOpenAIWSModeInfo("prewarm_skip reason=invalid_state has_lease=%v has_account=%v", lease != nil, account != nil)
		return nil
	}
	connID := strings.TrimSpace(lease.ConnID())
	if !s.isOpenAIWSGeneratePrewarmEnabled() {
		return nil
	}
	// A confirmed 429 already has an explicit client/session continuation
	// path. Do not issue a hidden generate=false probe on that account: the
	// probe has no client-visible response binding and therefore cannot safely
	// become a new guarded socket.
	if s.isOpenAIWS429GuardConnectionActive(account) {
		logOpenAIWSModeInfo("prewarm_skip account_id=%d conn_id=%s reason=429_guard_active", account.ID, connID)
		return nil
	}
	if decision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		logOpenAIWSModeInfo(
			"prewarm_skip account_id=%d conn_id=%s reason=transport_not_v2 transport=%s",
			account.ID,
			connID,
			normalizeOpenAIWSLogValue(string(decision.Transport)),
		)
		return nil
	}
	if strings.TrimSpace(previousResponseID) != "" {
		logOpenAIWSModeInfo(
			"prewarm_skip account_id=%d conn_id=%s reason=has_previous_response_id previous_response_id=%s",
			account.ID,
			connID,
			truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
		)
		return nil
	}
	if lease.IsPrewarmed() {
		logOpenAIWSModeInfo("prewarm_skip account_id=%d conn_id=%s reason=already_prewarmed", account.ID, connID)
		return nil
	}
	if NeedsToolContinuation(reqBody) {
		logOpenAIWSModeInfo("prewarm_skip account_id=%d conn_id=%s reason=tool_continuation", account.ID, connID)
		return nil
	}
	prewarmStart := time.Now()
	logOpenAIWSModeInfo("prewarm_start account_id=%d conn_id=%s", account.ID, connID)

	prewarmPayload := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		prewarmPayload[k] = v
	}
	prewarmPayload["generate"] = false
	// Prewarm is a real response.create write. Refresh its transport timestamp
	// independently so the later business request receives its own stamp.
	normalizeOpenAIWSResponseCreatePayload(prewarmPayload, openAIWSResponseCreateProtocolOptions{})
	prewarmPayloadJSON := payloadAsJSONBytes(prewarmPayload)

	if err := lease.WriteJSONWithContextTimeout(ctx, prewarmPayload, s.openAIWSWriteTimeout()); err != nil {
		lease.MarkBroken()
		logOpenAIWSModeInfo(
			"prewarm_write_fail account_id=%d conn_id=%s cause=%s",
			account.ID,
			connID,
			truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen),
		)
		return wrapOpenAIWSFallback("prewarm_write", err)
	}
	logOpenAIWSModeInfo("prewarm_write_sent account_id=%d conn_id=%s payload_bytes=%d", account.ID, connID, len(prewarmPayloadJSON))

	prewarmResponseID := ""
	prewarmEventCount := 0
	prewarmTerminalCount := 0
	rateLimitSignalHandled := false
	recordPrewarmRateLimit := func(upstreamStatus int, codeRaw, errTypeRaw, errMsgRaw string, responseBody []byte) bool {
		isRateLimit := isOpenAIWSRateLimitSignal(upstreamStatus, codeRaw, errTypeRaw, errMsgRaw)
		explicit429 := isOpenAIWSExplicit429Signal(upstreamStatus, codeRaw, errTypeRaw, errMsgRaw, responseBody)
		if !isRateLimit || rateLimitSignalHandled {
			return isRateLimit
		}
		if upstreamStatus == http.StatusTooManyRequests && !isOpenAIWSRateLimitError(codeRaw, errTypeRaw, errMsgRaw) {
			s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, lease.HandshakeHeaders(), responseBody)
		} else {
			s.persistOpenAIWSRateLimitSignal(ctx, account, lease.HandshakeHeaders(), responseBody, codeRaw, errTypeRaw, errMsgRaw, upstreamStatus)
		}
		rateLimitSignalHandled = true
		// A prewarm 429 can be the second explicit signal that confirms the
		// already-acquired socket. A lease observed during the transition is
		// eligible only when the exact socket was already pooled before it;
		// fresh post-block sockets remain failover-only.
		if explicit429 && (!lease.openAI429GuardActiveAtAcquire || s.isOpenAIWS429GuardConnectionCandidate(account, lease.ConnID(), lease.openAIRuntimeBlockGeneration)) {
			s.markOpenAI429GuardConnectionProof(account, lease)
			if s.isOpenAIWS429GuardConnectionPinned(account, lease.ConnID()) {
				lease.openAI429GuardProven.Store(true)
			}
		}
		return true
	}
	prewarmFailover := func(status int, message []byte, errMsg string) error {
		if status <= 0 {
			status = http.StatusBadGateway
		}
		// A confirmed 429 is semantic account state. Once this exact prewarm
		// lease has been positively pinned, retain it for the next continuation;
		// all first-signal and transport failures still evict normally.
		if status != http.StatusTooManyRequests || !s.isOpenAIWS429GuardConnectionPinned(account, lease.ConnID()) {
			lease.MarkBroken()
		}
		failoverErr := newOpenAIUpstreamFailoverError(
			status,
			lease.HandshakeHeaders(),
			append([]byte(nil), message...),
			errMsg,
			false,
		)
		if status == http.StatusTooManyRequests && s.isOpenAIWS429GuardConnectionPinned(account, lease.ConnID()) {
			return wrapOpenAIWSFallbackKeepConnection("prewarm_upstream_rate_limited", failoverErr)
		}
		return failoverErr
	}
	prewarmUpstreamFailureStatus := func(message []byte) int {
		// A prewarm response is never client-visible, so every explicit 5xx
		// means the socket cannot satisfy the next turn. Preserve the exact
		// status when present; fall back to the semantic server-error mapper.
		if status := openAIWSPayloadUpstreamStatus(message); status >= http.StatusInternalServerError && status <= 599 {
			return status
		}
		return openAIWSPayloadTransientStatus(message)
	}
	for {
		message, readErr := lease.ReadMessageWithContextTimeout(ctx, s.openAIWSReadTimeout())
		if readErr != nil {
			lease.MarkBroken()
			closeStatus, closeReason := summarizeOpenAIWSReadCloseError(readErr)
			logOpenAIWSModeInfo(
				"prewarm_read_fail account_id=%d conn_id=%s close_status=%s close_reason=%s cause=%s events=%d",
				account.ID,
				connID,
				closeStatus,
				closeReason,
				truncateOpenAIWSLogValue(readErr.Error(), openAIWSLogValueMaxLen),
				prewarmEventCount,
			)
			return wrapOpenAIWSFallback("prewarm_"+classifyOpenAIWSReadFallbackReason(readErr), readErr)
		}

		eventType, eventResponseID, _ := parseOpenAIWSEventEnvelope(message)
		if eventType == "" {
			continue
		}
		prewarmEventCount++
		if prewarmResponseID == "" && eventResponseID != "" {
			prewarmResponseID = eventResponseID
		}
		if prewarmEventCount <= openAIWSPrewarmEventLogHead || eventType == "error" || isOpenAIWSTerminalEvent(eventType) {
			logOpenAIWSModeInfo(
				"prewarm_event account_id=%d conn_id=%s idx=%d type=%s bytes=%d",
				account.ID,
				connID,
				prewarmEventCount,
				truncateOpenAIWSLogValue(eventType, openAIWSLogValueMaxLen),
				len(message),
			)
		}

		if eventType == "error" {
			errCodeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(message)
			upstreamStatus := openAIWSPayloadUpstreamStatus(message)
			isRateLimit := recordPrewarmRateLimit(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw, message)
			errMsg := strings.TrimSpace(errMsgRaw)
			if errMsg == "" {
				errMsg = "OpenAI websocket prewarm error"
			}
			fallbackReason, canFallback := classifyOpenAIWSErrorEventFromRaw(errCodeRaw, errTypeRaw, errMsgRaw)
			if isRateLimit {
				fallbackReason = "upstream_rate_limited"
				canFallback = true
			}
			errCode, errType, errMessage := summarizeOpenAIWSErrorEventFieldsFromRaw(errCodeRaw, errTypeRaw, errMsgRaw)
			logOpenAIWSModeInfo(
				"prewarm_error_event account_id=%d conn_id=%s idx=%d fallback_reason=%s can_fallback=%v err_code=%s err_type=%s err_message=%s",
				account.ID,
				connID,
				prewarmEventCount,
				truncateOpenAIWSLogValue(fallbackReason, openAIWSLogValueMaxLen),
				canFallback,
				errCode,
				errType,
				errMessage,
			)
			if isRateLimit {
				return prewarmFailover(http.StatusTooManyRequests, message, errMsg)
			}
			if transientStatus := prewarmUpstreamFailureStatus(message); transientStatus >= http.StatusInternalServerError {
				s.handleOpenAIAccountUpstreamError(ctx, account, transientStatus, lease.HandshakeHeaders(), message)
				return prewarmFailover(transientStatus, message, errMsg)
			}
			lease.MarkBroken()
			if canFallback {
				return wrapOpenAIWSFallback("prewarm_"+fallbackReason, errors.New(errMsg))
			}
			return wrapOpenAIWSFallback("prewarm_error_event", errors.New(errMsg))
		}

		if eventType == "response.failed" {
			errCodeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(message)
			upstreamStatus := openAIWSPayloadUpstreamStatus(message)
			isRateLimit := recordPrewarmRateLimit(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw, message)
			errMsg := strings.TrimSpace(errMsgRaw)
			if errMsg == "" {
				errMsg = "OpenAI websocket prewarm response failed"
			}
			if isRateLimit {
				return prewarmFailover(http.StatusTooManyRequests, message, errMsg)
			}
			if transientStatus := prewarmUpstreamFailureStatus(message); transientStatus >= http.StatusInternalServerError {
				s.handleOpenAIAccountUpstreamError(ctx, account, transientStatus, lease.HandshakeHeaders(), message)
				return prewarmFailover(transientStatus, message, errMsg)
			}
			lease.MarkBroken()
			return wrapOpenAIWSFallback("prewarm_response_failed", errors.New(errMsg))
		}

		if isOpenAIWSTerminalEvent(eventType) {
			prewarmTerminalCount++
			break
		}
	}

	lease.MarkPrewarmed()
	if prewarmResponseID != "" && stateStore != nil {
		ttl := s.openAIWSResponseStickyTTL()
		logOpenAIWSBindResponseAccountWarn(groupID, account.ID, prewarmResponseID, stateStore.BindResponseAccount(ctx, groupID, prewarmResponseID, account.ID, ttl))
		stateStore.BindResponseConn(prewarmResponseID, lease.ConnID(), ttl)
	}
	logOpenAIWSModeInfo(
		"prewarm_done account_id=%d conn_id=%s response_id=%s events=%d terminal_events=%d duration_ms=%d",
		account.ID,
		connID,
		truncateOpenAIWSLogValue(prewarmResponseID, openAIWSIDValueMaxLen),
		prewarmEventCount,
		prewarmTerminalCount,
		time.Since(prewarmStart).Milliseconds(),
	)
	return nil
}

func payloadAsJSON(payload map[string]any) string {
	return string(payloadAsJSONBytes(payload))
}

func payloadAsJSONBytes(payload map[string]any) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return body
}

// openAIWSEmptyErrorClientMessage is injected into upstream `error` events that
// carry no code/type/message before they are forwarded to a client. The phrase
// "You can retry your request" mirrors OpenAI's own transient-failure wording so
// downstream retry classifiers treat the failure as retryable.
const openAIWSEmptyErrorClientMessage = "Upstream provider returned an error without details. You can retry your request."

// ensureOpenAIWSErrorEventClientDetail rewrites an upstream `error` event whose
// error object has no code/type/message before forwarding it to a client.
// Strict SDK clients (e.g. openai-node) stringify the empty error object into
// the literal message "{}", which no downstream retry classifier can recognize,
// so a transient upstream failure terminates the client session instead of
// being retried. Like sanitizeOpenAICapacityShedErrorCodeForClient, this only
// changes the client-facing copy: monitoring, account-state and failover
// decisions all run on the original payload.
func ensureOpenAIWSErrorEventClientDetail(message []byte) ([]byte, bool) {
	code, errType, errMsg := parseOpenAIWSErrorEventFields(message)
	if code != "" || errType != "" || errMsg != "" {
		return message, false
	}
	if !gjson.ValidBytes(message) {
		return message, false
	}
	updated, err := sjson.SetBytes(message, "error", map[string]any{
		"code":    openAICapacityShedRetryableClientCode,
		"type":    "server_error",
		"message": openAIWSEmptyErrorClientMessage,
	})
	if err != nil {
		return message, false
	}
	return updated, true
}

func isOpenAIWSTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func normalizeOpenAIWSTerminalEvent(eventType string) string {
	switch strings.TrimSpace(eventType) {
	case "response.completed":
		return "response.completed"
	case "response.done":
		return "response.done"
	case "response.failed":
		return "response.failed"
	case "response.incomplete":
		return "response.incomplete"
	case "response.cancelled", "response.canceled":
		return "response.cancelled"
	default:
		return ""
	}
}

	// markOpenAIWSClientVisibleFailure records only terminal/error protocol events
	// that were delivered to the client. Callers invoke it only after any hidden
	// failover/recovery decision and a successful downstream write.
	func markOpenAIWSClientVisibleFailure(c *gin.Context, eventType string, payload []byte) {
		eventType = strings.TrimSpace(eventType)
		if eventType != "error" && eventType != "response.failed" {
			return
		}
		prefix := "error"
		if eventType == "response.failed" {
			prefix = "response.error"
		}
		code := strings.TrimSpace(gjson.GetBytes(payload, prefix+".code").String())
		errType := strings.TrimSpace(gjson.GetBytes(payload, prefix+".type").String())
		message := strings.TrimSpace(gjson.GetBytes(payload, prefix+".message").String())
		if eventType == "response.failed" && code == "" && errType == "" && message == "" {
			prefix = "error"
			code = strings.TrimSpace(gjson.GetBytes(payload, prefix+".code").String())
			errType = strings.TrimSpace(gjson.GetBytes(payload, prefix+".type").String())
			message = strings.TrimSpace(gjson.GetBytes(payload, prefix+".message").String())
		}
		status := int(gjson.GetBytes(payload, prefix+".status_code").Int())
		if status == 0 {
			status = int(gjson.GetBytes(payload, prefix+".status").Int())
		}
		if status == 0 && eventType == "error" {
			status = int(gjson.GetBytes(payload, "status").Int())
		}
		if status == 0 {
			status = openAIWSErrorHTTPStatusFromRaw(code, errType)
		}
		if errType == "" {
			errType = "upstream_error"
		}
		if code == "" {
			code = strings.ReplaceAll(eventType, ".", "_")
		}
		if message == "" {
			message = "upstream websocket request failed"
		}
		MarkOpsStreamFailure(c, errType, code, message, status)
	}

	func openAIWSPayloadUpstreamStatus(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	status := int(gjson.GetBytes(payload, "response.error.status_code").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "response.error.status").Int())
	}
	if status == 0 {
		status = int(gjson.GetBytes(payload, "error.status_code").Int())
	}
	if status == 0 {
		status = int(gjson.GetBytes(payload, "error.status").Int())
	}
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status").Int())
	}
	return status
}

func openAIWSPayloadTransientStatus(payload []byte) int {
	status := openAIWSPayloadUpstreamStatus(payload)
	if shouldCooldownOpenAITransientUpstreamError(status, payload) {
		return status
	}
	if status != 0 {
		return 0
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	switch {
	case code == "server_is_overloaded", code == "slow_down":
		return http.StatusServiceUnavailable
	case strings.Contains(code, "server_error"),
		strings.Contains(code, "internal_error"),
		strings.Contains(code, "upstream_error"),
		strings.Contains(errType, "server_error"),
		strings.Contains(errType, "internal_error"),
		strings.Contains(errType, "upstream_error"):
		return http.StatusInternalServerError
	default:
		return 0
	}
}

func (s *OpenAIGatewayService) handleOpenAIWSTerminalTransientFailure(ctx context.Context, account *Account, canonicalModel string, headers http.Header, payload []byte) string {
	eventType, _, _ := parseOpenAIWSEventEnvelope(payload)
	terminalEvent := normalizeOpenAIWSTerminalEvent(eventType)
	if terminalEvent != "response.failed" {
		return terminalEvent
	}
	s.handleOpenAIWSFailureAccountSideEffects(ctx, account, canonicalModel, headers, payload)
	return terminalEvent
}

func (s *OpenAIGatewayService) handleOpenAIWSErrorEventTransientFailure(ctx context.Context, account *Account, canonicalModel string, headers http.Header, payload []byte) {
	eventType, _, _ := parseOpenAIWSEventEnvelope(payload)
	if eventType != "error" {
		return
	}
	status := openAIWSPayloadTransientStatus(payload)
	if status != 0 {
		s.handleOpenAIAccountUpstreamError(ctx, account, status, headers, payload, canonicalModel)
	}
}

// handleOpenAIWSFailureAccountSideEffects applies both structured credential
// failures and transient failures. Its return value lets stream callers avoid
// applying the same transition twice for an error/response.failed pair.
func (s *OpenAIGatewayService) handleOpenAIWSFailureAccountSideEffects(ctx context.Context, account *Account, canonicalModel string, headers http.Header, payload []byte) bool {
	message := extractOpenAISSEErrorMessage(payload)
	status := openAIStreamFailureStatus(payload, message)
	switch status {
	case http.StatusUnauthorized, http.StatusTooManyRequests, 529:
		s.handleOpenAIStreamTerminalAccountSideEffects(nil, account, payload, message, headers)
		return true
	case http.StatusForbidden:
		if !openAIStream403AccountFailure(payload, message) {
			return false
		}
		s.handleOpenAIStreamTerminalAccountSideEffects(nil, account, payload, message, headers)
		return true
	}

	status = openAIWSPayloadTransientStatus(payload)
	if status == 0 {
		return false
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, status, headers, payload, canonicalModel)
	return true
}

func (s *OpenAIGatewayService) handleOpenAIWSDialTransientFailure(ctx context.Context, account *Account, canonicalModel string, err error) {
	var dialErr *openAIWSDialError
	if !errors.As(err, &dialErr) || dialErr == nil || !shouldCooldownOpenAITransientUpstreamError(dialErr.StatusCode, dialErr.ResponseBody) {
		return
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, canonicalModel)
}

func isOpenAIWSTokenEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return false
	}
	switch eventType {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done":
		return false
	}
	if strings.Contains(eventType, ".delta") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output_text") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output") {
		return true
	}
	// 终止事件（response.completed/done/failed/...）由 isOpenAIWSTerminalEvent 单独处理。
	// 不能把它们当作 token event，否则当上游没有可识别的 delta 时，
	// firstTokenMs 会被填到终止时刻，等于把"总耗时"误报为"首 token 延迟"。
	return false
}

// openAIWSEventHasSemanticOutput excludes lifecycle/control frames from the
// replay guard. A created/in-progress frame may already be visible to the
// client, but it does not contain assistant or tool output that would be
// duplicated by a safe upstream retry.
func openAIWSEventHasSemanticOutput(eventType string, payload []byte) bool {
	eventType = strings.TrimSpace(eventType)
	if isOpenAIWSTokenEvent(eventType) {
		return true
	}
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		item := gjson.GetBytes(payload, "item")
		return item.Exists() && item.Type == gjson.JSON && strings.TrimSpace(item.Get("type").String()) != ""
	case "response.completed", "response.done":
		output := gjson.GetBytes(payload, "response.output")
		return output.IsArray() && len(output.Array()) > 0
	default:
		return false
	}
}

func replaceOpenAIWSMessageModel(message []byte, fromModel, toModel string) []byte {
	if len(message) == 0 {
		return message
	}
	if strings.TrimSpace(fromModel) == "" || strings.TrimSpace(toModel) == "" || fromModel == toModel {
		return message
	}
	if !bytes.Contains(message, []byte(`"model"`)) || !bytes.Contains(message, []byte(fromModel)) {
		return message
	}
	modelValues := gjson.GetManyBytes(message, "model", "response.model")
	replaceModel := modelValues[0].Exists() && modelValues[0].Str == fromModel
	replaceResponseModel := modelValues[1].Exists() && modelValues[1].Str == fromModel
	if !replaceModel && !replaceResponseModel {
		return message
	}
	updated := message
	if replaceModel {
		if next, err := sjson.SetBytes(updated, "model", toModel); err == nil {
			updated = next
		}
	}
	if replaceResponseModel {
		if next, err := sjson.SetBytes(updated, "response.model", toModel); err == nil {
			updated = next
		}
	}
	return updated
}

func populateOpenAIUsageFromResponseJSON(body []byte, usage *OpenAIUsage) {
	if usage == nil || len(body) == 0 {
		return
	}
	if parsed, ok := extractOpenAIUsageFromJSONBytes(body); ok {
		*usage = parsed
	}
}

func getOpenAIGroupIDFromContext(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	value, exists := c.Get("api_key")
	if !exists {
		return 0
	}
	apiKey, ok := value.(*APIKey)
	if !ok || apiKey == nil || apiKey.GroupID == nil {
		return 0
	}
	return *apiKey.GroupID
}

// SelectAccountByPreviousResponseID 按 previous_response_id 命中账号粘连。
// 未命中或账号不可用时返回 (nil, nil)，由调用方继续走常规调度。
func (s *OpenAIGatewayService) SelectAccountByPreviousResponseID(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
) (*AccountSelectionResult, error) {
	// 分组利润控制：公共入口装门，保证不经 selectAccountWithScheduler
	// 的调用方也无法绕过利润准入（scheduler 内部路径已在唯一调度入口装门）。
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	return s.selectAccountByPreviousResponseIDForCapability(ctx, groupID, previousResponseID, requestedModel, excludedIDs, "", requireCompact)
}

func (s *OpenAIGatewayService) selectAccountByPreviousResponseIDForCapability(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) (*AccountSelectionResult, error) {
	if s == nil {
		return nil, nil
	}
	accountID, account, responseID, store := s.resolveAccountByPreviousResponseIDForCapability(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	if accountID <= 0 || account == nil || store == nil {
		return nil, nil
	}

	result, acquireErr := s.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
	if acquireErr == nil && result.Acquired {
		logOpenAIWSBindResponseAccountWarn(
			derefGroupID(groupID),
			accountID,
			responseID,
			store.BindResponseAccount(ctx, derefGroupID(groupID), responseID, accountID, s.openAIWSResponseStickyTTL()),
		)
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account:     account,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}), nil
	}

	cfg := s.schedulingConfig()
	if s.concurrencyService != nil {
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account: account,
			WaitPlan: &AccountWaitPlan{
				AccountID:      accountID,
				MaxConcurrency: account.Concurrency,
				Timeout:        cfg.StickySessionWaitTimeout,
				MaxWaiting:     cfg.StickySessionMaxWaiting,
			},
		}), nil
	}
	return nil, nil
}

func (s *OpenAIGatewayService) ResolveAccountIDByPreviousResponseIDForScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) int64 {
	accountID, _, _, _ := s.resolveAccountByPreviousResponseIDForCapability(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	return accountID
}

func (s *OpenAIGatewayService) resolveAccountByPreviousResponseIDForCapability(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
) (int64, *Account, string, OpenAIWSStateStore) {
	if s == nil {
		return 0, nil, "", nil
	}
	responseID := strings.TrimSpace(previousResponseID)
	if responseID == "" {
		return 0, nil, "", nil
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return 0, nil, "", nil
	}

	accountID, err := store.GetResponseAccount(ctx, derefGroupID(groupID), responseID)
	if err != nil || accountID <= 0 {
		return 0, nil, "", nil
	}
	if excludedIDs != nil {
		if _, excluded := excludedIDs[accountID]; excluded {
			return 0, nil, "", nil
		}
	}

	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil {
		_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
		return 0, nil, "", nil
	}
	// OAuth/SetupToken continuation state lives on the WSv2 session and cannot
	// survive an HTTP fallback. Official API-key Responses HTTP requests are
	// different: previous_response_id is supported by the provider and scoped to
	// the selected key/project, so the response-id binding must retain that key.
	if !account.IsOpenAIApiKey() && s.getOpenAIWSProtocolResolver().Resolve(account).Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return 0, nil, "", nil
	}
	if shouldClearStickySession(account, requestedModel) || !account.IsOpenAI() || !account.IsSchedulable() {
		_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
		return 0, nil, "", nil
	}
	if !parentHealthyForShadow(account, s.parentAccountLookup(ctx)) {
		_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
		return 0, nil, "", nil
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return 0, nil, "", nil
	}
	if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
		return 0, nil, "", nil
	}
	// Runtime blocks must apply to every previous_response_id path, including
	// deployments without a scheduler snapshot. The 429 guard continuation is
	// resolved before this helper and is the sole exception: it requires the
	// exact permanently pinned socket. Letting ordinary response affinity skip
	// this check would revive accounts blocked for auth, transport, or admin
	// reasons and could also bypass a stale 429 guard binding.
	if s.isOpenAIAccountRequestRuntimeBlocked(account, requestedModel) {
		return 0, nil, "", nil
	}
	// Quota auto-pause must also gate the previous_response_id sticky path; otherwise an
	// account over its 5h/7d threshold keeps serving the same response chain even though
	// normal scheduling skips it. Pause is transient, so fall through to normal scheduling
	// without deleting the binding (the window may reset before the next turn).
	if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		return 0, nil, "", nil
	}
	// 分组利润控制：与 quota auto-pause 同语义——利润不合格是暂时
	// 状态（上游倍率/高峰随时间变化），只跳过本次复用、落回普通调度，不删除
	// 绑定（倍率恢复后可继续按 previous_response_id 粘连）。
	if vetoed, _ := openAIProfitControlVetoReason(ctx, account); vetoed {
		return 0, nil, "", nil
	}
	if s.schedulerSnapshot != nil && s.accountRepo != nil {
		latest, latestErr := s.accountRepo.GetByID(ctx, account.ID)
		if latestErr != nil || latest == nil {
			_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
			return 0, nil, "", nil
		}
		if shouldClearStickySession(latest, requestedModel) || !latest.IsOpenAI() || !latest.IsSchedulable() {
			_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
			return 0, nil, "", nil
		}
		if !s.openAIAccountMatchesSchedulingGroup(latest, groupID) {
			return 0, nil, "", nil
		}
		if s.openAIGroupRequiresPrivacySet(ctx, groupID) && !latest.IsPrivacySet() {
			return 0, nil, "", nil
		}
		if !parentHealthyForShadow(latest, s.parentAccountLookup(ctx)) {
			_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
			return 0, nil, "", nil
		}
		if requestedModel != "" && !latest.IsModelSupported(requestedModel) {
			return 0, nil, "", nil
		}
		if !latest.SupportsOpenAIEndpointCapability(requiredCapability) {
			return 0, nil, "", nil
		}
		if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, latest); paused {
			return 0, nil, "", nil
		}
		// 利润门对最新账号状态复检一次，语义同上：跳过复用、不删绑定。
		if vetoed, _ := openAIProfitControlVetoReason(ctx, latest); vetoed {
			return 0, nil, "", nil
		}
		if s.isOpenAIAccountRequestRuntimeBlocked(latest, requestedModel) {
			_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
			return 0, nil, "", nil
		}
		account = latest
	}
	if requireCompact && openAICompactSupportTier(account) == 0 {
		_ = store.DeleteResponseAccount(ctx, derefGroupID(groupID), responseID)
		return 0, nil, "", nil
	}
	return accountID, account, responseID, store
}

// clearOpenAIWSContinuationBindings removes only the bindings still owned by
// the failed account/connection. A resumed turn must not leave a stale
// response or session affinity that can route the next client reconnect back
// to the broken connection, while a concurrent replacement binding is kept.
func (s *OpenAIGatewayService) clearOpenAIWSContinuationBindings(
	ctx context.Context,
	groupID int64,
	sessionHash string,
	accountID int64,
	previousResponseID string,
	connID string,
) {
	if s == nil || accountID <= 0 {
		return
	}
	store := s.getOpenAIWSStateStore()
	if store != nil {
		conditionalCleaner, hasConditionalCleaner := store.(openAIWSContinuationBindingCleaner)
		responseID := strings.TrimSpace(previousResponseID)
		if responseID != "" {
			if hasConditionalCleaner {
				conditionalCleaner.deleteResponseBindingIfMatches(ctx, groupID, responseID, accountID, connID)
			} else {
				boundAccountID, err := store.GetResponseAccount(ctx, groupID, responseID)
				boundConnID, connExists := store.GetResponseConn(responseID)
				connMatches := !connExists || strings.TrimSpace(connID) == "" || boundConnID == strings.TrimSpace(connID)
				if err == nil && boundAccountID == accountID && connMatches {
					_ = store.DeleteResponseAccount(ctx, groupID, responseID)
					if !connExists || strings.TrimSpace(connID) == "" || boundConnID == strings.TrimSpace(connID) {
						store.DeleteResponseConn(responseID)
					}
				}
			}
		}

		hash := strings.TrimSpace(sessionHash)
		if hash != "" {
			matchedSessionConn := false
			if hasConditionalCleaner {
				matchedSessionConn = conditionalCleaner.deleteSessionConnIfMatches(groupID, hash, connID)
			} else {
				boundConnID, exists := store.GetSessionConn(groupID, hash)
				matchedSessionConn = !exists || strings.TrimSpace(connID) == "" || boundConnID == strings.TrimSpace(connID)
				if matchedSessionConn {
					store.DeleteSessionConn(groupID, hash)
				}
			}
			if matchedSessionConn {
				store.DeleteSessionTurnState(groupID, hash)
				if stickyAccountID, err := s.getStickySessionAccountID(ctx, &groupID, hash); err == nil && stickyAccountID == accountID {
					_ = s.deleteStickySessionAccountID(ctx, &groupID, hash)
				}
			}
		}
	}
}

func (s *OpenAIGatewayService) pinOpenAI429GuardConnection(account *Account, connID string) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	if !s.isOpenAI429GuardPooledWSMode(account) {
		return false
	}
	if !s.isOpenAIWS429GuardConnectionActive(account) {
		return false
	}
	pool := s.getOpenAIWSConnPool()
	if pool == nil {
		return false
	}
	// Keep the runtime lock through the generation check and pool mutation. A
	// concurrent clear or non-429 transition must not let an old snapshot
	// re-install a permanent guard pin after the account has changed state.
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	snapshot := s.openAIAccountRuntimeBlockSnapshotLocked(account.ID)
	if !snapshot.Active || snapshot.Reason != "429" || snapshot.Generation == 0 {
		return false
	}
	// Do not tie the old socket to the account cooldown or either ordinary
	// sticky TTL. The guard's contract is: retain it until a real connection
	// failure, explicit account invalidation, or process shutdown.
	return pool.PinGuardConnForGeneration(account.ID, connID, snapshot.Generation)
}

func (s *OpenAIGatewayService) bindOpenAIWSGuardContinuation(
	stateStore OpenAIWSStateStore,
	groupID int64,
	account *Account,
	responseID string,
	connID string,
	sessionHash string,
	storeDisabled bool,
) {
	if s == nil || stateStore == nil || account == nil ||
		!s.isOpenAIWS429GuardConnectionPinned(account, connID) {
		return
	}
	guardStore, ok := stateStore.(openAIWSGuardBindingStore)
	if !ok {
		return
	}
	if strings.TrimSpace(responseID) != "" {
		guardStore.BindGuardResponse(groupID, responseID, account.ID, connID)
	}
	if storeDisabled && strings.TrimSpace(sessionHash) != "" {
		guardStore.BindGuardSession(groupID, sessionHash, account.ID, connID)
	}
}

func classifyOpenAIWSAcquireError(err error) string {
	if err == nil {
		return "acquire_conn"
	}
	var dialErr *openAIWSDialError
	if errors.As(err, &dialErr) {
		if _, rateLimited := openAIWSDialRateLimitStatus(err); rateLimited {
			return "upstream_rate_limited"
		}
		switch dialErr.StatusCode {
		case 426:
			return "upgrade_required"
		case 401, 403:
			return "auth_failed"
		case 429:
			return "upstream_rate_limited"
		}
		if dialErr.StatusCode >= 500 {
			return "upstream_5xx"
		}
		return "dial_failed"
	}
	if errors.Is(err, errOpenAIWSConnQueueFull) {
		return "conn_queue_full"
	}
	if errors.Is(err, errOpenAIWSPreferredConnUnavailable) {
		return "preferred_conn_unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "acquire_timeout"
	}
	return "acquire_conn"
}

func isOpenAIWSRateLimitError(codeRaw, errTypeRaw, msgRaw string) bool {
	code := strings.ToLower(strings.TrimSpace(codeRaw))
	errType := strings.ToLower(strings.TrimSpace(errTypeRaw))
	msg := strings.ToLower(strings.TrimSpace(msgRaw))

	if strings.Contains(errType, "rate_limit") || strings.Contains(errType, "usage_limit") {
		return true
	}
	if strings.Contains(code, "rate_limit") || strings.Contains(code, "usage_limit") || strings.Contains(code, "insufficient_quota") {
		return true
	}
	if strings.Contains(msg, "usage limit") && strings.Contains(msg, "reached") {
		return true
	}
	if strings.Contains(msg, "rate limit") && (strings.Contains(msg, "reached") || strings.Contains(msg, "exceeded")) {
		return true
	}
	// Reverse proxies occasionally collapse the upstream error into a plain
	// transport message, for example "exceeded retry limit, last status: 429
	// Too Many Requests". Match only explicit status/message combinations so a
	// bare number in arbitrary text never becomes an account-level 429 signal.
	if strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "last status: 429") ||
		strings.Contains(msg, "last status=429") ||
		strings.Contains(msg, "http status 429") ||
		strings.Contains(msg, "http 429") {
		return true
	}
	return false
}

// isOpenAIWSExplicit429Signal reports evidence that the upstream actually
// returned HTTP 429. Semantic codes such as usage_limit_reached or
// insufficient_quota are useful for request failover, but by themselves they
// must not advance the account-level two-confirmation 429 guard.
func isOpenAIWSExplicit429Signal(upstreamStatus int, codeRaw, errTypeRaw, msgRaw string, responseBody []byte) bool {
	if upstreamStatus != 0 {
		return upstreamStatus == http.StatusTooManyRequests
	}
	if bodyStatus := openAIWSPayloadUpstreamStatus(responseBody); bodyStatus != 0 {
		return bodyStatus == http.StatusTooManyRequests
	}
	for _, raw := range []string{codeRaw, errTypeRaw, msgRaw} {
		value := strings.ToLower(strings.TrimSpace(raw))
		switch value {
		case "429", "http 429", "http status 429", "status 429", "status: 429":
			return true
		}
		if strings.Contains(value, "too many requests") ||
			strings.Contains(value, "last status: 429") ||
			strings.Contains(value, "last status=429") ||
			strings.Contains(value, "http status 429") ||
			strings.Contains(value, "http 429") {
			return true
		}
	}
	return false
}

// isOpenAIWSRateLimitSignal classifies an upstream rate-limit signal without
// letting a contradictory explicit status be overridden by a stale error code.
// Some upstream relays preserve an old rate_limit_* code while reporting a
// current 5xx; only a status-less frame may fall back to its textual fields.
func isOpenAIWSRateLimitSignal(upstreamStatus int, codeRaw, errTypeRaw, msgRaw string) bool {
	if upstreamStatus != 0 {
		return upstreamStatus == http.StatusTooManyRequests
	}
	return isOpenAIWSRateLimitError(codeRaw, errTypeRaw, msgRaw)
}

// openAIWSDialRateLimitStatus extracts an explicit 429 from a failed
// WebSocket handshake. A few reverse proxies lose the HTTP status while
// retaining the status in the response body or transport error text. An
// explicit non-zero status always wins so a stale rate-limit code cannot turn a
// 5xx handshake into an account-level 429 signal.
func openAIWSDialRateLimitStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var dialErr *openAIWSDialError
	if !errors.As(err, &dialErr) || dialErr == nil {
		return 0, false
	}
	if dialErr.StatusCode != 0 {
		return dialErr.StatusCode, dialErr.StatusCode == http.StatusTooManyRequests
	}

	if bodyStatus := openAIWSPayloadUpstreamStatus(dialErr.ResponseBody); bodyStatus != 0 {
		return bodyStatus, bodyStatus == http.StatusTooManyRequests
	}
	codeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(dialErr.ResponseBody)
	if errMsgRaw == "" && len(dialErr.ResponseBody) > 0 {
		errMsgRaw = strings.TrimSpace(extractUpstreamErrorMessage(dialErr.ResponseBody))
	}
	if dialErr.Err != nil {
		transportMessage := strings.TrimSpace(dialErr.Err.Error())
		if errMsgRaw == "" {
			errMsgRaw = transportMessage
		} else if transportMessage != "" {
			errMsgRaw += " " + transportMessage
		}
	}
	if isOpenAIWSExplicit429Signal(0, codeRaw, errTypeRaw, errMsgRaw, dialErr.ResponseBody) {
		return http.StatusTooManyRequests, true
	}
	return 0, false
}

// openAIWS429GuardErrorEventFailureStatus classifies an `error` event emitted
// by the exact socket retained for a confirmed OAuth 429. Rate-limit frames
// are the one expected semantic outcome: every other error frame means the
// old upstream connection is no longer trustworthy and must be migrated
// before anything reaches the client.
func openAIWS429GuardErrorEventFailureStatus(upstreamStatus int, codeRaw, errTypeRaw, msgRaw string) (int, bool) {
	if isOpenAIWSRateLimitSignal(upstreamStatus, codeRaw, errTypeRaw, msgRaw) {
		return 0, false
	}
	status := upstreamStatus
	if status == 0 {
		status = openAIWSErrorHTTPStatusFromRawWithMessage(codeRaw, errTypeRaw, msgRaw)
	}
	// An `error` envelope with a malformed/success status is still an upstream
	// connection failure. Preserve a retryable gateway classification instead
	// of returning a misleading success status to the failover coordinator.
	if status < http.StatusBadRequest {
		status = http.StatusBadGateway
	}
	return status, true
}

func (s *OpenAIGatewayService) persistOpenAIWSRateLimitSignal(ctx context.Context, account *Account, headers http.Header, responseBody []byte, codeRaw, errTypeRaw, msgRaw string, upstreamStatus ...int) {
	if s == nil || account == nil || account.Platform != PlatformOpenAI {
		return
	}
	status := 0
	if len(upstreamStatus) > 0 {
		status = upstreamStatus[0]
	}
	if !isOpenAIWSExplicit429Signal(status, codeRaw, errTypeRaw, msgRaw, responseBody) {
		return
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, headers, responseBody)
}

func (s *OpenAIGatewayService) newOpenAIWSRateLimitFailoverError(account *Account, headers http.Header, responseBody []byte, message string) *UpstreamFailoverError {
	return s.newOpenAIAccountFailoverError(
		account,
		http.StatusTooManyRequests,
		headers,
		responseBody,
		strings.TrimSpace(message),
		false,
		false,
	)
}

func classifyOpenAIWSErrorEventFromRaw(codeRaw, errTypeRaw, msgRaw string) (string, bool) {
	code := strings.ToLower(strings.TrimSpace(codeRaw))
	errType := strings.ToLower(strings.TrimSpace(errTypeRaw))
	msg := strings.ToLower(strings.TrimSpace(msgRaw))

	switch code {
	case "upgrade_required":
		return "upgrade_required", true
	case "websocket_not_supported", "websocket_unsupported":
		return "ws_unsupported", true
	case "websocket_connection_limit_reached":
		return "ws_connection_limit_reached", true
	case "invalid_encrypted_content":
		return "invalid_encrypted_content", true
	case "previous_response_not_found":
		return "previous_response_not_found", true
	}
	if isOpenAIWSRateLimitError(codeRaw, errTypeRaw, msgRaw) {
		return "upstream_rate_limited", false
	}
	if strings.Contains(msg, "upgrade required") || strings.Contains(msg, "status 426") {
		return "upgrade_required", true
	}
	if strings.Contains(errType, "upgrade") {
		return "upgrade_required", true
	}
	if strings.Contains(msg, "websocket") && strings.Contains(msg, "unsupported") {
		return "ws_unsupported", true
	}
	if strings.Contains(msg, "connection limit") && strings.Contains(msg, "websocket") {
		return "ws_connection_limit_reached", true
	}
	if strings.Contains(msg, "invalid_encrypted_content") ||
		(strings.Contains(msg, "encrypted content") && strings.Contains(msg, "could not be verified")) {
		return "invalid_encrypted_content", true
	}
	if strings.Contains(msg, "previous_response_not_found") ||
		(strings.Contains(msg, "previous response") && strings.Contains(msg, "not found")) {
		return "previous_response_not_found", true
	}
	if strings.Contains(errType, "server_error") || strings.Contains(code, "server_error") {
		return "upstream_error_event", true
	}
	return "event_error", false
}

func classifyOpenAIWSErrorEvent(message []byte) (string, bool) {
	if len(message) == 0 {
		return "event_error", false
	}
	return classifyOpenAIWSErrorEventFromRaw(parseOpenAIWSErrorEventFields(message))
}

func openAIWSErrorHTTPStatusFromRaw(codeRaw, errTypeRaw string) int {
	return openAIWSErrorHTTPStatusFromRawWithMessage(codeRaw, errTypeRaw, "")
}

func openAIWSErrorHTTPStatusFromRawWithMessage(codeRaw, errTypeRaw, msgRaw string) int {
	code := strings.ToLower(strings.TrimSpace(codeRaw))
	errType := strings.ToLower(strings.TrimSpace(errTypeRaw))
	switch {
	case strings.Contains(errType, "invalid_request"),
		strings.Contains(code, "invalid_request"),
		strings.Contains(code, "bad_request"),
		code == "invalid_encrypted_content",
		code == "previous_response_not_found":
		return http.StatusBadRequest
	case strings.Contains(errType, "authentication"),
		strings.Contains(code, "invalid_api_key"),
		strings.Contains(code, "unauthorized"):
		return http.StatusUnauthorized
	case strings.Contains(errType, "permission"),
		strings.Contains(code, "forbidden"):
		return http.StatusForbidden
	case isOpenAIWSRateLimitError(codeRaw, errTypeRaw, msgRaw):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func openAIWSErrorHTTPStatus(message []byte) int {
	if len(message) == 0 {
		return http.StatusBadGateway
	}
	codeRaw, errTypeRaw, msgRaw := parseOpenAIWSErrorEventFields(message)
	return openAIWSErrorHTTPStatusFromRawWithMessage(codeRaw, errTypeRaw, msgRaw)
}

func (s *OpenAIGatewayService) openAIWSFallbackCooldown() time.Duration {
	if s == nil || s.cfg == nil {
		return 30 * time.Second
	}
	seconds := s.cfg.Gateway.OpenAIWS.FallbackCooldownSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (s *OpenAIGatewayService) isOpenAIWSFallbackCooling(accountID int64) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	cooldown := s.openAIWSFallbackCooldown()
	if cooldown <= 0 {
		return false
	}
	rawUntil, ok := s.openaiWSFallbackUntil.Load(accountID)
	if !ok || rawUntil == nil {
		return false
	}
	until, ok := rawUntil.(time.Time)
	if !ok || until.IsZero() {
		s.openaiWSFallbackUntil.Delete(accountID)
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	s.openaiWSFallbackUntil.Delete(accountID)
	return false
}

func (s *OpenAIGatewayService) markOpenAIWSFallbackCooling(accountID int64, _ string) {
	if s == nil || accountID <= 0 {
		return
	}
	cooldown := s.openAIWSFallbackCooldown()
	if cooldown <= 0 {
		return
	}
	s.openaiWSFallbackUntil.Store(accountID, time.Now().Add(cooldown))
}

func (s *OpenAIGatewayService) clearOpenAIWSFallbackCooling(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiWSFallbackUntil.Delete(accountID)
}
