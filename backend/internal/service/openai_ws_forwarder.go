package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	coderws "github.com/coder/websocket"
	"go.uber.org/zap"
)

const (
	openAIWSBetaV1Value = "responses_websockets=2026-02-04"
	openAIWSBetaV2Value = "responses_websockets=2026-02-06"

	openAIWSTurnStateHeader    = "x-codex-turn-state"
	openAIWSTurnMetadataHeader = "x-codex-turn-metadata"

	openAIWSLogValueMaxLen      = 160
	openAIWSHeaderValueMaxLen   = 120
	openAIWSIDValueMaxLen       = 64
	openAIWSEventLogHeadLimit   = 20
	openAIWSEventLogEveryN      = 50
	openAIWSBufferLogHeadLimit  = 8
	openAIWSBufferLogEveryN     = 20
	openAIWSPrewarmEventLogHead = 10
	openAIWSPayloadKeySizeTopN  = 6

	openAIWSPayloadSizeEstimateDepth    = 3
	openAIWSPayloadSizeEstimateMaxBytes = 64 * 1024
	openAIWSPayloadSizeEstimateMaxItems = 16

	openAIWSEventFlushBatchSizeDefault    = 4
	openAIWSEventFlushIntervalDefault     = 25 * time.Millisecond
	openAIWSPayloadLogSampleDefault       = 0.2
	openAIWSPassthroughIdleTimeoutDefault = time.Hour

	openAIWSStoreDisabledConnModeStrict   = "strict"
	openAIWSStoreDisabledConnModeAdaptive = "adaptive"
	openAIWSStoreDisabledConnModeOff      = "off"

	openAIWSIngressStagePreviousResponseNotFound = "previous_response_not_found"
	openAIWSMaxPrevResponseIDDeletePasses        = 8
)

var openAIWSLogValueReplacer = strings.NewReplacer(
	"error", "err",
	"fallback", "fb",
	"warning", "warnx",
	"failed", "fail",
)

var openAIWSIngressPreflightPingIdle = 20 * time.Second

// openAIWSFallbackError 表示可安全回退到 HTTP 的 WS 错误（尚未写下游）。
type openAIWSFallbackError struct {
	Reason         string
	Err            error
	KeepConnection bool
}

func (e *openAIWSFallbackError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("openai ws fallback: %s", strings.TrimSpace(e.Reason))
	}
	return fmt.Sprintf("openai ws fallback: %s: %v", strings.TrimSpace(e.Reason), e.Err)
}

