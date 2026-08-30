package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	openAIAccountStateUpdateTimeout       = 5 * time.Second
	openAIOAuth429FallbackCooldown        = 5 * time.Second
	openAIOAuth429ConfirmationWindow      = 30 * time.Second
	openAIOAuth429RetryWindow             = 2 * time.Minute
	openAIOAuth429RetryDelay              = 500 * time.Millisecond
	openAIOAuth429MaxRetryDelay           = 8 * time.Second
	openAIOAuth429MaxAccountAttempts      = 3
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormMaxAccountSwitches = 1
)

type openAIOAuth429StreakState struct {
	Count              int
	UpdatedAt          time.Time
	RemoteResetPending bool
}

// openAIRuntimeBlockSnapshot is read under the per-account runtime lock. The
// generation lets WebSocket acquisition distinguish a socket obtained before
// a block transition from one obtained while a block was already in force.
type openAIRuntimeBlockSnapshot struct {
	Generation uint64
	Until      time.Time
	Reason     string
	Active     bool
}

type openAIOAuth429ConfirmedContextKey struct{}

func withOpenAIOAuth429Confirmed(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIOAuth429ConfirmedContextKey{}, true)
}

func openAIOAuth429AlreadyConfirmed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	confirmed, _ := ctx.Value(openAIOAuth429ConfirmedContextKey{}).(bool)
	return confirmed
}

// OpenAIOAuth429FailoverState tracks the request-local follow-up budget after
// the first Grok OAuth 429. Once that 429 occurs, exactly one different account
// may be attempted; any failure from that follow-up account ends failover.
type OpenAIOAuth429FailoverState struct {
	grokOAuth429FollowupPending bool
}

type openAIOAuth429Disposition uint8

const (
	openAIOAuth429Transient openAIOAuth429Disposition = iota
	openAIOAuth429Quota5h
	openAIOAuth429Quota7d
	openAIOAuth429QuotaReset
)

// classifyOpenAIOAuth429 区分账号配额耗尽信号与普通瞬时 429。明确窗口达到
// 100% 时以该窗口为准；两个窗口都未耗尽时，响应头中的长窗口 reset 值不再
// 被当作账号冷却（这类 429 通常是短暂 RPM/TPM burst）。
func classifyOpenAIOAuth429(headers http.Header, responseBody []byte) (openAIOAuth429Disposition, *time.Time) {
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		if normalized := snapshot.Normalize(); normalized != nil {
			if normalized.Used7dPercent != nil && *normalized.Used7dPercent >= 100 {
				if normalized.Reset7dSeconds != nil {
					now := time.Now()
					resetAt := now.Add(time.Duration(*normalized.Reset7dSeconds) * time.Second)
					return openAIOAuth429Quota7d, &resetAt
				}
				return openAIOAuth429Quota7d, nil
			}
			if normalized.Used5hPercent != nil && *normalized.Used5hPercent >= 100 {
				if normalized.Reset5hSeconds != nil {
					now := time.Now()
					resetAt := now.Add(time.Duration(*normalized.Reset5hSeconds) * time.Second)
					return openAIOAuth429Quota5h, &resetAt
				}
				return openAIOAuth429Quota5h, nil
			}
		}
	}
	if resetAt := calculateOpenAI429ResetTime(headers); resetAt != nil {
		return openAIOAuth429QuotaReset, resetAt
	}
	if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
		resetAt := time.Unix(*resetUnix, 0)
		return openAIOAuth429QuotaReset, &resetAt
	}
	return openAIOAuth429Transient, nil
}

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike()
}

func isGrokOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformGrok && account.Type == AccountTypeOAuth
}

func isOpenAIAccount(account *Account) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.Platform == PlatformGrok)
}

