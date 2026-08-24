package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	openAIWSResponseAccountCachePrefix = "openai:response:"
	openAIHTTPResponseOwnerUserPrefix  = "openai:http-response-owner:user:"
	openAIHTTPResponseOwnerKeyPrefix   = "openai:http-response-owner:key:"
	openAIWSStateStoreCleanupInterval  = time.Minute
	openAIWSStateStoreCleanupMaxPerMap = 512
	openAIWSStateStoreMaxEntriesPerMap = 65536
	openAIWSStateStoreRedisTimeout     = 3 * time.Second
)

type openAIWSAccountBinding struct {
	accountID int64
	expiresAt time.Time
}

type openAIHTTPResponseOwnerBinding struct {
	userID    int64
	apiKeyID  int64
	expiresAt time.Time
}

type openAIWSConnBinding struct {
	connID    string
	expiresAt time.Time
}

type openAIWSTurnStateBinding struct {
	turnState string
	expiresAt time.Time
}

type openAIWSSessionConnBinding struct {
	connID    string
	expiresAt time.Time
}

// OpenAIWSStateStore 管理 WSv2 的粘连状态。
// - response_id -> account_id 用于续链路由
// - response_id -> conn_id 用于连接内上下文复用
//
// response_id -> account_id 优先走 GatewayCache（Redis），同时维护本地热缓存。
// response_id -> conn_id 仅在本进程内有效。
type OpenAIWSStateStore interface {
	BindResponseAccount(ctx context.Context, groupID int64, responseID string, accountID int64, ttl time.Duration) error
	GetResponseAccount(ctx context.Context, groupID int64, responseID string) (int64, error)
	DeleteResponseAccount(ctx context.Context, groupID int64, responseID string) error
	BindHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64, ttl time.Duration) error
	GetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string) (userID, apiKeyID int64, found bool, err error)

	BindResponseConn(responseID, connID string, ttl time.Duration)
	GetResponseConn(responseID string) (string, bool)
	DeleteResponseConn(responseID string)

	BindSessionTurnState(groupID int64, sessionHash, turnState string, ttl time.Duration)
	GetSessionTurnState(groupID int64, sessionHash string) (string, bool)
	DeleteSessionTurnState(groupID int64, sessionHash string)

	BindSessionConn(groupID int64, sessionHash, connID string, ttl time.Duration)
	GetSessionConn(groupID int64, sessionHash string) (string, bool)
	DeleteSessionConn(groupID int64, sessionHash string)
}

// openAIWSContinuationBindingCleaner is intentionally separate from the
// public state-store contract so custom test stores remain source-compatible.
// The default store uses these compare-and-delete operations to avoid a
// failed connection removing a newer concurrent response binding.
type openAIWSContinuationBindingCleaner interface {
	deleteResponseBindingIfMatches(ctx context.Context, groupID int64, responseID string, accountID int64, connID string) bool
	deleteSessionConnIfMatches(groupID int64, sessionHash, connID string) bool
}

// openAIWSGuardBindingStore keeps the local account/connection pair alive for
// the lifetime of a confirmed 429 guard pin. It is intentionally optional so
// lightweight test stores and external implementations retain the original
// state-store contract.
type openAIWSGuardBindingStore interface {
	BindGuardResponse(groupID int64, responseID string, accountID int64, connID string)
	BindGuardSession(groupID int64, sessionHash string, accountID int64, connID string)
	GetGuardSession(groupID int64, sessionHash string) (int64, string, bool)
}

// openAIWSConnectionBindingInvalidator is used by the pooled transport when a
// socket is closed outside an active request (for example, an idle health
// probe or account invalidation).  The exact account/connection pair is the
// ownership key; response and session identifiers alone are not sufficient to
// distinguish a stale old socket from a newer concurrent binding.
type openAIWSConnectionBindingInvalidator interface {
	invalidateConnectionBindings(accountID int64, connID string)
}

type defaultOpenAIWSStateStore struct {
	cache GatewayCache

	// These operation locks keep the two halves of a guard binding (account
	// and connection) from being observed or overwritten independently. The
	// per-map locks below still protect ordinary single-map access and cleanup.
	responseBindingOpMu sync.RWMutex
	sessionBindingOpMu  sync.RWMutex

	responseAccountOpMu  sync.Mutex
	responseToAccountMu  sync.RWMutex
	responseToAccount    map[string]openAIWSAccountBinding
	responseOwnerMu      sync.RWMutex
	responseOwners       map[string]openAIHTTPResponseOwnerBinding
	responseToConnMu     sync.RWMutex
	responseToConn       map[string]openAIWSConnBinding
	sessionToTurnStateMu sync.RWMutex
	sessionToTurnState   map[string]openAIWSTurnStateBinding
	sessionToAccountMu   sync.RWMutex
	sessionToAccount     map[string]openAIWSAccountBinding
	sessionToConnMu      sync.RWMutex
	sessionToConn        map[string]openAIWSSessionConnBinding

	lastCleanupUnixNano atomic.Int64
}