func (e *openAIWSFallbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func wrapOpenAIWSFallback(reason string, err error) error {
	return &openAIWSFallbackError{Reason: strings.TrimSpace(reason), Err: err}
}

// wrapOpenAIWSFallbackKeepConnection marks a semantic upstream failure whose
// pooled socket is still healthy. The caller must return the error to the
// client, but may release the lease without evicting the connection.
func wrapOpenAIWSFallbackKeepConnection(reason string, err error) error {
	return &openAIWSFallbackError{
		Reason:         strings.TrimSpace(reason),
		Err:            err,
		KeepConnection: true,
	}
}

func openAIWSFallbackKeepsConnection(err error) bool {
	var fallbackErr *openAIWSFallbackError
	return errors.As(err, &fallbackErr) && fallbackErr != nil && fallbackErr.KeepConnection
}

// OpenAIWSClientCloseError 表示应以指定 WebSocket close code 主动关闭客户端连接的错误。
type OpenAIWSClientCloseError struct {
	statusCode coderws.StatusCode
	reason     string
	err        error
}

type openAIWSIngressTurnError struct {
	stage                   string
	cause                   error
	wroteDownstream         bool
	wroteSemanticDownstream bool
}

type openAIWSCurrentTurnFailoverError struct {
	cause        error
	retryPayload []byte
}

func (e *openAIWSCurrentTurnFailoverError) Error() string {
	if e == nil || e.cause == nil {
		return "openai websocket current-turn failover"
	}
	return e.cause.Error()
}

func (e *openAIWSCurrentTurnFailoverError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newOpenAIWSCurrentTurnFailoverError(cause error, retryPayload []byte) error {
	return &openAIWSCurrentTurnFailoverError{
		cause:        cause,
		retryPayload: append([]byte(nil), retryPayload...),
	}
}

// OpenAIWSCurrentTurnRetryPayload returns an isolated copy of the payload that
// may be retried on a replacement account without replaying the first turn.
func OpenAIWSCurrentTurnRetryPayload(err error) ([]byte, bool) {
	var retryErr *openAIWSCurrentTurnFailoverError
	if !errors.As(err, &retryErr) || retryErr == nil {
		return nil, false
	}
	return append([]byte(nil), retryErr.retryPayload...), true
}

func (e *openAIWSIngressTurnError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause == nil {
		return strings.TrimSpace(e.stage)
	}
	return e.cause.Error()
}

func (e *openAIWSIngressTurnError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func wrapOpenAIWSIngressTurnError(stage string, cause error, wroteDownstream bool) error {
	return wrapOpenAIWSIngressTurnErrorWithSemantic(stage, cause, wroteDownstream, wroteDownstream)
}

func wrapOpenAIWSIngressTurnErrorWithSemantic(stage string, cause error, wroteDownstream, wroteSemanticDownstream bool) error {
	if cause == nil {
		return nil
	}
	return &openAIWSIngressTurnError{
		stage:                   strings.TrimSpace(stage),
		cause:                   cause,
		wroteDownstream:         wroteDownstream,
		wroteSemanticDownstream: wroteSemanticDownstream,
	}
}

func isOpenAIWSIngressTurnRetryable(err error) bool {
	var turnErr *openAIWSIngressTurnError
	if !errors.As(err, &turnErr) || turnErr == nil {
		return false
	}
	if errors.Is(turnErr.cause, context.Canceled) || errors.Is(turnErr.cause, context.DeadlineExceeded) {
		return false
	}
	if turnErr.wroteDownstream {
		return false
	}
	switch turnErr.stage {
	case "write_upstream", "read_upstream":
		return true
	default:
		return false
	}
}

// isOpenAIWS429GuardSwitchableTurnError accepts only upstream-side failures
// before a client-visible event has been committed. Client read/write errors,
// policy failures and a canceled request must stay on the normal close path.
func isOpenAIWS429GuardSwitchableTurnError(err error) bool {
	if err == nil {
		return false
	}
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		return failoverErr != nil
	}
	var turnErr *openAIWSIngressTurnError
	if !errors.As(err, &turnErr) || turnErr == nil || turnErr.wroteSemanticDownstream {
		return false
	}
	switch turnErr.stage {
	case "write_upstream":
		return true
	case "read_upstream":
		// A lifecycle preamble may already mean the upstream accepted the
		// request. Replaying after that point can duplicate a side effect even
		// when no token has reached the client yet.
		return !turnErr.wroteDownstream
	case "upstream_error_event", "upstream_response_failed", openAIWSIngressStagePreviousResponseNotFound:
		return true
	default:
		return false
	}
}

// isOpenAIWS429GuardConnectionActive reports whether the account has already
// crossed the explicit two-signal OAuth 429 confirmation threshold. A guard
// flag alone is not enough: the first 429 must still follow normal failover.
func (s *OpenAIGatewayService) isOpenAIWS429GuardConnectionActive(account *Account) bool {
	return s != nil && account != nil && account.Codex429GuardEnabled() &&
		account.IsOpenAIOAuth() && s.isOpenAI429GuardRuntimeBlocked(account)
}

// isOpenAIWS429GuardConnectionPinned is the connection-lifetime half of the
// guard. Once a confirmed 429 has pinned a pooled socket, the account cooldown
// may expire without making that socket eligible for ordinary cleanup or a
// different account. The pool remains the source of truth for liveness.
func (s *OpenAIGatewayService) isOpenAIWS429GuardConnectionPinned(account *Account, connID string) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	pool := s.getOpenAIWSConnPool()
	return pool != nil && pool.IsGuardConnPinned(account.ID, connID)
}

// isOpenAIWS429GuardConnectionCandidate is narrower than general pool reuse:
// it identifies only the socket that was already pooled when the confirmed
// 429 block began. It may receive the confirming event, but it is not retained
// until that event has been positively pinned.
func (s *OpenAIGatewayService) isOpenAIWS429GuardConnectionCandidate(account *Account, connID string, generations ...uint64) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	pool := s.getOpenAIWSConnPool()
	return pool != nil && pool.IsGuardConnCandidate(account.ID, connID, generations...)
}