// handleOpenAIAccountUpstreamError expects canonicalModel to be the model used
// for scheduling after applying account mapping exactly once.
func (s *OpenAIGatewayService) handleOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, canonicalModel ...string) bool {
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(statusCode, responseBody) {
		return false
	}
	// Any non-2xx upstream HTTP response means the model request was actually sent.
	if s != nil {
		scheduleOllamaCloudUsageActivity(s.deferredService, account)
	}
	// Capacity shedding describes this request, not account health. Keep the
	// account schedulable while the request-local retry budget handles recovery.
	if account != nil && account.Platform == PlatformOpenAI && isOpenAIRequestScopedCapacityShed("", responseBody) {
		return false
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	if account != nil && account.Platform == PlatformOpenAI && isOpenAIHTTPUpstreamAccessStateError(statusCode, "", responseBody) {
		message := "OpenAI upstream account or workspace is unavailable"
		if upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(responseBody)); upstreamMsg != "" {
			message = upstreamMsg
		}
		if s != nil && s.rateLimitService != nil {
			s.rateLimitService.handleAuthError(stateCtx, account, message)
		}
		if s != nil {
			s.BlockAccountScheduling(account, time.Time{}, "openai_access_state")
		}
		return true
	}

	if account != nil && account.Platform == PlatformOpenAI && isOpenAIContextWindowError("", responseBody) {
		return false
	}

	if isOpenAIImageRateLimitError(statusCode, responseBody) {
		if s != nil && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageRateLimit(stateCtx, account, statusCode, headers, responseBody)
		}
		return false
	}

	// Self-built images requests always carry a matching image_generation tool, so a
	// "tool choice not found in 'tools'" 400 means upstream revoked this account's
	// image capability. Gated on the self-built marker: passthrough clients control
	// their own tools/tool_choice and could otherwise poison a healthy account.
	if isOpenAIImagesSelfBuiltRequest(ctx) && isOpenAIImageCapabilityLossError(statusCode, responseBody) {
		if s != nil && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageCapabilityLoss(stateCtx, account, statusCode, responseBody)
		}
		return false
	}

	if s == nil || account == nil {
		return false
	}
	if s.rateLimitService != nil {
		// Keep the fast path consistent with the regular rate-limit handler. The
		// helper is idempotent within its short workspace window.
		s.rateLimitService.maybeHandleOpenAITeamLinkedError(stateCtx, account, statusCode, responseBody)
	}
	stateCtx = withTempUnschedulableModel(stateCtx, canonicalModel)
	if s.rateLimitService != nil && len(canonicalModel) > 0 && s.rateLimitService.HandleUpstreamModelNotFound(stateCtx, account, canonicalModel[0], statusCode, responseBody) {
		return true
	}
	if statusCode == http.StatusTooManyRequests && s.rateLimitService != nil && len(canonicalModel) > 0 &&
		s.rateLimitService.HandleOpenAICodexSparkRateLimit(stateCtx, account, canonicalModel[0], statusCode, headers, responseBody) {
		// Spark's quota is model-scoped; keep the account available for other
		// Codex models and let the scheduler skip only this model until reset.
		return false
	}
	confirmedOAuth429 := false
	if statusCode == http.StatusTooManyRequests && isOpenAIOAuthAccount(account) &&
		!account.IsShadow() && account.QuotaDimensionOrDefault() != QuotaDimensionSpark {
		if !s.confirmOpenAIOAuth429Context(stateCtx, account.ID, time.Now()) {
			repo := s.accountRepo
			if repo == nil && s.rateLimitService != nil {
				repo = s.rateLimitService.accountRepo
			}
			persistOpenAI429PlanType(stateCtx, repo, account, responseBody)
			persistOpenAICodexSnapshotWithRepo(stateCtx, repo, account, headers)
			return false
		}
		stateCtx = withOpenAIOAuth429Confirmed(stateCtx)
		confirmedOAuth429 = true
	}
	// Isolate a custom temporary-unschedulable match to the known upstream
	// model before entering the generic account error path. This keeps the
	// account available to other models and avoids the account runtime blocker.
	if s.rateLimitService != nil && statusCode != http.StatusUnauthorized && len(canonicalModel) > 0 && strings.TrimSpace(canonicalModel[0]) != "" &&
		s.rateLimitService.HandleTempUnschedulable(stateCtx, account, statusCode, responseBody, canonicalModel[0]) {
		return true
	}
	if confirmedOAuth429 {
		s.markOpenAIOAuth429RateLimited(stateCtx, account, headers, responseBody)
	}
	if s.rateLimitService == nil {
		return false
	}
	shouldDisable := s.rateLimitService.HandleUpstreamError(stateCtx, account, statusCode, headers, responseBody)
	modelTempMatched := statusCode != http.StatusUnauthorized && tempUnschedulableModel(stateCtx, nil) != "" &&
		len(matchTempUnschedulableRules(account, statusCode, responseBody)) > 0
	if shouldDisable && !modelTempMatched {
		s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
	}
	// Pool-mode retryable upstream errors are already bounded by the request-local
	// same-account retry budget. Recording the generic account+model transient
	// cooldown here would block the next approved retry before that budget is used.
	poolModeRetryable := account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	if !shouldDisable && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		shouldCooldownOpenAITransientUpstreamError(statusCode, responseBody) && !poolModeRetryable {
		model := ""
		if len(canonicalModel) > 0 {
			model = canonicalModel[0]
		}
		decision := s.recordOpenAIAccountModelTransientFailure(account, model, time.Now())
		if decision.FailureStreak > 0 {
			slog.Warn("openai_model_transient_state",
				"account_id", account.ID,
				"model", openAIAccountModelTransientModel(model),
				"failure_streak", decision.FailureStreak,
				"cooldown_ms", decision.Cooldown.Milliseconds(),
				"block_scope", "account_model",
			)
		}
	}
	return shouldDisable
}

// confirmOpenAIOAuth429 requires two explicit upstream 429 responses for the
// same Codex OAuth account within a short window before account-level cooldown
// is persisted. The first response still fails the current request and allows
// normal failover, but it does not poison future scheduling by itself.
func (s *OpenAIGatewayService) confirmOpenAIOAuth429(accountID int64, now time.Time) bool {
	return s.confirmOpenAIOAuth429Context(context.Background(), accountID, now)
}

func (s *OpenAIGatewayService) confirmOpenAIOAuth429Context(ctx context.Context, accountID int64, now time.Time) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	if s.rateLimitService != nil {
		return s.rateLimitService.confirmOpenAIOAuth429Context(ctx, accountID, now)
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()

	state := openAIOAuth429StreakState{}
	if raw, ok := s.openaiOAuth429Streak.Load(accountID); ok {
		state, _ = raw.(openAIOAuth429StreakState)
	}
	if state.UpdatedAt.IsZero() || now.Sub(state.UpdatedAt) > openAIOAuth429ConfirmationWindow || now.Before(state.UpdatedAt) {
		state.Count = 0
	}
	state.Count++
	state.UpdatedAt = now
	if state.Count >= 2 {
		s.openaiOAuth429Streak.Delete(accountID)
		return true
	}
	s.openaiOAuth429Streak.Store(accountID, state)
	return false
}