// NewOpenAIWSStateStore 创建默认 WS 状态存储。
func NewOpenAIWSStateStore(cache GatewayCache) OpenAIWSStateStore {
	store := &defaultOpenAIWSStateStore{
		cache:              cache,
		responseToAccount:  make(map[string]openAIWSAccountBinding, 256),
		responseOwners:     make(map[string]openAIHTTPResponseOwnerBinding, 256),
		responseToConn:     make(map[string]openAIWSConnBinding, 256),
		sessionToTurnState: make(map[string]openAIWSTurnStateBinding, 256),
		sessionToAccount:   make(map[string]openAIWSAccountBinding, 256),
		sessionToConn:      make(map[string]openAIWSSessionConnBinding, 256),
	}
	store.lastCleanupUnixNano.Store(time.Now().UnixNano())
	return store
}

func (s *defaultOpenAIWSStateStore) BindHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64, ttl time.Duration) error {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" || userID <= 0 || apiKeyID <= 0 {
		return nil
	}
	ttl = normalizeOpenAIWSTTL(ttl)
	s.maybeCleanup()

	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseOwnerMu.Lock()
	ensureBindingCapacity(s.responseOwners, mapKey, openAIWSStateStoreMaxEntriesPerMap)
	s.responseOwners[mapKey] = openAIHTTPResponseOwnerBinding{
		userID: userID, apiKeyID: apiKeyID, expiresAt: time.Now().Add(ttl),
	}
	s.responseOwnerMu.Unlock()

	if s.cache == nil {
		return nil
	}
	cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	if err := s.cache.SetSessionAccountID(cacheCtx, groupID, openAIHTTPResponseOwnerCacheKey(openAIHTTPResponseOwnerUserPrefix, id), userID, ttl); err != nil {
		return err
	}
	return s.cache.SetSessionAccountID(cacheCtx, groupID, openAIHTTPResponseOwnerCacheKey(openAIHTTPResponseOwnerKeyPrefix, id), apiKeyID, ttl)
}

func (s *defaultOpenAIWSStateStore) GetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string) (int64, int64, bool, error) {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" {
		return 0, 0, false, nil
	}
	s.maybeCleanup()

	now := time.Now()
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseOwnerMu.RLock()
	if binding, ok := s.responseOwners[mapKey]; ok && now.Before(binding.expiresAt) {
		s.responseOwnerMu.RUnlock()
		return binding.userID, binding.apiKeyID, true, nil
	}
	s.responseOwnerMu.RUnlock()

	if s.cache == nil {
		return 0, 0, false, nil
	}
	cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	userID, err := s.cache.GetSessionAccountID(cacheCtx, groupID, openAIHTTPResponseOwnerCacheKey(openAIHTTPResponseOwnerUserPrefix, id))
	if err != nil || userID <= 0 {
		return 0, 0, false, err
	}
	apiKeyID, err := s.cache.GetSessionAccountID(cacheCtx, groupID, openAIHTTPResponseOwnerCacheKey(openAIHTTPResponseOwnerKeyPrefix, id))
	if err != nil || apiKeyID <= 0 {
		return 0, 0, false, err
	}

	s.responseOwnerMu.Lock()
	ensureBindingCapacity(s.responseOwners, mapKey, openAIWSStateStoreMaxEntriesPerMap)
	s.responseOwners[mapKey] = openAIHTTPResponseOwnerBinding{
		userID: userID, apiKeyID: apiKeyID, expiresAt: now.Add(time.Minute),
	}
	s.responseOwnerMu.Unlock()
	return userID, apiKeyID, true, nil
}