// isOpenAIWS429GuardConnectionRetained allows a healthy, already-pinned old
// connection to keep serving after the short account-level 429 cooldown. A
// runtime block is still required before the first pin is created.
func (s *OpenAIGatewayService) isOpenAIWS429GuardConnectionRetained(account *Account, connID string) bool {
	if s.isOpenAIWS429GuardContinuationBlockedByNon429Runtime(account) {
		return false
	}
	if strings.TrimSpace(connID) != "" {
		return s.isOpenAIWS429GuardConnectionPinned(account, connID)
	}
	return s.isOpenAIWS429GuardConnectionActive(account)
}

// isOpenAIWS429GuardAccountActiveForLease keeps the acquire-time guard proof
// authoritative after the short runtime cooldown expires. A socket acquired
// during the guard window is still unproven until that exact lease observes the
// confirming 429 and must be evicted on a later non-rate-limit failure.
func (s *OpenAIGatewayService) isOpenAIWS429GuardAccountActiveForLease(account *Account, lease *openAIWSConnLease) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() || lease == nil {
		return false
	}
	return lease.openAI429GuardActiveAtAcquire || lease.openAI429GuardProven.Load() || s.isOpenAIWS429GuardConnectionActive(account)
}

// isOpenAIWS429GuardContinuationActiveForLease carries the acquire-time guard
// state through cooldown expiry. It is scoped to the current lease, so a later
// ordinary socket cannot inherit continuation failover behavior.
func (s *OpenAIGatewayService) isOpenAIWS429GuardContinuationActiveForLease(account *Account, lease *openAIWSConnLease, leaseRequested bool) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	return s.isOpenAIWS429GuardContinuationActive(account, leaseRequested) ||
		(lease != nil && (lease.openAI429GuardActiveAtAcquire || lease.openAI429GuardProven.Load()))
}

// isOpenAIWS429GuardContinuationActive keeps the connection-affinity lease
// separate from the live 429 retention state. A continuation selected while a
// confirmed block was active must still fail over safely if its socket breaks
// after the block expires; only rate-limit classification should consult the
// live runtime block directly.
func (s *OpenAIGatewayService) isOpenAIWS429GuardContinuationActive(account *Account, leaseRequested bool) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	return leaseRequested || s.isOpenAIWS429GuardConnectionActive(account)
}

// isOpenAIWS429GuardContinuationBlockedByNon429Runtime prevents a long-lived
// guarded ingress lease from bypassing a later auth/transport/admin block. A
// confirmed 429 is the sole runtime block that may keep the old socket alive;
// an empty reason is treated as non-429 so an unknown block fails closed.
func (s *OpenAIGatewayService) isOpenAIWS429GuardContinuationBlockedByNon429Runtime(account *Account) bool {
	if s == nil || account == nil {
		return false
	}
	snapshot := s.openAIAccountRuntimeBlockSnapshot(account.ID)
	return snapshot.Active && snapshot.Reason != "429"
}

// isOpenAIWS429GuardSwitchableAcquireError distinguishes a failed preferred
// connection from a healthy connection that is merely occupied. The latter
// must keep its binding so a concurrent turn cannot silently move accounts.
func isOpenAIWS429GuardSwitchableAcquireError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errOpenAIWSConnQueueFull) {
		return false
	}
	if errors.Is(err, errOpenAIWSPreferredConnUnavailable) || errors.Is(err, errOpenAIWSConnClosed) {
		return true
	}
	var dialErr *openAIWSDialError
	if errors.As(err, &dialErr) {
		return true
	}
	// A non-context error from the preferred-connection health probe is an
	// upstream transport failure and is eligible for immediate migration.
	return true
}

// isOpenAIWS429GuardReplaySafe reports whether removing a previous response
// identifier still leaves enough client-visible context to issue a new turn.
// Ordinary assistant history lives only upstream, so only a complete tool
// call/output pair is replayable from the local snapshot.
func isOpenAIWS429GuardReplaySafe(previousResponseID string, replayInput []json.RawMessage, replayInputExists bool) bool {
	if strings.TrimSpace(previousResponseID) == "" {
		return true
	}
	return replayInputExists &&
		openAIWSRawItemsHasFunctionCallOutput(replayInput) &&
		openAIWSRawItemsHaveToolCallContextForOutputs(replayInput)
}