func (s *OpenAIGatewayService) clearOpenAIOAuth429Streak(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	// Reset the distributed mirror before taking the local runtime lock. A
	// concurrent confirmation that starts after this reset belongs to the
	// fresh generation and must not be erased by a late local cleanup.
	if s.rateLimitService != nil {
		s.rateLimitService.clearOpenAIOAuth429Streak(accountID)
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	s.openaiOAuth429Streak.Delete(accountID)
}

// openAIAccountRuntimeBlockSnapshot returns a coherent reason/generation pair
// for one account. Expired entries are retired while holding the same lock used
// by BlockAccountScheduling and ClearAccountSchedulingBlock, so an acquire
// cannot observe a half-cleared block.
func (s *OpenAIGatewayService) openAIAccountRuntimeBlockSnapshot(accountID int64) openAIRuntimeBlockSnapshot {
	if s == nil || accountID <= 0 {
		return openAIRuntimeBlockSnapshot{}
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	return s.openAIAccountRuntimeBlockSnapshotLocked(accountID)
}

// openAIAccountRuntimeBlockSnapshotLocked is the lock-held implementation of
// openAIAccountRuntimeBlockSnapshot. Callers that need to act on the snapshot
// and mutate pool state must keep the account runtime lock until that action is
// complete, so a clear or non-429 transition cannot invalidate the decision.
func (s *OpenAIGatewayService) openAIAccountRuntimeBlockSnapshotLocked(accountID int64) openAIRuntimeBlockSnapshot {
	snapshot := openAIRuntimeBlockSnapshot{}
	if raw, ok := s.openaiAccountRuntimeBlockGeneration.Load(accountID); ok {
		snapshot.Generation, _ = raw.(uint64)
	}
	if raw, ok := s.openaiAccountRuntimeBlockReason.Load(accountID); ok {
		snapshot.Reason = strings.TrimSpace(fmt.Sprint(raw))
	}
	rawUntil, ok := s.openaiAccountRuntimeBlockUntil.Load(accountID)
	if !ok {
		s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(accountID)
		s.openaiAccountRuntimeBlockInstalledAt.Delete(accountID)
		return snapshot
	}
	until, ok := rawUntil.(time.Time)
	if !ok || until.IsZero() || !time.Now().Before(until) {
		s.openaiAccountRuntimeBlockUntil.Delete(accountID)
		s.openaiAccountRuntimeBlockReason.Delete(accountID)
		s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(accountID)
		s.openaiAccountRuntimeBlockInstalledAt.Delete(accountID)
		snapshot.Generation = s.openaiAccountRuntimeBlockSequence.Add(1)
		s.openaiAccountRuntimeBlockGeneration.Store(accountID, snapshot.Generation)
		snapshot.Reason = ""
		return snapshot
	}
	snapshot.Until = until
	snapshot.Active = true
	return snapshot
}

// stampOpenAIWSLeaseRuntimeBlockState records a coherent block view around a
// pool acquire. Treating any active block, or a generation transition during
// acquire, as guarded keeps failure handling conservative. Exact-socket proof
// is separately enforced by the pool's pre-block candidate marker.
func (s *OpenAIGatewayService) stampOpenAIWSLeaseRuntimeBlockState(
	accountID int64,
	lease *openAIWSConnLease,
	before openAIRuntimeBlockSnapshot,
) {
	if s == nil || lease == nil {
		return
	}
	after := s.openAIAccountRuntimeBlockSnapshot(accountID)
	lease.openAIRuntimeBlockGeneration = after.Generation
	lease.openAI429GuardActiveAtAcquire = before.Active || after.Active || before.Generation != after.Generation
	if pool := s.getOpenAIWSConnPool(); pool != nil && pool.IsGuardConnPinned(accountID, lease.ConnID()) {
		lease.openAI429GuardProven.Store(true)
	}
}

// markOpenAI429GuardConnectionProof records positive evidence that this exact
// pooled socket observed the confirming OAuth 429. Ordinary response/session
// bindings are deliberately insufficient: a socket opened after the block
// must never be promoted into the permanent guard connection later.
func (s *OpenAIGatewayService) markOpenAI429GuardConnectionProof(account *Account, lease *openAIWSConnLease) bool {
	if s == nil || account == nil || lease == nil || !account.Codex429GuardEnabled() ||
		!account.IsOpenAIOAuth() ||
		!s.isOpenAI429GuardPooledWSMode(account) {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	snapshot := s.openAIAccountRuntimeBlockSnapshotLocked(account.ID)
	if !snapshot.Active || snapshot.Reason != "429" || snapshot.Generation == 0 {
		return false
	}
	pool := s.getOpenAIWSConnPool()
	if pool == nil {
		return false
	}
	return pool.MarkAndPinGuardConnConfirmed(account.ID, lease.ConnID(), snapshot.Generation)
}

func shouldCooldownOpenAITransientUpstreamError(statusCode int, responseBody []byte) bool {
	switch statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return true
	case http.StatusBadRequest:
		return isOpenAITransientProcessingError(statusCode, "", responseBody)
	default:
		return false
	}
}

func (s *OpenAIGatewayService) markOpenAIOAuth429RateLimited(ctx context.Context, account *Account, headers http.Header, responseBody []byte) {
	if s == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	// Spark 影子：不按 /responses 429 的 global x-codex-* 信号做内存运行时熔断(同 handle429,外审第8轮 P1)。
	// 同时避免把 spark 的 429 计入全局 429 storm 计数(recordOpenAIOAuth429),否则会误伤母账号 failover 决策。
	if account.IsShadow() || account.QuotaDimensionOrDefault() == QuotaDimensionSpark {
		return
	}
	s.recordOpenAIOAuth429()
	disposition, resetAt := classifyOpenAIOAuth429(headers, responseBody)
	_ = disposition
	// A confirmed two-strike 429 must freeze immediately. The official same-account
	// retry window only delays an unconfirmed mark (stream/direct callers).
	if !openAIOAuth429AlreadyConfirmed(ctx) && s.openAIOAuth429RetryWindowActive(account) {
		return
	}

	now := time.Now()
	// A confirmed two-strike 429 may still carry an authoritative exhausted
	// quota-window reset.  Preserve that reset even when the short fallback
	// cooldown switch is disabled; the switch only controls synthetic cooldowns
	// for transient 429s.  This keeps an actually exhausted account out of
	// rotation without reviving the old "disabled still cools" bug.
	if resetAt != nil && resetAt.After(now) {
		s.BlockAccountScheduling(account, *resetAt, "429")
		s.openaiOAuth429RetryStartedAt.Delete(account.ID)
		return
	}
	if s.rateLimitService != nil {
		cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account)
		if !ok || cooldown <= 0 {
			s.openaiOAuth429RetryStartedAt.Delete(account.ID)
			return
		}
		cooldownUntil := now.Add(cooldown)
		s.BlockAccountScheduling(account, cooldownUntil, "429")
		s.openaiOAuth429RetryStartedAt.Delete(account.ID)
		return
	}
	s.BlockAccountScheduling(account, now.Add(openAIOAuth429FallbackCooldown), "429")
	s.openaiOAuth429RetryStartedAt.Delete(account.ID)
}

func (s *OpenAIGatewayService) shouldRetryOpenAIOAuth429OnSameAccount(account *Account, statusCode int, shouldDisable bool) bool {
	return s.shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account, statusCode, shouldDisable, nil, nil)
}