func (s *defaultOpenAIWSStateStore) BindResponseAccount(ctx context.Context, groupID int64, responseID string, accountID int64, ttl time.Duration) error {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" || accountID <= 0 {
		return nil
	}
	s.responseAccountOpMu.Lock()
	defer s.responseAccountOpMu.Unlock()
	ttl = normalizeOpenAIWSTTL(ttl)
	s.maybeCleanup()

	expiresAt := time.Now().Add(ttl)
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseBindingOpMu.Lock()
	s.responseToAccountMu.Lock()
	if existing, exists := s.responseToAccount[mapKey]; exists && existing.expiresAt.IsZero() {
		// A confirmed guard binding is process-local and permanent. A later
		// ordinary response write must not downgrade it to a TTL binding (or
		// publish a Redis record that could route around the guarded socket).
		s.responseToAccountMu.Unlock()
		s.responseBindingOpMu.Unlock()
		return nil
	}
	if !ensureBindingCapacityPreserving(s.responseToAccount, mapKey, openAIWSStateStoreMaxEntriesPerMap, func(binding openAIWSAccountBinding) bool {
		return !binding.expiresAt.IsZero()
	}) {
		// Never evict a permanent guard binding for an ordinary continuation.
		s.responseToAccountMu.Unlock()
		s.responseBindingOpMu.Unlock()
		return nil
	}
	s.responseToAccount[mapKey] = openAIWSAccountBinding{accountID: accountID, expiresAt: expiresAt}
	s.responseToAccountMu.Unlock()
	s.responseBindingOpMu.Unlock()

	if s.cache == nil {
		return nil
	}
	cacheKey := openAIWSResponseAccountCacheKey(id)
	// A response can finish after the downstream request has been canceled. Keep
	// the durable account binding write alive long enough to support continuation
	// routing after reconnect, while retaining request values for tracing.
	cacheCtx, cancel := withOpenAIWSStateStoreRedisWriteTimeout(ctx)
	defer cancel()
	return s.cache.SetSessionAccountID(cacheCtx, groupID, cacheKey, accountID, ttl)
}

func cleanupExpiredHTTPResponseOwnerBindings(bindings map[string]openAIHTTPResponseOwnerBinding, now time.Time, maxScan int) {
	if len(bindings) == 0 || maxScan <= 0 {
		return
	}
	scanned := 0
	for key, binding := range bindings {
		if now.After(binding.expiresAt) {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func (s *defaultOpenAIWSStateStore) GetResponseAccount(ctx context.Context, groupID int64, responseID string) (int64, error) {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" {
		return 0, nil
	}
	s.responseAccountOpMu.Lock()
	defer s.responseAccountOpMu.Unlock()
	s.maybeCleanup()

	now := time.Now()
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseBindingOpMu.RLock()
	s.responseToAccountMu.RLock()
	if binding, ok := s.responseToAccount[mapKey]; ok {
		if openAIWSBindingActive(binding.expiresAt, now) {
			accountID := binding.accountID
			s.responseToAccountMu.RUnlock()
			s.responseBindingOpMu.RUnlock()
			return accountID, nil
		}
	}
	s.responseToAccountMu.RUnlock()
	s.responseBindingOpMu.RUnlock()

	if s.cache == nil {
		return 0, nil
	}

	cacheKey := openAIWSResponseAccountCacheKey(id)
	cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	accountID, err := s.cache.GetSessionAccountID(cacheCtx, groupID, cacheKey)
	if err != nil || accountID <= 0 {
		// 缓存读取失败不阻断主流程，按未命中降级。
		return 0, nil
	}
	return accountID, nil
}

func (s *defaultOpenAIWSStateStore) DeleteResponseAccount(ctx context.Context, groupID int64, responseID string) error {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" {
		return nil
	}
	s.responseAccountOpMu.Lock()
	defer s.responseAccountOpMu.Unlock()
	s.responseBindingOpMu.Lock()
	defer s.responseBindingOpMu.Unlock()
	s.responseToAccountMu.Lock()
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	accountBinding, accountExists := s.responseToAccount[mapKey]
	// A permanent guard binding is an account/connection pair. Ordinary
	// response cleanup must never remove one half of that pair; only the
	// conditional cleaner below may release it after matching the socket.
	s.responseToConnMu.RLock()
	connBinding, connExists := s.responseToConn[id]
	s.responseToConnMu.RUnlock()
	if (accountExists && accountBinding.expiresAt.IsZero()) || (connExists && connBinding.expiresAt.IsZero()) {
		s.responseToAccountMu.Unlock()
		return nil
	}
	delete(s.responseToAccount, mapKey)
	s.responseToAccountMu.Unlock()

	if s.cache == nil {
		return nil
	}
	cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	return s.cache.DeleteSessionAccountID(cacheCtx, groupID, openAIWSResponseAccountCacheKey(id))
}

func (s *defaultOpenAIWSStateStore) deleteResponseBindingIfMatches(ctx context.Context, groupID int64, responseID string, accountID int64, connID string) bool {
	id := normalizeOpenAIWSResponseID(responseID)
	expectedConnID := strings.TrimSpace(connID)
	// A response binding is only safe to remove when both halves identify the
	// same failed upstream socket. In particular, an empty connID must never
	// turn into an account-only delete of a permanent guard tuple.
	if id == "" || accountID <= 0 || expectedConnID == "" {
		return false
	}
	s.responseAccountOpMu.Lock()
	defer s.responseAccountOpMu.Unlock()
	s.responseBindingOpMu.Lock()
	defer s.responseBindingOpMu.Unlock()

	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseToAccountMu.Lock()
	binding, ok := s.responseToAccount[mapKey]
	if !ok || binding.accountID != accountID {
		s.responseToAccountMu.Unlock()
		return false
	}
	// Compare the connection half before deleting the account half. A newer
	// binding for the same response must survive an old connection's failure.
	s.responseToConnMu.RLock()
	connBinding, connOK := s.responseToConn[id]
	s.responseToConnMu.RUnlock()
	if !connOK || strings.TrimSpace(connBinding.connID) != expectedConnID {
		s.responseToAccountMu.Unlock()
		return false
	}
	// Hold the account operation lock while removing the local/cache account
	// value; BindResponseAccount cannot publish a replacement in between.
	delete(s.responseToAccount, mapKey)
	s.responseToAccountMu.Unlock()

	s.responseToConnMu.Lock()
	if connBinding, connOK := s.responseToConn[id]; connOK && strings.TrimSpace(connBinding.connID) == expectedConnID {
		delete(s.responseToConn, id)
	}
	s.responseToConnMu.Unlock()
	if s.cache != nil {
		cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
		_ = s.cache.DeleteSessionAccountID(cacheCtx, groupID, openAIWSResponseAccountCacheKey(id))
		cancel()
	}
	return true
}

func (s *defaultOpenAIWSStateStore) BindResponseConn(responseID, connID string, ttl time.Duration) {
	id := normalizeOpenAIWSResponseID(responseID)
	conn := strings.TrimSpace(connID)
	if id == "" || conn == "" {
		return
	}
	ttl = normalizeOpenAIWSTTL(ttl)
	s.maybeCleanup()

	s.responseBindingOpMu.Lock()
	defer s.responseBindingOpMu.Unlock()
	s.responseToConnMu.Lock()
	if existing, exists := s.responseToConn[id]; exists && existing.expiresAt.IsZero() {
		// Preserve a permanent guard connection binding. It is released only by
		// explicit cleanup after the socket/account is invalidated.
		s.responseToConnMu.Unlock()
		return
	}
	if !ensureBindingCapacityPreserving(s.responseToConn, id, openAIWSStateStoreMaxEntriesPerMap, func(binding openAIWSConnBinding) bool {
		return !binding.expiresAt.IsZero()
	}) {
		// Never evict a permanent guard socket for an ordinary response pin.
		s.responseToConnMu.Unlock()
		return
	}
	s.responseToConn[id] = openAIWSConnBinding{
		connID:    conn,
		expiresAt: time.Now().Add(ttl),
	}
	s.responseToConnMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) GetResponseConn(responseID string) (string, bool) {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" {
		return "", false
	}
	s.maybeCleanup()

	now := time.Now()
	s.responseBindingOpMu.RLock()
	s.responseToConnMu.RLock()
	binding, ok := s.responseToConn[id]
	s.responseToConnMu.RUnlock()
	s.responseBindingOpMu.RUnlock()
	if !ok || !openAIWSBindingActive(binding.expiresAt, now) || strings.TrimSpace(binding.connID) == "" {
		return "", false
	}
	return binding.connID, true
}