// hasOpenAIWSVerifiedReplayHistory reports whether a continuation can be
// rebuilt from locally observed input rather than from an opaque upstream
// response id. Both snapshots must be non-empty, and the current replay must
// retain the prior local snapshot as its prefix.
func hasOpenAIWSVerifiedReplayHistory(
	previousInput []json.RawMessage,
	previousInputExists bool,
	currentInput []json.RawMessage,
	currentInputExists bool,
) bool {
	return previousInputExists && len(previousInput) > 0 &&
		currentInputExists && len(currentInput) > 0 &&
		openAIWSRawItemsHasPrefix(currentInput, previousInput)
}

func openAIWSIngressTurnRetryReason(err error) string {
	var turnErr *openAIWSIngressTurnError
	if !errors.As(err, &turnErr) || turnErr == nil {
		return "unknown"
	}
	if turnErr.stage == "" {
		return "unknown"
	}
	return turnErr.stage
}

func isOpenAIWSIngressPreviousResponseNotFound(err error) bool {
	var turnErr *openAIWSIngressTurnError
	if !errors.As(err, &turnErr) || turnErr == nil {
		return false
	}
	if strings.TrimSpace(turnErr.stage) != openAIWSIngressStagePreviousResponseNotFound {
		return false
	}
	return !turnErr.wroteSemanticDownstream
}

// NewOpenAIWSClientCloseError 创建一个客户端 WS 关闭错误。
func NewOpenAIWSClientCloseError(statusCode coderws.StatusCode, reason string, err error) error {
	return &OpenAIWSClientCloseError{
		statusCode: statusCode,
		reason:     strings.TrimSpace(reason),
		err:        err,
	}
}

func (e *OpenAIWSClientCloseError) Error() string {
	if e == nil {
		return ""
	}
	if e.err == nil {
		return fmt.Sprintf("openai ws client close: %d %s", int(e.statusCode), strings.TrimSpace(e.reason))
	}
	return fmt.Sprintf("openai ws client close: %d %s: %v", int(e.statusCode), strings.TrimSpace(e.reason), e.err)
}

func (e *OpenAIWSClientCloseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *OpenAIWSClientCloseError) StatusCode() coderws.StatusCode {
	if e == nil {
		return coderws.StatusInternalError
	}
	return e.statusCode
}

func (e *OpenAIWSClientCloseError) Reason() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.reason)
}

// OpenAIWSIngressHooks 定义入站 WS 每个 turn 的生命周期回调。
type OpenAIWSIngressHooks struct {
	// ClientLifecycleContext is the request context before an ingress lease
	// adds its independent cancellation signal. Downstream writes bind to it
	// so shutdown and disconnect cancellation remain direct during lease loss.
	ClientLifecycleContext context.Context
	// InitialRequestModel is the client-facing model from the first frame,
	// before channel or account mapping. Ingress modes preserve it for usage
	// attribution while MapRequestModel determines the upstream model.
	InitialRequestModel string
	// SessionHash is resolved by the authenticated handler for this client
	// WebSocket. It is used only when a frame lacks an explicit session signal,
	// so content-only/model-only frames cannot borrow another client's socket.
	SessionHash string
	// Force429GuardContinuation makes the first turn use the exact pooled
	// connection associated with a 429-guard continuation lease. The pool
	// verifies that connection atomically instead of silently opening another
	// connection for the rate-limited account.
	Force429GuardContinuation bool
	// InitialTurnStartedAt freezes when the first response.create was accepted.
	InitialTurnStartedAt time.Time
	// MaxReasoningEffort limits explicit reasoning effort values for this WS session.
	MaxReasoningEffort string
	// MaxReasoningEffortOverLimit is the access control when an explicit effort
	// exceeds the ceiling: downgrade (default) or deny.
	MaxReasoningEffortOverLimit string
	// ReasoningEffortMappings rewrites explicit effort values for this WS session.
	ReasoningEffortMappings []ReasoningEffortMapping
	// InitialPassthroughFrames contains client prelude frames read before the
	// first response.create (for example conversation.item.create). The handler
	// buffers these so the service can audit and forward them in order without
	// starting a relay turn before a response exists.
	InitialPassthroughFrames []OpenAIWSPassthroughInitialFrame
	// InitialResponseMessageType preserves the client frame encoding for the
	// first response.create when the direct relay writes it upstream.
	InitialResponseMessageType coderws.MessageType
	// TurnStarted reports the accepted start time for each direct relay turn.
	// Passthrough uses it for per-turn usage pricing without running the
	// pooled-ingress admission hook again.
	TurnStarted func(turn int, startedAt time.Time)
	BeforeTurn  func(turn int) error
	// BeforePassthroughTurn runs only for an admitted follow-up turn in the
	// direct upstream WebSocket relay. The normal BeforeTurn hook owns the
	// pooled ingress lifecycle; passthrough needs this separate callback to
	// reacquire per-turn resources after the prior terminal event released them.
	BeforePassthroughTurn func(turn int) error
	BeforeRequest         func(turn int, payload []byte, originalModel string) error
	// MapRequestModel resolves the current turn's client model to the model
	// that must be written into the upstream response.create frame.
	MapRequestModel func(turn int, originalModel string) (string, error)
	AfterTurn       func(turn int, result *OpenAIForwardResult, turnErr error)
}