func (s *OpenAIGatewayService) shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account *Account, statusCode int, shouldDisable bool, headers http.Header, responseBody []byte) bool {
	if shouldDisable || statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return false
	}
	_ = headers
	_ = responseBody
	// markOpenAIOAuth429RateLimited parks the account once the window expires.
	// Do not accidentally create a fresh window after that transition.
	if s.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	return s.openAIOAuth429RetryWindowActive(account)
}

// ShouldRetryOpenAIOAuth429 lets RateLimitService defer persistent account
// cooldown until the gateway's same-account retry window is exhausted.
func (s *OpenAIGatewayService) ShouldRetryOpenAIOAuth429(account *Account, headers http.Header, responseBody []byte) bool {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() || s.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	_ = headers
	_ = responseBody
	return s.openAIOAuth429RetryWindowActive(account)
}

func (s *OpenAIGatewayService) openAIOAuth429RetryWindowActive(account *Account) bool {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return false
	}
	now := time.Now()
	value, _ := s.openaiOAuth429RetryStartedAt.LoadOrStore(account.ID, now)
	startedAt, ok := value.(time.Time)
	if !ok {
		s.openaiOAuth429RetryStartedAt.Store(account.ID, now)
		startedAt = now
	}
	return now.Before(startedAt.Add(openAIOAuth429RetryWindow))
}

func (s *OpenAIGatewayService) openAIOAuth429RetryDeadline(account *Account) time.Time {
	if s == nil || !isOpenAIOAuthAccount(account) || account.IsShadow() {
		return time.Time{}
	}
	value, ok := s.openaiOAuth429RetryStartedAt.Load(account.ID)
	if !ok {
		return time.Time{}
	}
	startedAt, ok := value.(time.Time)
	if !ok {
		return time.Time{}
	}
	return startedAt.Add(openAIOAuth429RetryWindow)
}