func (s *defaultOpenAIWSStateStore) DeleteResponseConn(responseID string) {
	id := normalizeOpenAIWSResponseID(responseID)
	if id == "" {
		return
	}
	s.responseBindingOpMu.Lock()
	s.responseToConnMu.RLock()
	binding, exists := s.responseToConn[id]
	s.responseToConnMu.RUnlock()
	if exists && binding.expiresAt.IsZero() {
		// Permanent guard connections are released only by explicit conditional
		// cleanup after the exact socket has failed.
		s.responseBindingOpMu.Unlock()
		return
	}
	s.responseToConnMu.Lock()
	delete(s.responseToConn, id)
	s.responseToConnMu.Unlock()
	s.responseBindingOpMu.Unlock()
}

// BindGuardResponse publishes a local-only, non-expiring response/account /
// connection tuple. Redis is deliberately not written: another process cannot
// use this process-local socket, and a remote cache record would otherwise
// route a continuation to an account without its guarded connection.
func (s *defaultOpenAIWSStateStore) BindGuardResponse(groupID int64, responseID string, accountID int64, connID string) {
	id := normalizeOpenAIWSResponseID(responseID)
	conn := strings.TrimSpace(connID)
	if id == "" || accountID <= 0 || conn == "" {
		return
	}
	s.responseAccountOpMu.Lock()
	defer s.responseAccountOpMu.Unlock()
	s.responseBindingOpMu.Lock()
	defer s.responseBindingOpMu.Unlock()
	s.maybeCleanup()
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	s.responseToAccountMu.Lock()
	s.responseToConnMu.Lock()
	// A permanent tuple is immutable until the exact socket is invalidated. Do
	// not overwrite either half if another guard already owns this key.
	if existing, exists := s.responseToAccount[mapKey]; exists && existing.expiresAt.IsZero() && existing.accountID != accountID {
		s.responseToConnMu.Unlock()
		s.responseToAccountMu.Unlock()
		return
	}
	if existing, exists := s.responseToConn[id]; exists && existing.expiresAt.IsZero() && existing.connID != conn {
		s.responseToConnMu.Unlock()
		s.responseToAccountMu.Unlock()
		return
	}
	canEvictAccount := func(binding openAIWSAccountBinding) bool { return !binding.expiresAt.IsZero() }
	canEvictConn := func(binding openAIWSConnBinding) bool { return !binding.expiresAt.IsZero() }
	// Check both maps before evicting either one so a full permanent map cannot
	// leave the guard tuple with only one half installed.
	if !canEnsureBindingCapacity(s.responseToAccount, mapKey, openAIWSStateStoreMaxEntriesPerMap, canEvictAccount) ||
		!canEnsureBindingCapacity(s.responseToConn, id, openAIWSStateStoreMaxEntriesPerMap, canEvictConn) {
		s.responseToConnMu.Unlock()
		s.responseToAccountMu.Unlock()
		return
	}
	ensureBindingCapacityPreserving(s.responseToAccount, mapKey, openAIWSStateStoreMaxEntriesPerMap, canEvictAccount)
	ensureBindingCapacityPreserving(s.responseToConn, id, openAIWSStateStoreMaxEntriesPerMap, canEvictConn)
	s.responseToAccount[mapKey] = openAIWSAccountBinding{accountID: accountID}
	s.responseToConn[id] = openAIWSConnBinding{connID: conn}
	s.responseToConnMu.Unlock()
	s.responseToAccountMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) BindSessionTurnState(groupID int64, sessionHash, turnState string, ttl time.Duration) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	state := strings.TrimSpace(turnState)
	if key == "" || state == "" {
		return
	}
	ttl = normalizeOpenAIWSTTL(ttl)
	s.maybeCleanup()

	s.sessionToTurnStateMu.Lock()
	ensureBindingCapacity(s.sessionToTurnState, key, openAIWSStateStoreMaxEntriesPerMap)
	s.sessionToTurnState[key] = openAIWSTurnStateBinding{
		turnState: state,
		expiresAt: time.Now().Add(ttl),
	}
	s.sessionToTurnStateMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) GetSessionTurnState(groupID int64, sessionHash string) (string, bool) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	if key == "" {
		return "", false
	}
	s.maybeCleanup()

	now := time.Now()
	s.sessionToTurnStateMu.RLock()
	binding, ok := s.sessionToTurnState[key]
	s.sessionToTurnStateMu.RUnlock()
	if !ok || !openAIWSBindingActive(binding.expiresAt, now) || strings.TrimSpace(binding.turnState) == "" {
		return "", false
	}
	return binding.turnState, true
}