// OpenAIWSPassthroughInitialFrame is a client frame buffered before the first
// response.create in a Responses WebSocket session.
type OpenAIWSPassthroughInitialFrame struct {
	MessageType coderws.MessageType
	Payload     []byte
}

// OpenAIWSResumeState carries a fully rebuilt, not-yet-client-visible turn
// from a failed pooled WebSocket connection back to the WebSocket handler.
// The handler may select another account and send ReplayPayload once; callers
// must leave this nil when the original turn wrote any downstream bytes.
type OpenAIWSResumeState struct {
	ReplayPayload []byte
	// OriginalModel is the client-facing model before the failed account's
	// channel/account mapping. A replacement account must re-run mapping from
	// this value rather than treating the old upstream model as client input.
	OriginalModel      string
	SessionHash        string
	PreviousResponseID string
	FailedAccountID    int64
	FailedConnID       string
	Turn               int
}

func (s *OpenAIGatewayService) getOpenAIWSConnPool() *openAIWSConnPool {
	if s == nil {
		return nil
	}
	s.openaiWSPoolOnce.Do(func() {
		if s.openaiWSPool == nil {
			s.openaiWSPool = newOpenAIWSConnPool(s.cfg)
		}
	})
	pool := s.openaiWSPool
	if pool != nil {
		s.openaiWSPoolRef.Store(pool)
		pool.setGuardBindingInvalidator(func(accountID int64, connID string) {
			store := s.getOpenAIWSStateStore()
			if invalidator, ok := store.(openAIWSConnectionBindingInvalidator); ok {
				invalidator.invalidateConnectionBindings(accountID, connID)
			}
		})
	}
	return pool
}

// existingOpenAIWSConnPool returns an already initialized pool without
// starting WebSocket workers solely to invalidate a connection that does not
// exist. Every real guard pin flows through getOpenAIWSConnPool first.
func (s *OpenAIGatewayService) existingOpenAIWSConnPool() *openAIWSConnPool {
	if s == nil {
		return nil
	}
	return s.openaiWSPoolRef.Load()
}