func openAIOAuth429SameAccountRetryDelay(headers http.Header, deadline time.Time) time.Duration {
	delay := openAIOAuth429RetryDelay
	now := time.Now()
	if resetAt := parseRetryAfterResetTime(headers, now); resetAt != nil && resetAt.After(now) {
		delay = resetAt.Sub(now)
	}
	if delay > openAIOAuth429MaxRetryDelay {
		delay = openAIOAuth429MaxRetryDelay
	}
	if remaining := time.Until(deadline); !deadline.IsZero() && delay > remaining {
		delay = remaining
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func (s *OpenAIGatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if s == nil || !isOpenAIAccount(account) {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	_, _ = s.blockAccountSchedulingLocked(account, until, reason)
	var guardConns []*openAIWSConn
	if strings.TrimSpace(reason) != "429" {
		snapshot := s.openAIAccountRuntimeBlockSnapshotLocked(account.ID)
		if snapshot.Active && snapshot.Reason != "429" {
			if pool := s.existingOpenAIWSConnPool(); pool != nil {
				guardConns = pool.detachGuardConns(account.ID)
			}
		}
	}
	mu.Unlock()
	// A non-429 account failure invalidates the old guarded socket. Do this
	// after releasing the runtime lock because closing a websocket invokes the
	// local binding invalidator, which may perform its own synchronization. The
	// socket was already detached under the same runtime lock, so it cannot race
	// with a fresh 429 guard generation.
	closeOpenAIWSConns(guardConns)
}

// shouldMarkOpenAI429GuardCandidatesLocked recognizes the first transition
// into a confirmed Codex OAuth 429 block. It intentionally does not mark
// sockets when a different active block is overwritten, nor when an existing
// 429 block is merely extended: either case could promote a socket opened
// after the original 429 transition.
func (s *OpenAIGatewayService) shouldMarkOpenAI429GuardCandidatesLocked(account *Account, reason string) bool {
	if s == nil || account == nil || strings.TrimSpace(reason) != "429" ||
		!account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	current, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return true
	}
	until, ok := current.(time.Time)
	return !ok || until.IsZero() || !time.Now().Before(until)
}

func (s *OpenAIGatewayService) openAIAccountRuntimeBlockLock(accountID int64) *sync.Mutex {
	actual, _ := s.openaiAccountRuntimeBlockLocks.LoadOrStore(accountID, &sync.Mutex{})
	mu, ok := actual.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
		s.openaiAccountRuntimeBlockLocks.Store(accountID, mu)
	}
	return mu
}

func (s *OpenAIGatewayService) blockAccountSchedulingLocked(account *Account, until time.Time, reason string) (uint64, bool) {
	if s == nil || account == nil || account.ID <= 0 {
		return 0, false
	}
	now := time.Now()
	blockUntil := until
	if blockUntil.IsZero() || !blockUntil.After(now) {
		blockUntil = now.Add(openAIStopSchedulingBridgeCooldown)
	}

	currentRaw, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	currentUntil, validUntil := currentRaw.(time.Time)
	active := loaded && validUntil && !currentUntil.IsZero() && now.Before(currentUntil)
	currentReason := ""
	if rawReason, ok := s.openaiAccountRuntimeBlockReason.Load(account.ID); ok {
		currentReason = strings.TrimSpace(fmt.Sprint(rawReason))
	}
	currentGeneration := uint64(0)
	if rawGeneration, ok := s.openaiAccountRuntimeBlockGeneration.Load(account.ID); ok {
		currentGeneration, _ = rawGeneration.(uint64)
	}

	if active {
		// Preserve the row version captured when this block first appeared. A
		// later scheduler snapshot with a newer UpdatedAt then proves that a
		// durable writer (or an operator clear on another node) was observed.
		if observedRaw, present := s.openaiAccountRuntimeBlockObservedUpdatedAt.Load(account.ID); !present {
			s.openaiAccountRuntimeBlockObservedUpdatedAt.Store(account.ID, account.UpdatedAt)
		} else if observed, ok := observedRaw.(time.Time); ok &&
			!account.UpdatedAt.IsZero() && (observed.IsZero() || account.UpdatedAt.After(observed)) {
			// A newer account snapshot means the previous block generation was
			// observed after a durable write/clear. Treat this as a fresh local
			// persistence epoch so a failed subsequent write remains fail-closed.
			s.openaiAccountRuntimeBlockObservedUpdatedAt.Store(account.ID, account.UpdatedAt)
		}
		incomingReason := strings.TrimSpace(reason)
		// A confirmed non-429 block is stronger than a later 429 signal until
		// the account is explicitly cleared. Never relabel the same active
		// interval as a 429 guard, otherwise a stale socket could bypass the
		// transport/auth failure.
		if currentReason != "429" && incomingReason == "429" {
			if currentGeneration == 0 {
				currentGeneration = s.openaiAccountRuntimeBlockSequence.Add(1)
				s.openaiAccountRuntimeBlockGeneration.Store(account.ID, currentGeneration)
			}
			return currentGeneration, false
		}
		// Extending the same block does not create a new epoch. In particular,
		// a second 429 may arrive before the old socket has observed the first
		// one; its candidate proof must still match the active guard epoch.
		extends := blockUntil.After(currentUntil)
		s.storeOpenAIAccountRuntimeBlockReason(account.ID, incomingReason)
		nextReason := ""
		if rawReason, ok := s.openaiAccountRuntimeBlockReason.Load(account.ID); ok {
			nextReason = strings.TrimSpace(fmt.Sprint(rawReason))
		}
		reasonChanged := nextReason != currentReason
		if !extends && !reasonChanged {
			if currentGeneration == 0 {
				currentGeneration = s.openaiAccountRuntimeBlockSequence.Add(1)
				s.openaiAccountRuntimeBlockGeneration.Store(account.ID, currentGeneration)
			}
			return currentGeneration, false
		}
		if currentGeneration == 0 || reasonChanged {
			currentGeneration = s.openaiAccountRuntimeBlockSequence.Add(1)
			s.openaiAccountRuntimeBlockGeneration.Store(account.ID, currentGeneration)
		}
		if extends {
			s.openaiAccountRuntimeBlockUntil.Store(account.ID, blockUntil)
		}
		// Record the wall-clock installation point for the generation.  The
		// scheduler observer uses it to distinguish a newer request racing an
		// older cross-instance clear event.
		s.openaiAccountRuntimeBlockInstalledAt.Store(account.ID, now.UTC())
		return currentGeneration, true
	}

	// A missing, malformed, or expired block starts a fresh epoch. Mark the
	// pool boundary before publishing the new runtime state so sockets dialed
	// after this point cannot become guard candidates.
	s.openaiAccountRuntimeBlockObservedUpdatedAt.Store(account.ID, account.UpdatedAt)
	generation := s.openaiAccountRuntimeBlockSequence.Add(1)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, generation)
	if s.shouldMarkOpenAI429GuardCandidatesLocked(account, reason) {
		if pool := s.getOpenAIWSConnPool(); pool != nil {
			pool.markExistingConnsAs429GuardCandidatesAt(account.ID, now, generation)
		}
	}
	s.openaiAccountRuntimeBlockUntil.Store(account.ID, blockUntil)
	s.openaiAccountRuntimeBlockInstalledAt.Store(account.ID, now.UTC())
	s.storeOpenAIAccountRuntimeBlockReason(account.ID, reason)
	return generation, true
}

func (s *OpenAIGatewayService) storeOpenAIAccountRuntimeBlockReason(accountID int64, reason string) {
	if s == nil || accountID <= 0 {
		return
	}
	next := strings.TrimSpace(reason)
	currentRaw, loaded := s.openaiAccountRuntimeBlockReason.Load(accountID)
	current := ""
	if loaded {
		current = strings.TrimSpace(fmt.Sprint(currentRaw))
	}
	if current != "" && current != "account_scheduling_threshold" && next == "account_scheduling_threshold" {
		return
	}
	s.openaiAccountRuntimeBlockReason.Store(accountID, next)
}