func (s *defaultOpenAIWSStateStore) DeleteSessionTurnState(groupID int64, sessionHash string) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	if key == "" {
		return
	}
	s.sessionToTurnStateMu.Lock()
	delete(s.sessionToTurnState, key)
	s.sessionToTurnStateMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) BindSessionConn(groupID int64, sessionHash, connID string, ttl time.Duration) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	conn := strings.TrimSpace(connID)
	if key == "" || conn == "" {
		return
	}
	ttl = normalizeOpenAIWSTTL(ttl)
	s.maybeCleanup()

	s.sessionBindingOpMu.Lock()
	defer s.sessionBindingOpMu.Unlock()
	s.sessionToConnMu.Lock()
	if existing, exists := s.sessionToConn[key]; exists && existing.expiresAt.IsZero() {
		// Do not downgrade a permanent guard session to an ordinary TTL pin.
		s.sessionToConnMu.Unlock()
		return
	}
	if !ensureBindingCapacityPreserving(s.sessionToConn, key, openAIWSStateStoreMaxEntriesPerMap, func(binding openAIWSSessionConnBinding) bool {
		return !binding.expiresAt.IsZero()
	}) {
		// Preserve permanent guard sessions when the local continuation map is
		// saturated.
		s.sessionToConnMu.Unlock()
		return
	}
	s.sessionToConn[key] = openAIWSSessionConnBinding{
		connID:    conn,
		expiresAt: time.Now().Add(ttl),
	}
	s.sessionToConnMu.Unlock()
}

