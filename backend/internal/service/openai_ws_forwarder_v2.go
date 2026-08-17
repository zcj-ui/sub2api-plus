package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (s *OpenAIGatewayService) forwardOpenAIWSV2(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	reqBody map[string]any,
	token string,
	decision OpenAIWSProtocolDecision,
	isCodexCLI bool,
	reqStream bool,
	originalModel string,
	mappedModel string,
	startTime time.Time,
	attempt int,
	lastFailureReason string,
	agentTaskRecoveryTried *bool,
) (*OpenAIForwardResult, error) {
	if s == nil || account == nil {
		return nil, wrapOpenAIWSFallback("invalid_state", errors.New("service or account is nil"))
	}
	responseModelObserver := &upstreamResponseModelObserver{}

	wsURL, err := s.buildOpenAIResponsesWSURL(account)
	if err != nil {
		return nil, wrapOpenAIWSFallback("build_ws_url", err)
	}
	proxyURL, proxyErr := resolveRequiredOpenAIProxyURL(account)
	if proxyErr != nil {
		return nil, wrapOpenAIWSFallback("proxy_unavailable", proxyErr)
	}
	wsHost := "-"
	wsPath := "-"
	if parsed, parseErr := url.Parse(wsURL); parseErr == nil && parsed != nil {
		if h := strings.TrimSpace(parsed.Host); h != "" {
			wsHost = normalizeOpenAIWSLogValue(h)
		}
		if p := strings.TrimSpace(parsed.Path); p != "" {
			wsPath = normalizeOpenAIWSLogValue(p)
		}
	}
	logOpenAIWSModeDebug(
		"dial_target account_id=%d account_type=%s ws_host=%s ws_path=%s",
		account.ID,
		account.Type,
		wsHost,
		wsPath,
	)

	payload := s.buildOpenAIWSCreatePayload(reqBody, account)
	payloadStrategy, removedKeys := applyOpenAIWSRetryPayloadStrategy(payload, attempt)
	previousResponseID := openAIWSPayloadString(payload, "previous_response_id")
	previousResponseIDKind := ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
	promptCacheKey := openAIWSPayloadString(payload, "prompt_cache_key")
	_, hasTools := payload["tools"]
	debugEnabled := isOpenAIWSModeDebugEnabled()
	payloadBytes := -1
	resolvePayloadBytes := func() int {
		if payloadBytes >= 0 {
			return payloadBytes
		}
		payloadBytes = len(payloadAsJSONBytes(payload))
		return payloadBytes
	}
	streamValue := "-"
	if raw, ok := payload["stream"]; ok {
		streamValue = normalizeOpenAIWSLogValue(strings.TrimSpace(fmt.Sprintf("%v", raw)))
	}
	turnState := ""
	turnMetadata := ""
	if c != nil && c.Request != nil {
		turnState = strings.TrimSpace(c.GetHeader(openAIWSTurnStateHeader))
		turnMetadata = strings.TrimSpace(c.GetHeader(openAIWSTurnMetadataHeader))
	}
	setOpenAIWSTurnMetadata(payload, turnMetadata)
	payloadEventType := openAIWSPayloadString(payload, "type")
	if payloadEventType == "" {
		payloadEventType = "response.create"
	}
	if s.shouldEmitOpenAIWSPayloadSchema(attempt) {
		logOpenAIWSModeInfo(
			"[debug] payload_schema account_id=%d attempt=%d event=%s payload_keys=%s payload_bytes=%d payload_key_sizes=%s input_summary=%s stream=%s payload_strategy=%s removed_keys=%s has_previous_response_id=%v has_prompt_cache_key=%v has_tools=%v",
			account.ID,
			attempt,
			payloadEventType,
			normalizeOpenAIWSLogValue(strings.Join(sortedKeys(payload), ",")),
			resolvePayloadBytes(),
			normalizeOpenAIWSLogValue(summarizeOpenAIWSPayloadKeySizes(payload, openAIWSPayloadKeySizeTopN)),
			normalizeOpenAIWSLogValue(summarizeOpenAIWSInput(payload["input"])),
			streamValue,
			normalizeOpenAIWSLogValue(payloadStrategy),
			normalizeOpenAIWSLogValue(strings.Join(removedKeys, ",")),
			previousResponseID != "",
			promptCacheKey != "",
			hasTools,
		)
	}

	stateStore := s.getOpenAIWSStateStore()
	groupID := getOpenAIGroupIDFromContext(c)
	sessionHash := s.GenerateSessionHash(c, nil)
	if sessionHash == "" {
		var legacySessionHash string
		sessionHash, legacySessionHash = openAIWSSessionHashesFromID(promptCacheKey)
		attachOpenAILegacySessionHashToGin(c, legacySessionHash)
	}
	if turnState == "" && stateStore != nil && sessionHash != "" {
		if savedTurnState, ok := stateStore.GetSessionTurnState(groupID, sessionHash); ok {
			turnState = savedTurnState
		}
	}
	preferredConnID := ""
	if stateStore != nil && previousResponseID != "" {
		if connID, ok := stateStore.GetResponseConn(previousResponseID); ok {
			preferredConnID = connID
		}
	}
	storeDisabled := s.isOpenAIWSStoreDisabledInRequest(reqBody, account)
	if stateStore != nil && storeDisabled && previousResponseID == "" && sessionHash != "" {
		if connID, ok := stateStore.GetSessionConn(groupID, sessionHash); ok {
			preferredConnID = connID
		}
	}
	// Codex can omit both continuation markers on a follow-up request while
	// the scheduler has already selected this account. Once the account owns a
	// confirmed 429 guard socket, keep that selected request on the exact old
	// connection instead of opening a fresh post-429 socket. Restrict this
	// fallback to the official Codex OAuth WSv2 path; generic clients still
	// require an explicit response/session binding and cannot inherit state.
	if preferredConnID == "" && previousResponseID == "" && sessionHash == "" &&
		isCodexCLI && s.isOpenAI429GuardPooledWSMode(account) {
		if pool := s.getOpenAIWSConnPool(); pool != nil {
			if guardConnID, ok := pool.PermanentGuardConnID(account.ID); ok {
				preferredConnID = guardConnID
			}
		}
	}
	forcePreferredConn := preferredConnID != "" &&
		s.isOpenAIWS429GuardConnectionPinned(account, preferredConnID)
	storeDisabledConnMode := s.openAIWSStoreDisabledConnMode()
	forceNewConnByPolicy := shouldForceNewConnOnStoreDisabled(storeDisabledConnMode, lastFailureReason)
	forceNewConn := forceNewConnByPolicy && storeDisabled && previousResponseID == "" && sessionHash != "" && preferredConnID == ""
	wsHeaders, sessionResolution, buildHdrErr := s.buildOpenAIWSHeaders(
		ctx,
		c,
		account,
		token,
		decision,
		isCodexCLI,
		turnState,
		turnMetadata,
		promptCacheKey,
		openAIWSPayloadString(payload, "model"),
		openAIWSPayloadString(payload, "service_tier"),
	)
	if buildHdrErr != nil {
		return nil, fmt.Errorf("build ws headers: %w", buildHdrErr)
	}
	logOpenAIWSModeDebug(
		"acquire_start account_id=%d account_type=%s transport=%s preferred_conn_id=%s has_previous_response_id=%v session_hash=%s has_turn_state=%v turn_state_len=%d has_turn_metadata=%v turn_metadata_len=%d store_disabled=%v store_disabled_conn_mode=%s retry_last_reason=%s force_new_conn=%v header_user_agent=%s header_openai_beta=%s header_originator=%s header_accept_language=%s header_session_id=%s header_conversation_id=%s session_id_source=%s conversation_id_source=%s has_prompt_cache_key=%v has_chatgpt_account_id=%v has_authorization=%v has_session_id=%v has_conversation_id=%v proxy_enabled=%v",
		account.ID,
		account.Type,
		normalizeOpenAIWSLogValue(string(decision.Transport)),
		truncateOpenAIWSLogValue(preferredConnID, openAIWSIDValueMaxLen),
		previousResponseID != "",
		truncateOpenAIWSLogValue(sessionHash, 12),
		turnState != "",
		len(turnState),
		turnMetadata != "",
		len(turnMetadata),
		storeDisabled,
		normalizeOpenAIWSLogValue(storeDisabledConnMode),
		truncateOpenAIWSLogValue(lastFailureReason, openAIWSLogValueMaxLen),
		forceNewConn,
		openAIWSHeaderValueForLog(wsHeaders, "user-agent"),
		openAIWSHeaderValueForLog(wsHeaders, "openai-beta"),
		openAIWSHeaderValueForLog(wsHeaders, "originator"),
		openAIWSHeaderValueForLog(wsHeaders, "accept-language"),
		openAIWSHeaderValueForLog(wsHeaders, "session_id"),
		openAIWSHeaderValueForLog(wsHeaders, "conversation_id"),
		normalizeOpenAIWSLogValue(sessionResolution.SessionSource),
		normalizeOpenAIWSLogValue(sessionResolution.ConversationSource),
		promptCacheKey != "",
		hasOpenAIWSHeader(wsHeaders, "chatgpt-account-id"),
		hasOpenAIWSHeader(wsHeaders, "authorization"),
		hasOpenAIWSHeader(wsHeaders, "session_id"),
		hasOpenAIWSHeader(wsHeaders, "conversation_id"),
		proxyURL != "",
	)

	acquireCtx, acquireCancel := context.WithTimeout(ctx, s.openAIWSAcquireTimeout())
	defer acquireCancel()
	blockBeforeAcquire := s.openAIAccountRuntimeBlockSnapshot(account.ID)

	lease, err := s.getOpenAIWSConnPool().Acquire(acquireCtx, openAIWSAcquireRequest{
		Account: account,
		WSURL:   wsURL,
		Headers: wsHeaders,
		HeadersFactory: func(factoryCtx context.Context, headers http.Header) (http.Header, error) {
			return s.refreshOpenAIAgentIdentityHeaders(factoryCtx, account, headers)
		},
		PreferredConnID:    preferredConnID,
		ForcePreferredConn: forcePreferredConn,
		ForceNewConn:       forceNewConn,
		ProxyURL:           proxyURL,
	})
	if err != nil {
		var agentDialErr *openAIWSDialError
		if s.isAgentIdentityAccount(ctx, account) && errors.As(err, &agentDialErr) && isAgentIdentityTaskInvalidWSDialError(agentDialErr) && agentTaskRecoveryTried != nil && !*agentTaskRecoveryTried {
			*agentTaskRecoveryTried = true
			if recoveryErr := s.recoverAgentIdentityTask(ctx, account, account.GetCredential("task_id")); recoveryErr != nil {
				return nil, fmt.Errorf("agent identity task recovery failed: %w", recoveryErr)
			}
			return nil, &agentIdentityTaskRecoveredError{}
		}
		s.handleOpenAIWSDialTransientFailure(ctx, account, mappedModel, err)
		dialStatus, dialClass, dialCloseStatus, dialCloseReason, dialRespServer, dialRespVia, dialRespCFRay, dialRespReqID := summarizeOpenAIWSDialError(err)
		logOpenAIWSModeInfo(
			"acquire_fail account_id=%d account_type=%s transport=%s reason=%s dial_status=%d dial_class=%s dial_close_status=%s dial_close_reason=%s dial_resp_server=%s dial_resp_via=%s dial_resp_cf_ray=%s dial_resp_x_request_id=%s cause=%s preferred_conn_id=%s force_new_conn=%v ws_host=%s ws_path=%s proxy_enabled=%v",
			account.ID,
			account.Type,
			normalizeOpenAIWSLogValue(string(decision.Transport)),
			normalizeOpenAIWSLogValue(classifyOpenAIWSAcquireError(err)),
			dialStatus,
			dialClass,
			dialCloseStatus,
			truncateOpenAIWSLogValue(dialCloseReason, openAIWSHeaderValueMaxLen),
			dialRespServer,
			dialRespVia,
			dialRespCFRay,
			dialRespReqID,
			truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen),
			truncateOpenAIWSLogValue(preferredConnID, openAIWSIDValueMaxLen),
			forceNewConn,
			wsHost,
			wsPath,
			proxyURL != "",
		)
		var dialErr *openAIWSDialError
		if errors.As(err, &dialErr) && dialErr != nil && dialErr.StatusCode == http.StatusTooManyRequests {
			s.persistOpenAIWSRateLimitSignal(ctx, account, dialErr.ResponseHeaders, nil, "rate_limit_exceeded", "rate_limit_error", strings.TrimSpace(err.Error()))
		}
		// A confirmed 429 guard deliberately keeps a healthy old socket alive.
		// Once that exact pinned socket cannot be acquired, however, it is no
		// longer a valid continuation and retrying the same account only burns the
		// request-local WS retry budget. Remove its stale bindings and let the
		// handler immediately select another account.
		if forcePreferredConn && isOpenAIWS429GuardSwitchableAcquireError(err) {
			s.clearOpenAIWSContinuationBindings(ctx, groupID, sessionHash, account.ID, previousResponseID, preferredConnID)
			return nil, newOpenAIUpstreamFailoverError(
				http.StatusBadGateway,
				http.Header{},
				nil,
				"Codex 429 guard connection became unavailable: "+sanitizeUpstreamErrorMessage(err.Error()),
				false,
			)
		}
		// A preferred guard socket can be temporarily occupied or queued. Those
		// conditions are not proof that the old connection failed; retain the
		// binding so the next request still targets the same healthy socket.
		return nil, wrapOpenAIWSFallback(classifyOpenAIWSAcquireError(err), err)
	}
	// cleanExit 标记正常终端事件退出，此时上游不会再发送帧，连接可安全归还复用。
	// 所有异常路径（读写错误、error 事件等）已在各自分支中提前调用 MarkBroken，
	// 因此 defer 中只需处理正常退出时不 MarkBroken 即可。
	// The guard state is captured after acquisition so callers can distinguish
	// a pre-existing socket from a replacement opened while the account was
	// already 429-blocked.
	s.stampOpenAIWSLeaseRuntimeBlockState(account.ID, lease, blockBeforeAcquire)
	cleanExit := false
	defer func() {
		if !cleanExit {
			lease.MarkBroken()
		}
		lease.Release()
	}()
	connID := strings.TrimSpace(lease.ConnID())
	logOpenAIWSModeDebug(
		"connected account_id=%d account_type=%s transport=%s conn_id=%s conn_reused=%v conn_pick_ms=%d queue_wait_ms=%d has_previous_response_id=%v",
		account.ID,
		account.Type,
		normalizeOpenAIWSLogValue(string(decision.Transport)),
		connID,
		lease.Reused(),
		lease.ConnPickDuration().Milliseconds(),
		lease.QueueWaitDuration().Milliseconds(),
		previousResponseID != "",
	)
	if previousResponseID != "" {
		logOpenAIWSModeInfo(
			"continuation_probe account_id=%d account_type=%s conn_id=%s previous_response_id=%s previous_response_id_kind=%s preferred_conn_id=%s conn_reused=%v store_disabled=%v session_hash=%s header_session_id=%s header_conversation_id=%s session_id_source=%s conversation_id_source=%s has_turn_state=%v turn_state_len=%d has_prompt_cache_key=%v",
			account.ID,
			account.Type,
			truncateOpenAIWSLogValue(connID, openAIWSIDValueMaxLen),
			truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
			normalizeOpenAIWSLogValue(previousResponseIDKind),
			truncateOpenAIWSLogValue(preferredConnID, openAIWSIDValueMaxLen),
			lease.Reused(),
			storeDisabled,
			truncateOpenAIWSLogValue(sessionHash, 12),
			openAIWSHeaderValueForLog(wsHeaders, "session_id"),
			openAIWSHeaderValueForLog(wsHeaders, "conversation_id"),
			normalizeOpenAIWSLogValue(sessionResolution.SessionSource),
			normalizeOpenAIWSLogValue(sessionResolution.ConversationSource),
			turnState != "",
			len(turnState),
			promptCacheKey != "",
		)
	}
	if c != nil {
		SetOpsLatencyMs(c, OpsOpenAIWSConnPickMsKey, lease.ConnPickDuration().Milliseconds())
		SetOpsLatencyMs(c, OpsOpenAIWSQueueWaitMsKey, lease.QueueWaitDuration().Milliseconds())
		c.Set(OpsOpenAIWSConnReusedKey, lease.Reused())
		if connID != "" {
			c.Set(OpsOpenAIWSConnIDKey, connID)
		}
	}

	handshakeTurnState := strings.TrimSpace(lease.HandshakeHeader(openAIWSTurnStateHeader))
	logOpenAIWSModeDebug(
		"handshake account_id=%d conn_id=%s has_turn_state=%v turn_state_len=%d",
		account.ID,
		connID,
		handshakeTurnState != "",
		len(handshakeTurnState),
	)
	if handshakeTurnState != "" {
		if stateStore != nil && sessionHash != "" {
			stateStore.BindSessionTurnState(groupID, sessionHash, handshakeTurnState, s.openAIWSSessionStickyTTL())
		}
		if c != nil {
			c.Header(http.CanonicalHeaderKey(openAIWSTurnStateHeader), handshakeTurnState)
		}
	}

	if prewarmErr := s.performOpenAIWSGeneratePrewarm(
		ctx,
		lease,
		decision,
		payload,
		previousResponseID,
		reqBody,
		account,
		stateStore,
		groupID,
	); prewarmErr != nil {
		// A confirmed OAuth 429 is a semantic account state. The prewarm event
		// can fail the current request while the pooled socket remains healthy
		// for the next bound request; transport/protocol failures keep the
		// default eviction path.
		if openAIWSFallbackKeepsConnection(prewarmErr) {
			cleanExit = true
		}
		return nil, prewarmErr
	}

	if err := lease.WriteJSONWithContextTimeout(ctx, payload, s.openAIWSWriteTimeout()); err != nil {
		lease.MarkBroken()
		logOpenAIWSModeInfo(
			"write_request_fail account_id=%d conn_id=%s cause=%s payload_bytes=%d",
			account.ID,
			connID,
			truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen),
			resolvePayloadBytes(),
		)
		return nil, wrapOpenAIWSFallback("write_request", err)
	}
	if debugEnabled {
		logOpenAIWSModeDebug(
			"write_request_sent account_id=%d conn_id=%s stream=%v payload_bytes=%d previous_response_id=%s",
			account.ID,
			connID,
			reqStream,
			resolvePayloadBytes(),
			truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
		)
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	var finalResponse []byte
	wroteDownstream := false
	needModelReplace := originalModel != mappedModel
	var mappedModelBytes []byte
	if needModelReplace && mappedModel != "" {
		mappedModelBytes = []byte(mappedModel)
	}
	bufferedStreamEvents := make([][]byte, 0, 4)
	eventCount := 0
	tokenEventCount := 0
	terminalEventCount := 0
	bufferedEventCount := 0
	flushedBufferedEventCount := 0
	firstEventType := ""
	lastEventType := ""
	upstreamTerminalEvent := ""
	// A few reverse proxies emit response.failed as the complete non-streaming
	// reply and omit the nested response object. Keep the exact 429 envelope so
	// the guard can return a client error without evicting the healthy socket or
	// falling through to the generic missing_final_response path.
	var retained429ResponseFailedMessage []byte

	var flusher http.Flusher
	if reqStream {
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), http.Header{}, s.responseHeaderFilter)
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		f, ok := c.Writer.(http.Flusher)
		if !ok {
			lease.MarkBroken()
			return nil, wrapOpenAIWSFallback("streaming_not_supported", errors.New("streaming not supported"))
		}
		flusher = f
	}

	clientDisconnected := false
	rateLimitSignalHandled := false
	guardLeaseMayRetain := func() bool {
		if !lease.openAI429GuardActiveAtAcquire {
			return true
		}
		return s.isOpenAIWS429GuardConnectionCandidate(account, lease.ConnID(), lease.openAIRuntimeBlockGeneration)
	}
	guardRateLimitedConnectionActive := func() bool {
		return guardLeaseMayRetain() && s.isOpenAIWS429GuardConnectionRetained(account, lease.ConnID())
	}
	recordRateLimitSignal := func(upstreamStatus int, codeRaw, errTypeRaw, errMsgRaw string, responseBody []byte) bool {
		isRateLimit := isOpenAIWSRateLimitSignal(upstreamStatus, codeRaw, errTypeRaw, errMsgRaw)
		if !isRateLimit || rateLimitSignalHandled {
			return isRateLimit
		}
		if upstreamStatus == http.StatusTooManyRequests && !isOpenAIWSRateLimitError(codeRaw, errTypeRaw, errMsgRaw) {
			s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusTooManyRequests, lease.HandshakeHeaders(), responseBody)
		} else {
			s.persistOpenAIWSRateLimitSignal(ctx, account, lease.HandshakeHeaders(), responseBody, codeRaw, errTypeRaw, errMsgRaw)
		}
		rateLimitSignalHandled = true
		if isRateLimit && guardLeaseMayRetain() {
			s.markOpenAI429GuardConnectionProof(account, lease)
			if s.pinOpenAI429GuardConnection(account, lease.ConnID()) {
				lease.openAI429GuardProven.Store(true)
			}
		}
		return true
	}
	flushBatchSize := s.openAIWSEventFlushBatchSize()
	flushInterval := s.openAIWSEventFlushInterval()
	pendingFlushEvents := 0
	lastFlushAt := time.Now()
	flushStreamWriter := func(force bool) {
		if clientDisconnected || flusher == nil || pendingFlushEvents <= 0 {
			return
		}
		if !force && flushBatchSize > 1 && pendingFlushEvents < flushBatchSize {
			if flushInterval <= 0 || time.Since(lastFlushAt) < flushInterval {
				return
			}
		}
		flusher.Flush()
		pendingFlushEvents = 0
		lastFlushAt = time.Now()
	}
	emitStreamMessage := func(message []byte, forceFlush bool) {
		if clientDisconnected {
			return
		}
		frame := make([]byte, 0, len(message)+8)
		frame = append(frame, "data: "...)
		frame = append(frame, message...)
		frame = append(frame, '\n', '\n')
		_, wErr := c.Writer.Write(frame)
		if wErr == nil {
			wroteDownstream = true
			pendingFlushEvents++
			flushStreamWriter(forceFlush)
			return
		}
		clientDisconnected = true
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI WS Mode] client disconnected, continue draining upstream: account=%d", account.ID)
	}
	flushBufferedStreamEvents := func(reason string) {
		if len(bufferedStreamEvents) == 0 {
			return
		}
		flushed := len(bufferedStreamEvents)
		for _, buffered := range bufferedStreamEvents {
			emitStreamMessage(buffered, false)
		}
		bufferedStreamEvents = bufferedStreamEvents[:0]
		flushStreamWriter(true)
		flushedBufferedEventCount += flushed
		if debugEnabled {
			logOpenAIWSModeDebug(
				"buffer_flush account_id=%d conn_id=%s reason=%s flushed=%d total_flushed=%d client_disconnected=%v",
				account.ID,
				connID,
				truncateOpenAIWSLogValue(reason, openAIWSLogValueMaxLen),
				flushed,
				flushedBufferedEventCount,
				clientDisconnected,
			)
		}
	}

	readTimeout := s.openAIWSReadTimeout()
	var pendingJSONDocuments [][]byte

	for {
		var message []byte
		var readErr error
		if len(pendingJSONDocuments) > 0 {
			message = pendingJSONDocuments[0]
			pendingJSONDocuments = pendingJSONDocuments[1:]
		} else {
			message, readErr = lease.ReadMessageWithContextTimeout(ctx, readTimeout)
			if readErr == nil {
				if documents, repaired := splitOpenAIConcatenatedJSONDocuments(message); repaired {
					logOpenAIWSModeInfo(
						"concatenated_json_repaired account_id=%d conn_id=%s documents=%d bytes=%d",
						account.ID,
						truncateOpenAIWSLogValue(connID, openAIWSIDValueMaxLen),
						len(documents),
						len(message),
					)
					message = documents[0]
					pendingJSONDocuments = append(pendingJSONDocuments, documents[1:]...)
				}
			}
		}
		if readErr == nil && !json.Valid(message) {
			eventType, _, _ := parseOpenAIWSEventEnvelope(message)
			if eventType == "" {
				eventType = "unknown"
			}
			lease.MarkBroken()
			logOpenAIWSModeInfo(
				"invalid_event_json account_id=%d conn_id=%s event_type=%s bytes=%d wrote_downstream=%v",
				account.ID,
				truncateOpenAIWSLogValue(connID, openAIWSIDValueMaxLen),
				truncateOpenAIWSLogValue(eventType, openAIWSLogValueMaxLen),
				len(message),
				wroteDownstream,
			)
			if !wroteDownstream {
				return nil, wrapOpenAIWSFallback("invalid_event_json", errors.New("upstream websocket returned malformed Responses event JSON"))
			}
			return nil, errors.New("upstream websocket returned malformed Responses event JSON after downstream output")
		}
		if readErr != nil {
			lease.MarkBroken()
			closeStatus, closeReason := summarizeOpenAIWSReadCloseError(readErr)
			logOpenAIWSModeInfo(
				"read_fail account_id=%d conn_id=%s wrote_downstream=%v close_status=%s close_reason=%s cause=%s events=%d token_events=%d terminal_events=%d buffered_pending=%d buffered_flushed=%d first_event=%s last_event=%s",
				account.ID,
				connID,
				wroteDownstream,
				closeStatus,
				closeReason,
				truncateOpenAIWSLogValue(readErr.Error(), openAIWSLogValueMaxLen),
				eventCount,
				tokenEventCount,
				terminalEventCount,
				len(bufferedStreamEvents),
				flushedBufferedEventCount,
				truncateOpenAIWSLogValue(firstEventType, openAIWSLogValueMaxLen),
				truncateOpenAIWSLogValue(lastEventType, openAIWSLogValueMaxLen),
			)
			if !wroteDownstream {
				return nil, wrapOpenAIWSFallback(classifyOpenAIWSReadFallbackReason(readErr), readErr)
			}
			if clientDisconnected {
				break
			}
			setOpsUpstreamError(c, 0, sanitizeUpstreamErrorMessage(readErr.Error()), "")
			return nil, fmt.Errorf("openai ws read event: %w", readErr)
		}
		if normalized, changed := normalizeCompletedImageGenerationStatus(message); changed {
			message = normalized
		}

		eventType, eventResponseID, responseField := parseOpenAIWSEventEnvelope(message)
		if eventType == "" {
			continue
		}
		responseModelObserver.ObserveOpenAI(message, eventType)
		eventCount++
		if firstEventType == "" {
			firstEventType = eventType
		}
		lastEventType = eventType

		if responseID == "" && eventResponseID != "" {
			responseID = eventResponseID
		}

		isTokenEvent := isOpenAIWSTokenEvent(eventType)
		if isTokenEvent {
			tokenEventCount++
		}
		isTerminalEvent := isOpenAIWSTerminalEvent(eventType)
		if isTerminalEvent {
			terminalEventCount++
		}
		if firstTokenMs == nil && isTokenEvent {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		if debugEnabled && shouldLogOpenAIWSEvent(eventCount, eventType) {
			logOpenAIWSModeDebug(
				"event_received account_id=%d conn_id=%s idx=%d type=%s bytes=%d token=%v terminal=%v buffered_pending=%d",
				account.ID,
				connID,
				eventCount,
				truncateOpenAIWSLogValue(eventType, openAIWSLogValueMaxLen),
				len(message),
				isTokenEvent,
				isTerminalEvent,
				len(bufferedStreamEvents),
			)
		}

		if !clientDisconnected {
			if needModelReplace && len(mappedModelBytes) > 0 && openAIWSEventMayContainModel(eventType) && bytes.Contains(message, mappedModelBytes) {
				message = replaceOpenAIWSMessageModel(message, mappedModel, originalModel)
			}
			if openAIWSEventMayContainToolCalls(eventType) && openAIWSMessageLikelyContainsToolCalls(message) {
				if corrected, changed := s.toolCorrector.CorrectToolCallsInSSEBytes(message); changed {
					message = corrected
				}
			}
		}
		if openAIWSEventShouldParseUsage(eventType) {
			parseOpenAIWSResponseUsageFromCompletedEvent(message, usage)
		}
		imageCounter.AddSSEData(message)

		if eventType == "response.failed" {
			guardAccountActiveAtEvent := s.isOpenAIWS429GuardAccountActiveForLease(account, lease)
			errCodeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(message)
			upstreamStatus := openAIWSPayloadUpstreamStatus(message)
			isRateLimit := recordRateLimitSignal(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw, message)
			// A confirming 429 may have activated and pinned this exact lease
			// while recordRateLimitSignal ran. Re-read after recording so that
			// signal is retained, while a non-429 failure on an unproven fresh
			// socket is never allowed to return to the pool.
			guardConnectionAtEvent := guardRateLimitedConnectionActive()
			if hit, code, msg := detectOpenAICyberPolicy(message); hit {
				MarkOpsCyberPolicy(c, CyberPolicyMark{
					Code:           code,
					Message:        msg,
					Body:           truncateString(string(message), 4096),
					UpstreamStatus: http.StatusOK,
					UpstreamInTok:  usage.InputTokens,
					UpstreamOutTok: usage.OutputTokens,
				})
			}
			if guardAccountActiveAtEvent && !guardConnectionAtEvent && !isRateLimit {
				failedStatus, _ := openAIWS429GuardErrorEventFailureStatus(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw)
				lease.MarkBroken()
				if !clientDisconnected && !wroteDownstream {
					return nil, &UpstreamFailoverError{
						StatusCode:      failedStatus,
						ResponseBody:    append([]byte(nil), message...),
						ResponseHeaders: cloneHeader(lease.HandshakeHeaders()),
					}
				}
			}
			if !clientDisconnected && !wroteDownstream && isRateLimit && !guardRateLimitedConnectionActive() {
				lease.MarkBroken()
				return nil, &UpstreamFailoverError{
					StatusCode:      http.StatusTooManyRequests,
					ResponseBody:    append([]byte(nil), message...),
					ResponseHeaders: cloneHeader(lease.HandshakeHeaders()),
				}
			}
			if guardConnectionAtEvent {
				// A response.failed envelope is terminal failure even when a
				// reverse proxy supplies a malformed successful status. Reuse the
				// same strict classifier as ordinary error frames so the pinned
				// old socket is evicted immediately.
				if failedStatus, failed := openAIWS429GuardErrorEventFailureStatus(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw); failed {
					lease.MarkBroken()
					if !clientDisconnected && !wroteDownstream {
						return nil, &UpstreamFailoverError{
							StatusCode:      failedStatus,
							ResponseBody:    append([]byte(nil), message...),
							ResponseHeaders: cloneHeader(lease.HandshakeHeaders()),
						}
					}
				}
				if isRateLimit && guardRateLimitedConnectionActive() && len(pendingJSONDocuments) == 0 {
					retained429ResponseFailedMessage = append(retained429ResponseFailedMessage[:0], message...)
				}
			}
		}

		if eventType == "error" {
			guardAccountActiveAtEvent := s.isOpenAIWS429GuardAccountActiveForLease(account, lease)
			s.handleOpenAIWSErrorEventTransientFailure(ctx, account, mappedModel, lease.HandshakeHeaders(), message)
			errCodeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(message)
			upstreamStatus := openAIWSPayloadUpstreamStatus(message)
			isRateLimit := recordRateLimitSignal(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw, message)
			guardConnectionAtEvent := guardRateLimitedConnectionActive()
			// A retained 429 socket is usable only while it continues to emit
			// rate-limit semantics. Any other `error` envelope is an upstream
			// failure; evict it before the client sees the frame so the handler
			// can fail over to another account.
			if guardAccountActiveAtEvent && !guardConnectionAtEvent && !isRateLimit {
				failedStatus, _ := openAIWS429GuardErrorEventFailureStatus(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw)
				lease.MarkBroken()
				if !clientDisconnected && !wroteDownstream {
					return nil, &UpstreamFailoverError{
						StatusCode:      failedStatus,
						ResponseBody:    append([]byte(nil), message...),
						ResponseHeaders: cloneHeader(lease.HandshakeHeaders()),
					}
				}
			}
			if guardConnectionAtEvent {
				if failedStatus, failed := openAIWS429GuardErrorEventFailureStatus(upstreamStatus, errCodeRaw, errTypeRaw, errMsgRaw); failed {
					lease.MarkBroken()
					if !clientDisconnected && !wroteDownstream {
						return nil, &UpstreamFailoverError{
							StatusCode:      failedStatus,
							ResponseBody:    append([]byte(nil), message...),
							ResponseHeaders: cloneHeader(lease.HandshakeHeaders()),
						}
					}
				}
			}
			errMsg := strings.TrimSpace(errMsgRaw)
			if errMsg == "" {
				errMsg = "Upstream websocket error"
			}
			fallbackReason, canFallback := classifyOpenAIWSErrorEventFromRaw(errCodeRaw, errTypeRaw, errMsgRaw)
			if isRateLimit {
				fallbackReason = "upstream_rate_limited"
				canFallback = true
			}
			if guardConnectionAtEvent {
				if status := openAIWSPayloadTransientStatus(message); status >= http.StatusInternalServerError {
					canFallback = true
				}
			}
			errCode, errType, errMessage := summarizeOpenAIWSErrorEventFieldsFromRaw(errCodeRaw, errTypeRaw, errMsgRaw)
			logOpenAIWSModeInfo(
				"error_event account_id=%d conn_id=%s idx=%d fallback_reason=%s can_fallback=%v err_code=%s err_type=%s err_message=%s",
				account.ID,
				connID,
				eventCount,
				truncateOpenAIWSLogValue(fallbackReason, openAIWSLogValueMaxLen),
				canFallback,
				errCode,
				errType,
				errMessage,
			)
			if fallbackReason == "previous_response_not_found" {
				logOpenAIWSModeInfo(
					"previous_response_not_found_diag account_id=%d account_type=%s conn_id=%s previous_response_id=%s previous_response_id_kind=%s response_id=%s event_idx=%d req_stream=%v store_disabled=%v conn_reused=%v session_hash=%s header_session_id=%s header_conversation_id=%s session_id_source=%s conversation_id_source=%s has_turn_state=%v turn_state_len=%d has_prompt_cache_key=%v err_code=%s err_type=%s err_message=%s",
					account.ID,
					account.Type,
					connID,
					truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
					normalizeOpenAIWSLogValue(previousResponseIDKind),
					truncateOpenAIWSLogValue(responseID, openAIWSIDValueMaxLen),
					eventCount,
					reqStream,
					storeDisabled,
					lease.Reused(),
					truncateOpenAIWSLogValue(sessionHash, 12),
					openAIWSHeaderValueForLog(wsHeaders, "session_id"),
					openAIWSHeaderValueForLog(wsHeaders, "conversation_id"),
					normalizeOpenAIWSLogValue(sessionResolution.SessionSource),
					normalizeOpenAIWSLogValue(sessionResolution.ConversationSource),
					turnState != "",
					len(turnState),
					promptCacheKey != "",
					errCode,
					errType,
					errMessage,
				)
			}
			keepRateLimitedConnection := isRateLimit && guardRateLimitedConnectionActive()
			if guardConnectionAtEvent && openAIWSPayloadTransientStatus(message) >= http.StatusInternalServerError {
				keepRateLimitedConnection = false
			}
			// A confirmed 429 is a semantic account state, not a broken socket.
			// Keep the lease reusable; transport/read failures still mark it broken.
			if !keepRateLimitedConnection {
				lease.MarkBroken()
			}
			if !wroteDownstream && canFallback && !keepRateLimitedConnection {
				return nil, wrapOpenAIWSFallback(fallbackReason, errors.New(errMsg))
			}
			// Some upstreams send only error.status/status_code. Preserve that
			// explicit HTTP classification instead of degrading a status-only 429
			// to the generic 502 mapper below.
			statusCode := openAIWSPayloadUpstreamStatus(message)
			if statusCode == 0 {
				statusCode = openAIWSErrorHTTPStatusFromRawWithMessage(errCodeRaw, errTypeRaw, errMsgRaw)
			}
			setOpsUpstreamError(c, statusCode, errMsg, "")
			if reqStream && !clientDisconnected {
				flushBufferedStreamEvents("error_event")
				clientMessage := message
				if updated, changed := ensureOpenAIWSErrorEventClientDetail(message); changed {
					clientMessage = updated
					logOpenAIWSModeInfo(
						"error_event_detail_injected account_id=%d conn_id=%s idx=%d",
						account.ID,
						connID,
						eventCount,
					)
				}
				emitStreamMessage(clientMessage, true)
			}
			if !reqStream {
				c.JSON(statusCode, gin.H{
					"error": gin.H{
						"type":    "upstream_error",
						"message": errMsg,
					},
				})
			}
			if keepRateLimitedConnection {
				// A confirmed 429 is not a websocket transport failure. The current
				// request still returns its error, while the healthy connection stays
				// available for the next bound continuation. Promote the prior
				// response/session tuple now; otherwise its ordinary sticky TTL can
				// expire while this socket remains permanently reserved.
				s.bindOpenAIWSGuardContinuation(
					stateStore,
					groupID,
					account,
					previousResponseID,
					lease.ConnID(),
					sessionHash,
					storeDisabled,
				)
				cleanExit = true
			}
			return nil, fmt.Errorf("openai ws error event: %s", errMsg)
		}

		if reqStream {
			// 在首个 token 前先缓冲事件（如 response.created），
			// 以便上游早期断连时仍可安全回退到 HTTP，不给下游发送半截流。
			shouldBuffer := firstTokenMs == nil && !isTokenEvent && !isTerminalEvent
			if shouldBuffer {
				buffered := make([]byte, len(message))
				copy(buffered, message)
				bufferedStreamEvents = append(bufferedStreamEvents, buffered)
				bufferedEventCount++
				if debugEnabled && shouldLogOpenAIWSBufferedEvent(bufferedEventCount) {
					logOpenAIWSModeDebug(
						"buffer_enqueue account_id=%d conn_id=%s idx=%d event_idx=%d event_type=%s buffer_size=%d",
						account.ID,
						connID,
						bufferedEventCount,
						eventCount,
						truncateOpenAIWSLogValue(eventType, openAIWSLogValueMaxLen),
						len(bufferedStreamEvents),
					)
				}
			} else {
				flushBufferedStreamEvents(eventType)
				emitStreamMessage(message, isTerminalEvent)
			}
		} else {
			if responseField.Exists() && responseField.Type == gjson.JSON {
				finalResponse = []byte(responseField.Raw)
			}
		}

		if isTerminalEvent {
			upstreamTerminalEvent = s.handleOpenAIWSTerminalTransientFailure(ctx, account, mappedModel, lease.HandshakeHeaders(), message)
			// A terminal event must be the final JSON document in its WS message.
			// Ignore any tail for the completed client turn, but never reuse the
			// ambiguous upstream connection for another request.
			cleanExit = len(pendingJSONDocuments) == 0
			break
		}
	}

	if !reqStream {
		if len(finalResponse) == 0 {
			if len(retained429ResponseFailedMessage) > 0 && s.isOpenAIWS429GuardConnectionRetained(account, lease.ConnID()) {
				guardResponseID := strings.TrimSpace(responseID)
				if guardResponseID == "" {
					guardResponseID = strings.TrimSpace(previousResponseID)
				}
				s.bindOpenAIWSGuardContinuation(stateStore, groupID, account, guardResponseID, lease.ConnID(), sessionHash, storeDisabled)
				cleanExit = true
				errCodeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(retained429ResponseFailedMessage)
				errMsg := strings.TrimSpace(errMsgRaw)
				if errMsg == "" {
					errMsg = "upstream rate limit exceeded"
				}
				setOpsUpstreamError(c, http.StatusTooManyRequests, errMsg, "")
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error": gin.H{
						"type":    "rate_limit_error",
						"message": errMsg,
						"code":    strings.TrimSpace(errCodeRaw),
						"param":   strings.TrimSpace(errTypeRaw),
					},
				})
				return nil, fmt.Errorf("openai ws response.failed: %s", errMsg)
			}
			logOpenAIWSModeInfo(
				"missing_final_response account_id=%d conn_id=%s events=%d token_events=%d terminal_events=%d wrote_downstream=%v",
				account.ID,
				connID,
				eventCount,
				tokenEventCount,
				terminalEventCount,
				wroteDownstream,
			)
			if !wroteDownstream {
				return nil, wrapOpenAIWSFallback("missing_final_response", errors.New("no terminal response payload"))
			}
			return nil, errors.New("ws finished without final response")
		}

		if needModelReplace {
			finalResponse = s.replaceModelInResponseBody(finalResponse, mappedModel, originalModel)
		}
		finalResponse = s.correctToolCallsInResponseBody(finalResponse)
		populateOpenAIUsageFromResponseJSON(finalResponse, usage)
		if responseID == "" {
			responseID = strings.TrimSpace(gjson.GetBytes(finalResponse, "id").String())
		}

		c.Data(http.StatusOK, "application/json", finalResponse)
	} else {
		flushStreamWriter(true)
	}

	if responseID != "" && stateStore != nil {
		ttl := s.openAIWSResponseStickyTTL()
		logOpenAIWSBindResponseAccountWarn(groupID, account.ID, responseID, stateStore.BindResponseAccount(ctx, groupID, responseID, account.ID, ttl))
		stateStore.BindResponseConn(responseID, lease.ConnID(), ttl)
	}
	if stateStore != nil && storeDisabled && sessionHash != "" {
		stateStore.BindSessionConn(groupID, sessionHash, lease.ConnID(), s.openAIWSSessionStickyTTL())
	}
	guardResponseID := responseID
	if strings.TrimSpace(guardResponseID) == "" {
		guardResponseID = previousResponseID
	}
	s.bindOpenAIWSGuardContinuation(stateStore, groupID, account, guardResponseID, lease.ConnID(), sessionHash, storeDisabled)
	firstTokenMsValue := -1
	if firstTokenMs != nil {
		firstTokenMsValue = *firstTokenMs
	}
	logOpenAIWSModeDebug(
		"completed account_id=%d conn_id=%s response_id=%s stream=%v duration_ms=%d events=%d token_events=%d terminal_events=%d buffered_events=%d buffered_flushed=%d first_event=%s last_event=%s first_token_ms=%d wrote_downstream=%v client_disconnected=%v",
		account.ID,
		connID,
		truncateOpenAIWSLogValue(strings.TrimSpace(responseID), openAIWSIDValueMaxLen),
		reqStream,
		time.Since(startTime).Milliseconds(),
		eventCount,
		tokenEventCount,
		terminalEventCount,
		bufferedEventCount,
		flushedBufferedEventCount,
		truncateOpenAIWSLogValue(firstEventType, openAIWSLogValueMaxLen),
		truncateOpenAIWSLogValue(lastEventType, openAIWSLogValueMaxLen),
		firstTokenMsValue,
		wroteDownstream,
		clientDisconnected,
	)

	return &OpenAIForwardResult{
		RequestID:                     responseID,
		Usage:                         *usage,
		Model:                         originalModel,
		UpstreamModel:                 mappedModel,
		UpstreamResponseModel:         responseModelObserver.Model(),
		UpstreamResponseModelConflict: responseModelObserver.Conflict(),
		ImageCount:                    imageCounter.Count(),
		ImageOutputSizes:              imageCounter.Sizes(),
		ServiceTier:                   extractOpenAIServiceTier(reqBody),
		ReasoningEffort:               extractOpenAIReasoningEffort(reqBody, mappedModel, originalModel),
		Stream:                        reqStream,
		OpenAIWSMode:                  true,
		UpstreamTerminalEvent:         upstreamTerminalEvent,
		ResponseHeaders:               lease.HandshakeHeaders(),
		Duration:                      time.Since(startTime),
		FirstTokenMs:                  firstTokenMs,
	}, nil
}

// ProxyResponsesWebSocketFromClient 处理客户端入站 WebSocket（OpenAI Responses WS Mode）并转发到上游。
// 当前实现按“单请求 -> 终止事件 -> 下一请求”的顺序代理，适配 Codex CLI 的 turn 模式。
// stripCodexSparkImageGenerationToolFromRawPayload removes the image_generation
// tool from a raw /responses payload when the upstream model is gpt-5.3-codex-spark.
// Spark rejects that tool upstream with HTTP 400 (invalid_request_error, param=tools);
// Codex clients advertise it by default. Returns the (possibly unchanged) payload,
// whether it changed, and any JSON decode error.
func stripCodexSparkImageGenerationToolFromRawPayload(payload []byte, model string) ([]byte, bool, error) {
	if !isCodexSparkModel(model) {
		return payload, false, nil
	}
	return stripOpenAIImageGenerationToolsFromRawPayload(payload)
}