// ReconcileOpenAIAccountRuntimeBlock is called by SchedulerSnapshotService
// after it has read an authoritative account row in response to a scheduler
// outbox event.  Runtime blocks are intentionally installed before durable
// state writes so a failing request is stopped immediately; that means a
// different gateway instance can clear the database and leave a stale block
// in this process.  Retire it only when all of the following hold:
//
//   - the fresh row has no active durable account cooldown;
//   - the row version is newer than the version captured for this local block;
//   - the local block generation was installed no later than the outbox read
//     (a newer request racing the event wins the CAS and remains blocked).
//
// A durable recovery retires all local guard reservations for the account. A
// confirmed Codex 429 may have retained one exact connection, but once another
// instance has authoritatively cleared the cooldown that socket must not keep
// ordinary scheduling filtered; it is detached and closed below.
func (s *OpenAIGatewayService) ReconcileOpenAIAccountRuntimeBlock(account *Account, observedAt time.Time) {
	if s == nil || !isOpenAIAccount(account) || account.ID <= 0 || account.UpdatedAt.IsZero() {
		return
	}
	if observedAt.IsZero() {
		// Callers in production always provide the pre-read timestamp.  A zero
		// value from a custom integration is treated conservatively as "now" so
		// a concurrently installed block is still considered newer.
		observedAt = time.Now().UTC()
	}

	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()

	// Capture the generation and installation timestamp while holding the same
	// lock used by BlockAccountScheduling/ClearAccountSchedulingBlock.
	rawUntil, active := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	until, untilOK := rawUntil.(time.Time)
	if !active || !untilOK || until.IsZero() || !time.Now().Before(until) {
		mu.Unlock()
		return
	}
	if accountPersistedSchedulingCooldownActive(account) {
		mu.Unlock()
		return
	}

	if rawInstalled, ok := s.openaiAccountRuntimeBlockInstalledAt.Load(account.ID); ok {
		if installedAt, ok := rawInstalled.(time.Time); ok && !installedAt.IsZero() && installedAt.After(observedAt) {
			// A request installed a newer local generation while the outbox
			// worker was reading the row.  Do not let the older event erase it.
			mu.Unlock()
			return
		}
	}
	rawObserved, hasObserved := s.openaiAccountRuntimeBlockObservedUpdatedAt.Load(account.ID)
	observedVersion, observedOK := rawObserved.(time.Time)
	if !hasObserved || !observedOK || observedVersion.IsZero() || !account.UpdatedAt.After(observedVersion) {
		// A zero/missing row version cannot prove that a durable writer observed
		// the transition.  Keep the local block fail-closed in that case.
		mu.Unlock()
		return
	}

	var generation uint64
	if rawGeneration, ok := s.openaiAccountRuntimeBlockGeneration.Load(account.ID); ok {
		generation, _ = rawGeneration.(uint64)
	}
	currentReason := ""
	if rawReason, ok := s.openaiAccountRuntimeBlockReason.Load(account.ID); ok {
		currentReason = strings.TrimSpace(fmt.Sprint(rawReason))
	}
	// Re-check the same state under the lock immediately before deletion.  The
	// lock makes this a generation/deadline CAS against a concurrent transport
	// failure or a fresh 429 transition.
	currentRaw, currentOK := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	currentUntil, currentTimeOK := currentRaw.(time.Time)
	if !currentOK || !currentTimeOK || !currentUntil.Equal(until) {
		mu.Unlock()
		return
	}
	var guardConns []*openAIWSConn
	if currentReason == "429" {
		// A durable recovery makes the account eligible for ordinary requests
		// again.  Permanent guard pins are continuation-only reservations; leave
		// one behind and hasOpenAI429GuardReservation would keep every ordinary
		// scheduler candidate filtered even after this CAS succeeds.  Detach
		// under the runtime transition, then close after releasing the lock.
		if pool := s.existingOpenAIWSConnPool(); pool != nil {
			guardConns = pool.detachGuardConns(account.ID)
		}
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockReason.Delete(account.ID)
	s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
	s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
	s.openaiOAuth429RetryStartedAt.Delete(account.ID)
	nextGeneration := s.openaiAccountRuntimeBlockSequence.Add(1)
	if generation >= nextGeneration {
		// The sequence is process-local and normally strictly increasing.  Keep
		// the stored value monotonic even for a wrapped/custom test sequence.
		nextGeneration = generation + 1
	}
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, nextGeneration)
	mu.Unlock()
	// Do not clear the two-strike confirmation here.  A new 429 can arrive
	// immediately after this observer releases the runtime lock; clearing the
	// distributed streak after that point would erase the newer confirmation.
	// Explicit administrator recovery still clears the streak through the
	// existing ClearAccountSchedulingBlock path.
	closeOpenAIWSConns(guardConns)
}

// ReconcileOpenAIAccountRuntimeBlockEvent is the scheduler-facing, payload-aware
// entrypoint.  Only an explicit durable-clear marker may retire a local block;
// ordinary account updates (quota snapshots, proxy/name edits, etc.) are not
// sufficient evidence because they can advance UpdatedAt after a failed block
// write.  The legacy method above remains available for direct/admin recovery
// and focused integrations that already know a clear occurred.
func (s *OpenAIGatewayService) ReconcileOpenAIAccountRuntimeBlockEvent(account *Account, observedAt time.Time, payload map[string]any) {
	if !SchedulerRuntimeBlockClearRequested(payload) {
		return
	}
	s.ReconcileOpenAIAccountRuntimeBlock(account, observedAt)
}

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.clearOpenAIOAuth429Streak(accountID)
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockReason.Delete(accountID)
	s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(accountID)
	s.openaiAccountRuntimeBlockInstalledAt.Delete(accountID)
	s.openaiOAuth429RetryStartedAt.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
	mu.Unlock()
}

// clearAllOpenAIRuntimeBlockState releases process-local account blocks during
// service shutdown.  The maps are intentionally ephemeral; leaving their keys
// behind on a reused service instance could resurrect a stale guard after the
// websocket pool has been rebuilt.
func (s *OpenAIGatewayService) clearAllOpenAIRuntimeBlockState() {
	if s == nil {
		return
	}
	clear := func(m *sync.Map) {
		m.Range(func(key, _ any) bool {
			m.Delete(key)
			return true
		})
	}
	clear(&s.openaiAccountRuntimeBlockUntil)
	clear(&s.openaiAccountRuntimeBlockReason)
	clear(&s.openaiAccountRuntimeBlockObservedUpdatedAt)
	clear(&s.openaiAccountRuntimeBlockInstalledAt)
	clear(&s.openaiAccountRuntimeBlockLocks)
	clear(&s.openaiAccountRuntimeBlockGeneration)
	clear(&s.openaiOAuth429RetryStartedAt)
}

