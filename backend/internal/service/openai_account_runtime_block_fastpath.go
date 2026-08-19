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
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormThreshold          = 20
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

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth
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
	if account != nil && account.Platform == PlatformOpenAI &&
		isOpenAIRequestScopedCapacityShed("", responseBody) {
		return false
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	if account != nil && account.Platform == PlatformOpenAI && isOpenAIContextWindowError("", responseBody) {
		return false
	}

	if isOpenAIImageRateLimitError(statusCode, responseBody) {
		if s != nil && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageRateLimit(stateCtx, account, statusCode, headers, responseBody)
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
		return snapshot
	}
	until, ok := rawUntil.(time.Time)
	if !ok || until.IsZero() || !time.Now().Before(until) {
		s.openaiAccountRuntimeBlockUntil.Delete(accountID)
		s.openaiAccountRuntimeBlockReason.Delete(accountID)
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

	cooldownUntil := time.Now().Add(openAIOAuth429FallbackCooldown)
	if s.rateLimitService != nil {
		if resetAt := s.rateLimitService.calculateOpenAI429ResetTime(headers); resetAt != nil && resetAt.After(time.Now()) {
			cooldownUntil = *resetAt
		} else if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
			if resetAt := time.Unix(*resetUnix, 0); resetAt.After(time.Now()) {
				cooldownUntil = resetAt
			}
		} else if cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && cooldown > 0 {
			cooldownUntil = time.Now().Add(cooldown)
		}
	}
	s.BlockAccountScheduling(account, cooldownUntil, "429")
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
		return currentGeneration, true
	}

	// A missing, malformed, or expired block starts a fresh epoch. Mark the
	// pool boundary before publishing the new runtime state so sockets dialed
	// after this point cannot become guard candidates.
	generation := s.openaiAccountRuntimeBlockSequence.Add(1)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, generation)
	if s.shouldMarkOpenAI429GuardCandidatesLocked(account, reason) {
		if pool := s.getOpenAIWSConnPool(); pool != nil {
			pool.markExistingConnsAs429GuardCandidatesAt(account.ID, now, generation)
		}
	}
	s.openaiAccountRuntimeBlockUntil.Store(account.ID, blockUntil)
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

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.clearOpenAIOAuth429Streak(accountID)
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockReason.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
	mu.Unlock()
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
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return s.hasOpenAI429GuardReservation(account)
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockReason.Delete(account.ID)
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

func (s *OpenAIGatewayService) isOpenAIAccountRequestRuntimeBlocked(account *Account, requestedModel string) bool {
	return s != nil && (s.isOpenAIAccountRuntimeBlocked(account) || s.isOpenAIAccountModelRuntimeBlocked(account, requestedModel))
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

func (s *OpenAIGatewayService) isOpenAIOAuth429Storm() bool {
	if s == nil {
		return false
	}
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || time.Since(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		return false
	}
	return s.openaiOAuth429WindowCount.Load() >= openAIOAuth429StormThreshold
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
	return s.isOpenAIOAuth429Storm()
}