// BindGuardSession is the session-hash counterpart to BindGuardResponse. The
// account and connection are local-only and remain valid until the guarded
// socket is evicted or the binding is explicitly cleared.
func (s *defaultOpenAIWSStateStore) BindGuardSession(groupID int64, sessionHash string, accountID int64, connID string) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	conn := strings.TrimSpace(connID)
	if key == "" || accountID <= 0 || conn == "" {
		return
	}
	s.maybeCleanup()
	s.sessionBindingOpMu.Lock()
	defer s.sessionBindingOpMu.Unlock()
	s.sessionToAccountMu.Lock()
	s.sessionToConnMu.Lock()
	if existing, exists := s.sessionToAccount[key]; exists && existing.expiresAt.IsZero() && existing.accountID != accountID {
		s.sessionToConnMu.Unlock()
		s.sessionToAccountMu.Unlock()
		return
	}
	if existing, exists := s.sessionToConn[key]; exists && existing.expiresAt.IsZero() && existing.connID != conn {
		s.sessionToConnMu.Unlock()
		s.sessionToAccountMu.Unlock()
		return
	}
	canEvictAccount := func(binding openAIWSAccountBinding) bool { return !binding.expiresAt.IsZero() }
	canEvictConn := func(binding openAIWSSessionConnBinding) bool { return !binding.expiresAt.IsZero() }
	if !canEnsureBindingCapacity(s.sessionToAccount, key, openAIWSStateStoreMaxEntriesPerMap, canEvictAccount) ||
		!canEnsureBindingCapacity(s.sessionToConn, key, openAIWSStateStoreMaxEntriesPerMap, canEvictConn) {
		s.sessionToConnMu.Unlock()
		s.sessionToAccountMu.Unlock()
		return
	}
	ensureBindingCapacityPreserving(s.sessionToAccount, key, openAIWSStateStoreMaxEntriesPerMap, canEvictAccount)
	ensureBindingCapacityPreserving(s.sessionToConn, key, openAIWSStateStoreMaxEntriesPerMap, canEvictConn)
	s.sessionToAccount[key] = openAIWSAccountBinding{accountID: accountID}
	s.sessionToConn[key] = openAIWSSessionConnBinding{connID: conn}
	s.sessionToConnMu.Unlock()
	s.sessionToAccountMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) GetGuardSession(groupID int64, sessionHash string) (int64, string, bool) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	if key == "" {
		return 0, "", false
	}
	s.maybeCleanup()
	s.sessionBindingOpMu.RLock()
	defer s.sessionBindingOpMu.RUnlock()
	s.sessionToAccountMu.RLock()
	accountBinding, accountOK := s.sessionToAccount[key]
	s.sessionToAccountMu.RUnlock()
	s.sessionToConnMu.RLock()
	connBinding, connOK := s.sessionToConn[key]
	s.sessionToConnMu.RUnlock()
	if !accountOK || !connOK || accountBinding.accountID <= 0 || strings.TrimSpace(connBinding.connID) == "" {
		return 0, "", false
	}
	if !accountBinding.expiresAt.IsZero() || !connBinding.expiresAt.IsZero() {
		return 0, "", false
	}
	return accountBinding.accountID, connBinding.connID, true
}

func (s *defaultOpenAIWSStateStore) GetSessionConn(groupID int64, sessionHash string) (string, bool) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	if key == "" {
		return "", false
	}
	s.maybeCleanup()

	now := time.Now()
	s.sessionBindingOpMu.RLock()
	defer s.sessionBindingOpMu.RUnlock()
	s.sessionToConnMu.RLock()
	binding, ok := s.sessionToConn[key]
	s.sessionToConnMu.RUnlock()
	if !ok || !openAIWSBindingActive(binding.expiresAt, now) || strings.TrimSpace(binding.connID) == "" {
		return "", false
	}
	return binding.connID, true
}

func (s *defaultOpenAIWSStateStore) DeleteSessionConn(groupID int64, sessionHash string) {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	if key == "" {
		return
	}
	s.sessionBindingOpMu.Lock()
	defer s.sessionBindingOpMu.Unlock()
	s.sessionToAccountMu.RLock()
	accountBinding, accountExists := s.sessionToAccount[key]
	s.sessionToAccountMu.RUnlock()
	s.sessionToConnMu.RLock()
	connBinding, connExists := s.sessionToConn[key]
	s.sessionToConnMu.RUnlock()
	if (accountExists && accountBinding.expiresAt.IsZero()) || (connExists && connBinding.expiresAt.IsZero()) {
		// Keep the account/connection pair intact until the failed connection is
		// explicitly identified by deleteSessionConnIfMatches.
		return
	}
	s.sessionToAccountMu.Lock()
	delete(s.sessionToAccount, key)
	s.sessionToAccountMu.Unlock()
	s.sessionToConnMu.Lock()
	delete(s.sessionToConn, key)
	s.sessionToConnMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) deleteSessionConnIfMatches(groupID int64, sessionHash, connID string) bool {
	key := openAIWSSessionTurnStateKey(groupID, sessionHash)
	expected := strings.TrimSpace(connID)
	if key == "" || expected == "" {
		return false
	}
	s.sessionBindingOpMu.Lock()
	defer s.sessionBindingOpMu.Unlock()
	s.sessionToAccountMu.Lock()
	defer s.sessionToAccountMu.Unlock()
	s.sessionToConnMu.Lock()
	defer s.sessionToConnMu.Unlock()
	binding, ok := s.sessionToConn[key]
	if !ok || strings.TrimSpace(binding.connID) != expected {
		return false
	}
	delete(s.sessionToConn, key)
	delete(s.sessionToAccount, key)
	return true
}