func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlocked(account *Account) bool {
	if s == nil || !isOpenAIAccount(account) {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	if account.HasAvailableCodexCredits() {
		if rawReason, ok := s.openaiAccountRuntimeBlockReason.Load(account.ID); ok && rawReason == "account_scheduling_threshold" {
			s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
			s.openaiAccountRuntimeBlockReason.Delete(account.ID)
			s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
			s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
			s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
			return s.hasOpenAI429GuardReservation(account)
		}
	}
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return s.hasOpenAI429GuardReservation(account)
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockReason.Delete(account.ID)
		s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
		s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return s.hasOpenAI429GuardReservation(account)
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockReason.Delete(account.ID)
	s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
	s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return s.hasOpenAI429GuardReservation(account)
}

// hasOpenAI429GuardReservation keeps ordinary scheduling away from an account
// whose only live route is a local WebSocket retained after a confirmed Codex
// OAuth 429. The continuation selector is the sole exception and force-acquires
// the exact bound socket instead of treating this account as generally usable.
func (s *OpenAIGatewayService) hasOpenAI429GuardReservation(account *Account) bool {
	if s == nil || account == nil || !account.Codex429GuardEnabled() || !account.IsOpenAIOAuth() {
		return false
	}
	pool := s.existingOpenAIWSConnPool()
	return pool != nil && pool.HasPermanentGuardPin(account.ID)
}

// openAI429GuardRuntimeBlockUntil returns the local expiry for a confirmed
// OAuth 429 block. The expiry is used both by scheduling and by the WebSocket
// pool pin so an otherwise healthy old connection outlives normal idle/max-age
// cleanup while that confirmed state remains active.
func (s *OpenAIGatewayService) openAI429GuardRuntimeBlockUntil(account *Account) (time.Time, bool) {
	if s == nil || account == nil || account.ID <= 0 {
		return time.Time{}, false
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	reason, _ := s.openaiAccountRuntimeBlockReason.Load(account.ID)
	if strings.TrimSpace(fmt.Sprint(reason)) != "429" {
		return time.Time{}, false
	}
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return time.Time{}, false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		return time.Time{}, false
	}
	if !time.Now().Before(until) {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockReason.Delete(account.ID)
		s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
		s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return time.Time{}, false
	}
	return until, true
}

// isOpenAI429GuardRuntimeBlocked distinguishes the confirmed OAuth 429 block
// from every other temporary scheduling block. It intentionally requires the
// in-process reason because response-to-connection affinity is local too.
func (s *OpenAIGatewayService) isOpenAI429GuardRuntimeBlocked(account *Account) bool {
	_, active := s.openAI429GuardRuntimeBlockUntil(account)
	return active
}

func (s *OpenAIGatewayService) getOpenAIAccountModelTransientState() *openAIAccountModelTransientState {
	if s == nil {
		return nil
	}
	s.openaiModelTransientOnce.Do(func() {
		if s.openaiModelTransient == nil {
			s.openaiModelTransient = newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax)
		}
	})
	return s.openaiModelTransient
}

func canonicalOpenAIAccountSchedulingModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if account.IsOpenAI() {
		return resolveOpenAIAccountUpstreamModelForRequest(account, model, false)
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		return mapped
	}
	return model
}

func openAIAccountModelTransientModel(canonicalModel string) string {
	return normalizeOpenAIAccountModelTransientModel(canonicalModel)
}

func (s *OpenAIGatewayService) recordOpenAIAccountModelTransientFailure(account *Account, canonicalModel string, now time.Time) openAIAccountModelTransientDecision {
	if s == nil || account == nil {
		return openAIAccountModelTransientDecision{}
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return openAIAccountModelTransientDecision{}
	}
	return state.recordFailure(account.ID, openAIAccountModelTransientModel(canonicalModel), now)
}

func (s *OpenAIGatewayService) clearOpenAIAccountModelTransientState(accountID int64, model string) {
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return
	}
	state.recordSuccess(accountID, model)
}

func (s *OpenAIGatewayService) isOpenAIAccountModelRuntimeBlocked(account *Account, requestedModel string) bool {
	if s == nil || account == nil {
		return false
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return false
	}
	canonicalModel := canonicalOpenAIAccountSchedulingModel(account, requestedModel)
	return state.isBlocked(account.ID, openAIAccountModelTransientModel(canonicalModel), time.Now())
}

// accountPersistedSchedulingCooldownActive reports whether the account snapshot
// still carries an active durable account-level cooldown. The scheduler cache
// is shared by gateway instances, so these fields are the authority used to
// retire a stale process-local runtime block after another instance clears it.
func accountPersistedSchedulingCooldownActive(account *Account) bool {
	if account == nil {
		return false
	}
	now := time.Now()
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return true
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return true
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return true
	}
	return false
}

// openAIAccountRuntimeBlockSnapshot is a scheduler handoff value. Generation
// and deadline form the CAS identity; the observed row version protects a
// block whose durable write has not yet become visible to this instance.
type openAIAccountRuntimeBlockSnapshot struct {
	until      time.Time
	generation uint64
	blocked    bool
}

// peekOpenAIAccountRuntimeBlock returns a coherent local block snapshot and
// retires entries whose local deadline has elapsed.
func (s *OpenAIGatewayService) peekOpenAIAccountRuntimeBlock(account *Account) openAIAccountRuntimeBlockSnapshot {
	if s == nil || !isOpenAIAccount(account) {
		return openAIAccountRuntimeBlockSnapshot{}
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return openAIAccountRuntimeBlockSnapshot{}
	}
	until, isTime := value.(time.Time)
	if !isTime || until.IsZero() || !time.Now().Before(until) {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockReason.Delete(account.ID)
		s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(account.ID)
		s.openaiAccountRuntimeBlockInstalledAt.Delete(account.ID)
		s.openaiOAuth429RetryStartedAt.Delete(account.ID)
		generation := s.openaiAccountRuntimeBlockSequence.Add(1)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, generation)
		return openAIAccountRuntimeBlockSnapshot{}
	}

	snapshot := openAIAccountRuntimeBlockSnapshot{until: until, blocked: true}
	if rawGeneration, ok := s.openaiAccountRuntimeBlockGeneration.Load(account.ID); ok {
		snapshot.generation, _ = rawGeneration.(uint64)
	}
	return snapshot
}