func (s *OpenAIGatewayService) getOpenAIWSPassthroughDialer() openAIWSClientDialer {
	if s == nil {
		return nil
	}
	s.openaiWSPassthroughDialerOnce.Do(func() {
		if s.openaiWSPassthroughDialer == nil {
			s.openaiWSPassthroughDialer = newDefaultOpenAIWSClientDialer()
		}
	})
	return s.openaiWSPassthroughDialer
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSPoolMetrics() OpenAIWSPoolMetricsSnapshot {
	pool := s.getOpenAIWSConnPool()
	if pool == nil {
		return OpenAIWSPoolMetricsSnapshot{}
	}
	return pool.SnapshotMetrics()
}

type OpenAIWSPerformanceMetricsSnapshot struct {
	Pool      OpenAIWSPoolMetricsSnapshot      `json:"pool"`
	Retry     OpenAIWSRetryMetricsSnapshot     `json:"retry"`
	Transport OpenAIWSTransportMetricsSnapshot `json:"transport"`
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSPerformanceMetrics() OpenAIWSPerformanceMetricsSnapshot {
	pool := s.getOpenAIWSConnPool()
	snapshot := OpenAIWSPerformanceMetricsSnapshot{
		Retry: s.SnapshotOpenAIWSRetryMetrics(),
	}
	if pool == nil {
		return snapshot
	}
	snapshot.Pool = pool.SnapshotMetrics()
	snapshot.Transport = pool.SnapshotTransportMetrics()
	return snapshot
}

func (s *OpenAIGatewayService) getOpenAIWSStateStore() OpenAIWSStateStore {
	if s == nil {
		return nil
	}
	s.openaiWSStateStoreOnce.Do(func() {
		if s.openaiWSStateStore == nil {
			s.openaiWSStateStore = NewOpenAIWSStateStore(s.cache)
		}
	})
	return s.openaiWSStateStore
}

func (s *OpenAIGatewayService) openAIWSResponseStickyTTL() time.Duration {
	if s != nil && s.cfg != nil {
		seconds := s.cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Hour
}

func (s *OpenAIGatewayService) openAIWSIngressPreviousResponseRecoveryEnabled() bool {
	if s != nil && s.cfg != nil {
		return s.cfg.Gateway.OpenAIWS.IngressPreviousResponseRecoveryEnabled
	}
	return true
}

func (s *OpenAIGatewayService) openAIWSReadTimeout() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds) * time.Second
	}
	return 15 * time.Minute
}

func (s *OpenAIGatewayService) openAIWSPassthroughIdleTimeout() time.Duration {
	if timeout := s.openAIWSReadTimeout(); timeout > 0 {
		return timeout
	}
	return openAIWSPassthroughIdleTimeoutDefault
}

func (s *OpenAIGatewayService) openAIWSWriteTimeout() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.WriteTimeoutSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.WriteTimeoutSeconds) * time.Second
	}
	return 2 * time.Minute
}

func (s *OpenAIGatewayService) openAIWSEventFlushBatchSize() int {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.EventFlushBatchSize > 0 {
		return s.cfg.Gateway.OpenAIWS.EventFlushBatchSize
	}
	return openAIWSEventFlushBatchSizeDefault
}

func (s *OpenAIGatewayService) openAIWSEventFlushInterval() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.EventFlushIntervalMS >= 0 {
		if s.cfg.Gateway.OpenAIWS.EventFlushIntervalMS == 0 {
			return 0
		}
		return time.Duration(s.cfg.Gateway.OpenAIWS.EventFlushIntervalMS) * time.Millisecond
	}
	return openAIWSEventFlushIntervalDefault
}

func (s *OpenAIGatewayService) openAIWSPayloadLogSampleRate() float64 {
	if s != nil && s.cfg != nil {
		rate := s.cfg.Gateway.OpenAIWS.PayloadLogSampleRate
		if rate < 0 {
			return 0
		}
		if rate > 1 {
			return 1
		}
		return rate
	}
	return openAIWSPayloadLogSampleDefault
}

func (s *OpenAIGatewayService) shouldLogOpenAIWSPayloadSchema(attempt int) bool {
	// 首次尝试保留一条完整 payload_schema 便于排障。
	if attempt <= 1 {
		return true
	}
	rate := s.openAIWSPayloadLogSampleRate()
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	return rand.Float64() < rate
}

func (s *OpenAIGatewayService) shouldEmitOpenAIWSPayloadSchema(attempt int) bool {
	if !s.shouldLogOpenAIWSPayloadSchema(attempt) {
		return false
	}
	return logger.L().Core().Enabled(zap.DebugLevel)
}

func (s *OpenAIGatewayService) openAIWSDialTimeout() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.DialTimeoutSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.DialTimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}

func (s *OpenAIGatewayService) openAIWSAcquireTimeout() time.Duration {
	// Acquire 覆盖“连接复用命中/排队/新建连接”三个阶段。
	// 这里不再叠加 write_timeout，避免高并发排队时把 TTFT 长尾拉到分钟级。
	dial := s.openAIWSDialTimeout()
	if dial <= 0 {
		dial = 10 * time.Second
	}
	return dial + 2*time.Second
}