// invalidateConnectionBindings removes every local response/session binding
// that still points at one exact account socket.  It deliberately does not
// touch Redis: the response-to-connection half is process-local, so a remote
// account record cannot route a request back to this closed socket, and its
// ordinary TTL remains bounded.  Keeping this operation local also prevents a
// background health-check failure from blocking on a cache round trip.
func (s *defaultOpenAIWSStateStore) invalidateConnectionBindings(accountID int64, connID string) {
	if s == nil || accountID <= 0 {
		return
	}
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return
	}

	// Keep the same lock order as response bind/delete operations: account
	// operation -> binding operation -> account map -> connection map. The
	// account-before-connection order is important because conditional delete
	// takes both maps while comparing an exact socket identity.
	s.responseAccountOpMu.Lock()
	s.responseBindingOpMu.Lock()
	s.responseToAccountMu.Lock()
	s.responseToConnMu.Lock()
	for responseID, binding := range s.responseToConn {
		if strings.TrimSpace(binding.connID) != connID {
			continue
		}
		delete(s.responseToConn, responseID)
		suffix := ":" + responseID
		for mapKey, accountBinding := range s.responseToAccount {
			if accountBinding.accountID == accountID && strings.HasSuffix(mapKey, suffix) {
				delete(s.responseToAccount, mapKey)
			}
		}
	}
	s.responseToConnMu.Unlock()
	s.responseToAccountMu.Unlock()
	s.responseBindingOpMu.Unlock()
	s.responseAccountOpMu.Unlock()

	// Session bindings use the same exact socket identity.  Remove the local
	// turn-state entry together with the account/connection pair so a reconnect
	// cannot inherit a stale protocol state after a health-check eviction.
	s.sessionBindingOpMu.Lock()
	s.sessionToAccountMu.Lock()
	s.sessionToConnMu.Lock()
	s.sessionToTurnStateMu.Lock()
	for key, binding := range s.sessionToConn {
		if strings.TrimSpace(binding.connID) != connID {
			continue
		}
		if accountBinding, ok := s.sessionToAccount[key]; ok && accountBinding.accountID != accountID {
			continue
		}
		delete(s.sessionToConn, key)
		delete(s.sessionToAccount, key)
		delete(s.sessionToTurnState, key)
	}
	s.sessionToTurnStateMu.Unlock()
	s.sessionToConnMu.Unlock()
	s.sessionToAccountMu.Unlock()
	s.sessionBindingOpMu.Unlock()
}

func (s *defaultOpenAIWSStateStore) maybeCleanup() {
	if s == nil {
		return
	}
	now := time.Now()
	last := time.Unix(0, s.lastCleanupUnixNano.Load())
	if now.Sub(last) < openAIWSStateStoreCleanupInterval {
		return
	}
	if !s.lastCleanupUnixNano.CompareAndSwap(last.UnixNano(), now.UnixNano()) {
		return
	}

	// 增量限额清理，避免高规模下一次性全量扫描导致长时间阻塞。
	s.responseToAccountMu.Lock()
	cleanupExpiredAccountBindings(s.responseToAccount, now, openAIWSStateStoreCleanupMaxPerMap)
	s.responseToAccountMu.Unlock()

	s.responseOwnerMu.Lock()
	cleanupExpiredHTTPResponseOwnerBindings(s.responseOwners, now, openAIWSStateStoreCleanupMaxPerMap)
	s.responseOwnerMu.Unlock()

	s.responseToConnMu.Lock()
	cleanupExpiredConnBindings(s.responseToConn, now, openAIWSStateStoreCleanupMaxPerMap)
	s.responseToConnMu.Unlock()

	s.sessionToTurnStateMu.Lock()
	cleanupExpiredTurnStateBindings(s.sessionToTurnState, now, openAIWSStateStoreCleanupMaxPerMap)
	s.sessionToTurnStateMu.Unlock()

	s.sessionToAccountMu.Lock()
	cleanupExpiredAccountBindings(s.sessionToAccount, now, openAIWSStateStoreCleanupMaxPerMap)
	s.sessionToAccountMu.Unlock()

	s.sessionToConnMu.Lock()
	cleanupExpiredSessionConnBindings(s.sessionToConn, now, openAIWSStateStoreCleanupMaxPerMap)
	s.sessionToConnMu.Unlock()
}