// clearOpenAIAccountRuntimeBlockIfUnchanged deletes a local block only when
// generation and deadline captured by peek are still current. The same lock is
// used by block installation, so a concurrent transport failure cannot be
// erased by a stale scheduler read.
func (s *OpenAIGatewayService) clearOpenAIAccountRuntimeBlockIfUnchanged(accountID int64, snapshot openAIAccountRuntimeBlockSnapshot) {
	if s == nil || accountID <= 0 || !snapshot.blocked {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()

	generation, ok := s.openaiAccountRuntimeBlockGeneration.Load(accountID)
	if !ok || generation != snapshot.generation {
		return
	}
	current, ok := s.openaiAccountRuntimeBlockUntil.Load(accountID)
	currentUntil, isTime := current.(time.Time)
	if !ok || !isTime || !currentUntil.Equal(snapshot.until) {
		return
	}
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockReason.Delete(accountID)
	s.openaiAccountRuntimeBlockObservedUpdatedAt.Delete(accountID)
	s.openaiAccountRuntimeBlockInstalledAt.Delete(accountID)
	s.openaiOAuth429RetryStartedAt.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
}

// localRuntimeBlockPersistencePending keeps a just-installed block active when
// the durable Set* write failed. Production account snapshots carry UpdatedAt;
// a newer row version proves that a database writer (possibly another instance)
// has observed the transition. A zero-version fixture with a repository is
// treated conservatively and remains blocked until its local deadline.
func (s *OpenAIGatewayService) localRuntimeBlockPersistencePending(account *Account, snapshot openAIAccountRuntimeBlockSnapshot) bool {
	if s == nil || account == nil || !snapshot.blocked {
		return false
	}
	observedRaw, present := s.openaiAccountRuntimeBlockObservedUpdatedAt.Load(account.ID)
	if !present {
		return false
	}
	observedUpdatedAt, _ := observedRaw.(time.Time)
	if account.UpdatedAt.IsZero() {
		// A zero row version cannot prove that a durable writer observed the
		// transition. Keep every ordinary account-level block fail-closed until
		// an explicit clear or a versioned snapshot arrives. The sole exception
		// is the administrator utilization-threshold block when a fresh paid
		// credit snapshot is available; that is the documented credit override
		// and is safe to retire locally.
		if rawReason, ok := s.openaiAccountRuntimeBlockReason.Load(account.ID); ok &&
			strings.TrimSpace(fmt.Sprint(rawReason)) == "account_scheduling_threshold" &&
			account.HasAvailableCodexCredits() {
			return false
		}
		return true
	}
	if observedUpdatedAt.IsZero() {
		return false
	}
	return !account.UpdatedAt.After(observedUpdatedAt)
}

func (s *OpenAIGatewayService) isOpenAIAccountRequestRuntimeBlocked(account *Account, requestedModel string) bool {
	if s == nil {
		return false
	}
	snapshot := s.peekOpenAIAccountRuntimeBlock(account)
	if snapshot.blocked {
		rawReason, _ := s.openaiAccountRuntimeBlockReason.Load(account.ID)
		reason := strings.TrimSpace(fmt.Sprint(rawReason))
		// A positive Codex credit balance intentionally overrides only the
		// administrator utilization-threshold block. Preserve the local block
		// while its durable write is still unobserved, but do not let a stale
		// threshold block hide an otherwise usable credited account.
		creditThresholdOverride := account != nil && account.HasAvailableCodexCredits() && reason == "account_scheduling_threshold" &&
			!s.localRuntimeBlockPersistencePending(account, snapshot)
		if !creditThresholdOverride && (accountPersistedSchedulingCooldownActive(account) || s.localRuntimeBlockPersistencePending(account, snapshot)) {
			return true
		}
		s.clearOpenAIAccountRuntimeBlockIfUnchanged(account.ID, snapshot)
	}
	// Keep the existing guard-reservation semantics (a confirmed 429 may retain
	// one exact websocket) and independent model-scoped transient cooldowns.
	return s.isOpenAIAccountRuntimeBlocked(account) || s.isOpenAIAccountModelRuntimeBlocked(account, requestedModel)
}

func (s *OpenAIGatewayService) recordOpenAIOAuth429() {
	if s == nil {
		return
	}
	now := time.Now()
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || now.Sub(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		if s.openaiOAuth429WindowStartUnixNano.CompareAndSwap(windowStart, now.UnixNano()) {
			s.openaiOAuth429WindowCount.Store(1)
			return
		}
	}
	s.openaiOAuth429WindowCount.Add(1)
}

func (s *OpenAIGatewayService) ShouldStopOpenAIOAuth429Failover(account *Account, statusCode int, failedSwitches int, state *OpenAIOAuth429FailoverState) bool {
	if failedSwitches < openAIOAuth429StormMaxAccountSwitches {
		return false
	}
	if state != nil && state.grokOAuth429FollowupPending {
		// The follow-up budget was armed by a Grok OAuth 429. Consume it on
		// any failing follow-up account, even if a mixed pool selected an API-key
		// account next.
		return true
	}
	if isGrokOAuthAccount(account) {
		if state == nil {
			// Preserve the old threshold for callers that have not adopted the
			// request-local state contract yet.
			return statusCode == http.StatusTooManyRequests && failedSwitches >= 2
		}
		if statusCode == http.StatusTooManyRequests {
			state.grokOAuth429FollowupPending = true
		}
		return false
	}
	if statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) {
		return false
	}
	// Each OpenAI OAuth candidate has already consumed its full same-account
	// retry window before reaching this switch point. A global storm is useful
	// telemetry, but must not prevent trying the bounded next-account budget.
	return failedSwitches >= openAIOAuth429MaxAccountAttempts
}