func cleanupExpiredAccountBindings(bindings map[string]openAIWSAccountBinding, now time.Time, maxScan int) {
	if len(bindings) == 0 || maxScan <= 0 {
		return
	}
	scanned := 0
	for key, binding := range bindings {
		if !binding.expiresAt.IsZero() && now.After(binding.expiresAt) {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func cleanupExpiredConnBindings(bindings map[string]openAIWSConnBinding, now time.Time, maxScan int) {
	if len(bindings) == 0 || maxScan <= 0 {
		return
	}
	scanned := 0
	for key, binding := range bindings {
		if !binding.expiresAt.IsZero() && now.After(binding.expiresAt) {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func cleanupExpiredTurnStateBindings(bindings map[string]openAIWSTurnStateBinding, now time.Time, maxScan int) {
	if len(bindings) == 0 || maxScan <= 0 {
		return
	}
	scanned := 0
	for key, binding := range bindings {
		if !binding.expiresAt.IsZero() && now.After(binding.expiresAt) {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func cleanupExpiredSessionConnBindings(bindings map[string]openAIWSSessionConnBinding, now time.Time, maxScan int) {
	if len(bindings) == 0 || maxScan <= 0 {
		return
	}
	scanned := 0
	for key, binding := range bindings {
		if !binding.expiresAt.IsZero() && now.After(binding.expiresAt) {
			delete(bindings, key)
		}
		scanned++
		if scanned >= maxScan {
			break
		}
	}
}

func openAIWSBindingActive(expiresAt, now time.Time) bool {
	return expiresAt.IsZero() || now.Before(expiresAt)
}

// ensureBindingCapacity applies the optional eviction policy and reports
// whether a slot is available. Existing callers that ignore the return value
// retain the original bounded-map behavior.
func ensureBindingCapacity[T any](bindings map[string]T, incomingKey string, maxEntries int, evictable ...func(T) bool) bool {
	if len(bindings) < maxEntries || maxEntries <= 0 {
		return true
	}
	if _, exists := bindings[incomingKey]; exists {
		return true
	}
	canEvict := func(T) bool { return true }
	if len(evictable) > 0 && evictable[0] != nil {
		canEvict = evictable[0]
	}
	for key, value := range bindings {
		if canEvict(value) {
			delete(bindings, key)
			return true
		}
	}
	return false
}

// ensureBindingCapacityPreserving evicts only entries accepted by canEvict.
// Permanent Codex guard tuples use a zero expiry and must survive ordinary
// continuation pressure; returning false lets callers keep the map bounded
// without installing a partial or unpinned binding.
func ensureBindingCapacityPreserving[T any](bindings map[string]T, incomingKey string, maxEntries int, canEvict func(T) bool) bool {
	if len(bindings) < maxEntries || maxEntries <= 0 {
		return true
	}
	if _, exists := bindings[incomingKey]; exists {
		return true
	}
	for key, value := range bindings {
		if canEvict == nil || canEvict(value) {
			delete(bindings, key)
			return true
		}
	}
	return false
}

func canEnsureBindingCapacity[T any](bindings map[string]T, incomingKey string, maxEntries int, canEvict func(T) bool) bool {
	if len(bindings) < maxEntries || maxEntries <= 0 {
		return true
	}
	if _, exists := bindings[incomingKey]; exists {
		return true
	}
	for _, value := range bindings {
		if canEvict == nil || canEvict(value) {
			return true
		}
	}
	return false
}

func normalizeOpenAIWSResponseID(responseID string) string {
	return strings.TrimSpace(responseID)
}

func openAIWSResponseAccountCacheKey(responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return openAIWSResponseAccountCachePrefix + hex.EncodeToString(sum[:])
}

func openAIHTTPResponseOwnerCacheKey(prefix, responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return prefix + hex.EncodeToString(sum[:])
}

// openAIWSResponseAccountMapKey 本地热缓存按分组隔离的 key，与 Redis 层保持一致，避免跨组命中。
func openAIWSResponseAccountMapKey(groupID int64, responseID string) string {
	return fmt.Sprintf("%d:%s", groupID, responseID)
}

func normalizeOpenAIWSTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Hour
	}
	return ttl
}

func openAIWSSessionTurnStateKey(groupID int64, sessionHash string) string {
	hash := strings.TrimSpace(sessionHash)
	if hash == "" {
		return ""
	}
	return fmt.Sprintf("%d:%s", groupID, hash)
}

func withOpenAIWSStateStoreRedisTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, openAIWSStateStoreRedisTimeout)
}

func withOpenAIWSStateStoreRedisWriteTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(ctx, openAIWSStateStoreRedisTimeout)
}
