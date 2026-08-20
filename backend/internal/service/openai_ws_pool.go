package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"golang.org/x/sync/errgroup"
)

const (
	openAIWSConnMaxAge             = 60 * time.Minute
	openAIWSConnHealthCheckIdle    = 90 * time.Second
	openAIWSConnHealthCheckTO      = 2 * time.Second
	openAIWSConnPrewarmExtraDelay  = 2 * time.Second
	openAIWSAcquireCleanupInterval = 3 * time.Second
	openAIWSBackgroundPingInterval = 30 * time.Second
	openAIWSBackgroundSweepTicker  = 30 * time.Second

	openAIWSPrewarmFailureWindow   = 30 * time.Second
	openAIWSPrewarmFailureSuppress = 2
)

var (
	errOpenAIWSConnClosed               = errors.New("openai ws connection closed")
	errOpenAIWSConnQueueFull            = errors.New("openai ws connection queue full")
	errOpenAIWSPreferredConnUnavailable = errors.New("openai ws preferred connection unavailable")
)

type openAIWSDialError struct {
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte
	Err             error
}

func (e *openAIWSDialError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("openai ws dial failed: status=%d err=%v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("openai ws dial failed: %v", e.Err)
}

func (e *openAIWSDialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type openAIWSAcquireRequest struct {
	Account *Account
	WSURL   string
	Headers http.Header
	// IdentityCompatibility is populated for genuine Codex CLI requests. It
	// keeps installation/session/thread continuation state isolated inside the
	// account's pooled sockets while remaining empty for generic compatibility
	// traffic (which retains the historical beta-only compatibility key).
	IdentityCompatibility string
	// HeadersFactory is evaluated inside dialConn. It exists so credentials
	// whose authorization is per-dial (Agent Identity) are never cached in
	// lastAcquire or delayed prewarm state.
	HeadersFactory  func(context.Context, http.Header) (http.Header, error)
	ProxyURL        string
	PreferredConnID string
	// ForceNewConn: 强制本次获取新连接（避免复用导致连接内续链状态互相污染）。
	ForceNewConn bool
	// ForcePreferredConn: 强制本次只使用 PreferredConnID，禁止漂移到其它连接。
	ForcePreferredConn bool
}

type openAIWSHandshakeCompatibilityKey struct {
	betaFeatures        string
	codexInstallationID string
	sessionIDHyphen     string
	sessionIDUnderscore string
	threadID            string
	clientRequestID     string
	codexWindowID       string
	// identity is the fork's non-reversible genuine-Codex session key. It
	// protects a retained 429 continuation socket even when optional handshake
	// features change between turns.
	identity string
}

type openAIWSConnLease struct {
	pool                          *openAIWSConnPool
	accountID                     int64
	conn                          *openAIWSConn
	queueWait                     time.Duration
	connPick                      time.Duration
	reused                        bool
	openAI429GuardActiveAtAcquire bool
	openAIRuntimeBlockGeneration  uint64
	// openAI429GuardProven records that this lease was the exact connection
	// observed or selected for a confirmed 429 guard. It remains true after
	// pool eviction so failure handling can switch accounts instead of
	// redialing the same account after the cooldown expires.
	openAI429GuardProven atomic.Bool
	released             atomic.Bool
}

func (l *openAIWSConnLease) activeConn() (*openAIWSConn, error) {
	if l == nil || l.conn == nil {
		return nil, errOpenAIWSConnClosed
	}
	if l.released.Load() {
		return nil, errOpenAIWSConnClosed
	}
	return l.conn, nil
}

func (l *openAIWSConnLease) ConnID() string {
	if l == nil || l.conn == nil {
		return ""
	}
	return l.conn.id
}

func (l *openAIWSConnLease) QueueWaitDuration() time.Duration {
	if l == nil {
		return 0
	}
	return l.queueWait
}

func (l *openAIWSConnLease) ConnPickDuration() time.Duration {
	if l == nil {
		return 0
	}
	return l.connPick
}

func (l *openAIWSConnLease) Reused() bool {
	if l == nil {
		return false
	}
	return l.reused
}

func (l *openAIWSConnLease) HandshakeHeader(name string) string {
	if l == nil || l.conn == nil {
		return ""
	}
	return l.conn.handshakeHeader(name)
}

func (l *openAIWSConnLease) HandshakeHeaders() http.Header {
	if l == nil || l.conn == nil {
		return nil
	}
	return cloneHeader(l.conn.handshakeHeaders)
}

func (l *openAIWSConnLease) IsPrewarmed() bool {
	if l == nil || l.conn == nil {
		return false
	}
	return l.conn.isPrewarmed()
}

func (l *openAIWSConnLease) MarkPrewarmed() {
	if l == nil || l.conn == nil {
		return
	}
	l.conn.markPrewarmed()
}

func (l *openAIWSConnLease) WriteJSON(value any, timeout time.Duration) error {
	conn, err := l.activeConn()
	if err != nil {
		return err
	}
	return conn.writeJSONWithTimeout(context.Background(), value, timeout)
}

func (l *openAIWSConnLease) WriteJSONWithContextTimeout(ctx context.Context, value any, timeout time.Duration) error {
	conn, err := l.activeConn()
	if err != nil {
		return err
	}
	return conn.writeJSONWithTimeout(ctx, value, timeout)
}

func (l *openAIWSConnLease) WriteJSONContext(ctx context.Context, value any) error {
	conn, err := l.activeConn()
	if err != nil {
		return err
	}
	return conn.writeJSON(value, ctx)
}

func (l *openAIWSConnLease) ReadMessage(timeout time.Duration) ([]byte, error) {
	conn, err := l.activeConn()
	if err != nil {
		return nil, err
	}
	return conn.readMessageWithTimeout(timeout)
}

func (l *openAIWSConnLease) ReadMessageContext(ctx context.Context) ([]byte, error) {
	conn, err := l.activeConn()
	if err != nil {
		return nil, err
	}
	return conn.readMessage(ctx)
}

func (l *openAIWSConnLease) ReadMessageWithContextTimeout(ctx context.Context, timeout time.Duration) ([]byte, error) {
	conn, err := l.activeConn()
	if err != nil {
		return nil, err
	}
	return conn.readMessageWithContextTimeout(ctx, timeout)
}

func (l *openAIWSConnLease) PingWithTimeout(timeout time.Duration) error {
	conn, err := l.activeConn()
	if err != nil {
		return err
	}
	return conn.pingWithTimeout(timeout)
}

func (l *openAIWSConnLease) SupportsIdlePingWithoutReader() bool {
	conn, err := l.activeConn()
	if err != nil {
		return false
	}
	return conn.supportsIdlePingWithoutReader()
}

func (l *openAIWSConnLease) MarkBroken() {
	if l == nil || l.pool == nil || l.conn == nil || l.released.Load() {
		return
	}
	l.pool.evictConn(l.accountID, l.conn.id)
}

func (l *openAIWSConnLease) Release() {
	if l == nil || l.conn == nil {
		return
	}
	if !l.released.CompareAndSwap(false, true) {
		return
	}
	l.conn.release()
	if l.pool != nil {
		l.pool.notifyAccountPoolChanged(l.accountID)
	}
}

type openAIWSConn struct {
	id        string
	accountID int64
	ws        openAIWSClientConn
	// onClose is installed by the owning pool for real dialed connections. It
	// lets out-of-band eviction (idle ping, max-age cleanup, account reset)
	// invalidate the exact response/session bindings that point at this socket.
	onClose func(accountID int64, connID string)

	handshakeHeaders       http.Header
	handshakeCompatibility openAIWSHandshakeCompatibilityKey
	routingAffinity        string
	proxyURL               string
	wsURL                  string
	proxyURLKnown          bool
	wsURLKnown             bool

	leaseCh   chan struct{}
	closedCh  chan struct{}
	closeOnce sync.Once

	readMu  sync.Mutex
	writeMu sync.Mutex

	waiters       atomic.Int32
	createdAtNano atomic.Int64
	lastUsedNano  atomic.Int64
	prewarmed     atomic.Bool
	// guardConfirmed429Generation is positive only after this exact pooled
	// socket has observed the confirming 429 for the corresponding runtime
	// block generation. It prevents ordinary sticky bindings from promoting a
	// newly dialed connection into the permanent guard socket.
	guardConfirmed429Generation atomic.Uint64
	// guard429CandidateGeneration is non-zero only for sockets already present
	// in the pool when a confirmed Codex OAuth 429 block begins. Binding the
	// candidate to the exact runtime generation prevents stale evidence from a
	// prior block from promoting the socket in a later block.
	guard429CandidateGeneration atomic.Uint64
}

func newOpenAIWSConn(id string, accountID int64, ws openAIWSClientConn, handshakeHeaders http.Header) *openAIWSConn {
	now := time.Now()
	conn := &openAIWSConn{
		id:               id,
		accountID:        accountID,
		ws:               ws,
		handshakeHeaders: cloneHeader(handshakeHeaders),
		leaseCh:          make(chan struct{}, 1),
		closedCh:         make(chan struct{}),
	}
	conn.leaseCh <- struct{}{}
	conn.createdAtNano.Store(now.UnixNano())
	conn.lastUsedNano.Store(now.UnixNano())
	return conn
}

func (c *openAIWSConn) tryAcquire() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.closedCh:
		return false
	default:
	}
	select {
	case <-c.leaseCh:
		select {
		case <-c.closedCh:
			c.release()
			return false
		default:
		}
		return true
	default:
		return false
	}
}

func (c *openAIWSConn) acquire(ctx context.Context) error {
	if c == nil {
		return errOpenAIWSConnClosed
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closedCh:
			return errOpenAIWSConnClosed
		case <-c.leaseCh:
			// A cancellation and a lease delivery can become ready together. Once
			// the semaphore token has been consumed, check the context again and
			// return it before reporting cancellation so a canceled waiter cannot
			// strand a pooled connection.
			if err := ctx.Err(); err != nil {
				c.release()
				return err
			}
			select {
			case <-c.closedCh:
				c.release()
				return errOpenAIWSConnClosed
			default:
			}
			return nil
		}
	}
}

func (c *openAIWSConn) release() {
	if c == nil {
		return
	}
	select {
	case c.leaseCh <- struct{}{}:
	default:
	}
	c.touch()
}

func (c *openAIWSConn) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.closedCh)
		if c.ws != nil {
			_ = c.ws.Close()
		}
		if c.onClose != nil {
			c.onClose(c.accountID, c.id)
		}
		select {
		case c.leaseCh <- struct{}{}:
		default:
		}
	})
}

func (c *openAIWSConn) writeJSONWithTimeout(parent context.Context, value any, timeout time.Duration) error {
	if c == nil {
		return errOpenAIWSConnClosed
	}
	select {
	case <-c.closedCh:
		return errOpenAIWSConnClosed
	default:
	}

	writeCtx := parent
	if writeCtx == nil {
		writeCtx = context.Background()
	}
	if timeout <= 0 {
		return c.writeJSON(value, writeCtx)
	}
	var cancel context.CancelFunc
	writeCtx, cancel = context.WithTimeout(writeCtx, timeout)
	defer cancel()
	return c.writeJSON(value, writeCtx)
}

func (c *openAIWSConn) writeJSON(value any, writeCtx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ws == nil {
		return errOpenAIWSConnClosed
	}
	if writeCtx == nil {
		writeCtx = context.Background()
	}
	if err := c.ws.WriteJSON(writeCtx, value); err != nil {
		return err
	}
	c.touch()
	return nil
}

func (c *openAIWSConn) readMessageWithTimeout(timeout time.Duration) ([]byte, error) {
	return c.readMessageWithContextTimeout(context.Background(), timeout)
}

func (c *openAIWSConn) readMessageWithContextTimeout(parent context.Context, timeout time.Duration) ([]byte, error) {
	if c == nil {
		return nil, errOpenAIWSConnClosed
	}
	select {
	case <-c.closedCh:
		return nil, errOpenAIWSConnClosed
	default:
	}

	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return c.readMessage(parent)
	}
	readCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return c.readMessage(readCtx)
}

func (c *openAIWSConn) readMessage(readCtx context.Context) ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.ws == nil {
		return nil, errOpenAIWSConnClosed
	}
	if readCtx == nil {
		readCtx = context.Background()
	}
	payload, err := c.ws.ReadMessage(readCtx)
	if err != nil {
		return nil, err
	}
	c.touch()
	return payload, nil
}

func (c *openAIWSConn) pingWithTimeout(timeout time.Duration) error {
	if c == nil {
		return errOpenAIWSConnClosed
	}
	select {
	case <-c.closedCh:
		return errOpenAIWSConnClosed
	default:
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ws == nil {
		return errOpenAIWSConnClosed
	}
	if timeout <= 0 {
		timeout = openAIWSConnHealthCheckTO
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.ws.Ping(pingCtx); err != nil {
		return err
	}
	return nil
}

func (c *openAIWSConn) supportsIdlePingWithoutReader() bool {
	if c == nil || c.ws == nil {
		return false
	}
	capable, ok := c.ws.(openAIWSIdlePingCapable)
	// Test and alternate implementations keep the historical probe behavior
	// unless they explicitly declare it unsafe.
	return !ok || capable.SupportsIdlePingWithoutReader()
}

func (c *openAIWSConn) touch() {
	if c == nil {
		return
	}
	c.lastUsedNano.Store(time.Now().UnixNano())
}

func (c *openAIWSConn) createdAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	nano := c.createdAtNano.Load()
	if nano <= 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

func (c *openAIWSConn) lastUsedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	nano := c.lastUsedNano.Load()
	if nano <= 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

func (c *openAIWSConn) idleDuration(now time.Time) time.Duration {
	if c == nil {
		return 0
	}
	last := c.lastUsedAt()
	if last.IsZero() {
		return 0
	}
	return now.Sub(last)
}

func (c *openAIWSConn) age(now time.Time) time.Duration {
	if c == nil {
		return 0
	}
	created := c.createdAt()
	if created.IsZero() {
		return 0
	}
	return now.Sub(created)
}

func (c *openAIWSConn) isLeased() bool {
	if c == nil {
		return false
	}
	return len(c.leaseCh) == 0
}

func (c *openAIWSConn) handshakeHeader(name string) string {
	if c == nil || c.handshakeHeaders == nil {
		return ""
	}
	return strings.TrimSpace(c.handshakeHeaders.Get(strings.TrimSpace(name)))
}

func (c *openAIWSConn) matchesHandshakeCompatibility(compatibility openAIWSHandshakeCompatibilityKey) bool {
	return c != nil && c.handshakeCompatibility == compatibility
}

// matchesHandshakeIdentity keeps genuine Codex CLI sessions isolated even
// when a permanent 429 guard deliberately reuses a socket across a later beta
// feature-set change. The upstream connection carries continuation state, so
// an installation/session/thread mismatch is never safe to relax.
func (c *openAIWSConn) matchesHandshakeIdentity(compatibility openAIWSHandshakeCompatibilityKey) bool {
	return c != nil && c.handshakeCompatibility.identity == compatibility.identity
}

func (c *openAIWSConn) matchesProxyURL(proxyURL string) bool {
	if c == nil {
		return false
	}
	if !c.proxyURLKnown && c.proxyURL == "" {
		// Test/custom pool fixtures created before proxy identity was tracked
		// have no metadata. Treat them as unknown rather than as direct, while
		// every real dial records proxyURLKnown below.
		return true
	}
	return c.proxyURL == normalizeOpenAIWSProxyURL(proxyURL)
}

func (c *openAIWSConn) matchesWSURL(wsURL string) bool {
	if c == nil {
		return false
	}
	if !c.wsURLKnown && c.wsURL == "" {
		return true
	}
	return c.wsURL == normalizeOpenAIWSURL(wsURL)
}

func (c *openAIWSConn) matchesRoutingAffinity(routingAffinity string) bool {
	return c != nil && c.routingAffinity == routingAffinity
}

func (c *openAIWSConn) isPrewarmed() bool {
	if c == nil {
		return false
	}
	return c.prewarmed.Load()
}

func (c *openAIWSConn) markPrewarmed() {
	if c == nil {
		return
	}
	c.prewarmed.Store(true)
}

type openAIWSAccountPool struct {
	mu               sync.Mutex
	conns            map[string]*openAIWSConn
	pinnedConns      map[string]int
	guardPinnedUntil map[string]time.Time
	changedCh        chan struct{}
	creating         int
	generation       uint64
	lastCleanupAt    time.Time
	lastAcquire      *openAIWSAcquireRequest
	prewarmActive    bool
	prewarmUntil     time.Time
	prewarmFails     int
	prewarmFailAt    time.Time
}

func (ap *openAIWSAccountPool) changeChannelLocked() chan struct{} {
	if ap.changedCh == nil {
		ap.changedCh = make(chan struct{})
	}
	return ap.changedCh
}

func (ap *openAIWSAccountPool) signalChangedLocked() {
	if ap == nil {
		return
	}
	if ap.changedCh != nil {
		close(ap.changedCh)
	}
	ap.changedCh = make(chan struct{})
}

type OpenAIWSPoolMetricsSnapshot struct {
	AcquireTotal            int64
	AcquireReuseTotal       int64
	AcquireCreateTotal      int64
	AcquireQueueWaitTotal   int64
	AcquireQueueWaitMsTotal int64
	ConnPickTotal           int64
	ConnPickMsTotal         int64
	ScaleUpTotal            int64
	ScaleDownTotal          int64
}

type openAIWSPoolMetrics struct {
	acquireTotal          atomic.Int64
	acquireReuseTotal     atomic.Int64
	acquireCreateTotal    atomic.Int64
	acquireQueueWaitTotal atomic.Int64
	acquireQueueWaitMs    atomic.Int64
	connPickTotal         atomic.Int64
	connPickMs            atomic.Int64
	scaleUpTotal          atomic.Int64
	scaleDownTotal        atomic.Int64
}

type openAIWSConnPool struct {
	cfg *config.Config
	// 通过接口解耦底层 WS 客户端实现，默认使用 coder/websocket。
	clientDialer openAIWSClientDialer

	guardBindingInvalidatorMu sync.RWMutex
	guardBindingInvalidator   func(accountID int64, connID string)

	accounts sync.Map // key: int64(accountID), value: *openAIWSAccountPool
	seq      atomic.Uint64

	metrics openAIWSPoolMetrics

	workerStopCh chan struct{}
	workerWg     sync.WaitGroup
	closeOnce    sync.Once
}

func newOpenAIWSConnPool(cfg *config.Config) *openAIWSConnPool {
	pool := &openAIWSConnPool{
		cfg:          cfg,
		clientDialer: newDefaultOpenAIWSClientDialer(),
		workerStopCh: make(chan struct{}),
	}
	pool.startBackgroundWorkers()
	return pool
}

func (p *openAIWSConnPool) SnapshotMetrics() OpenAIWSPoolMetricsSnapshot {
	if p == nil {
		return OpenAIWSPoolMetricsSnapshot{}
	}
	return OpenAIWSPoolMetricsSnapshot{
		AcquireTotal:            p.metrics.acquireTotal.Load(),
		AcquireReuseTotal:       p.metrics.acquireReuseTotal.Load(),
		AcquireCreateTotal:      p.metrics.acquireCreateTotal.Load(),
		AcquireQueueWaitTotal:   p.metrics.acquireQueueWaitTotal.Load(),
		AcquireQueueWaitMsTotal: p.metrics.acquireQueueWaitMs.Load(),
		ConnPickTotal:           p.metrics.connPickTotal.Load(),
		ConnPickMsTotal:         p.metrics.connPickMs.Load(),
		ScaleUpTotal:            p.metrics.scaleUpTotal.Load(),
		ScaleDownTotal:          p.metrics.scaleDownTotal.Load(),
	}
}

func (p *openAIWSConnPool) SnapshotTransportMetrics() OpenAIWSTransportMetricsSnapshot {
	if p == nil {
		return OpenAIWSTransportMetricsSnapshot{}
	}
	if dialer, ok := p.clientDialer.(openAIWSTransportMetricsDialer); ok {
		return dialer.SnapshotTransportMetrics()
	}
	return OpenAIWSTransportMetricsSnapshot{}
}

func (p *openAIWSConnPool) setClientDialerForTest(dialer openAIWSClientDialer) {
	if p == nil || dialer == nil {
		return
	}
	p.clientDialer = dialer
}

// setGuardBindingInvalidator wires the process-local WS state store to the
// transport pool. The callback is optional for standalone pool users.
func (p *openAIWSConnPool) setGuardBindingInvalidator(invalidator func(accountID int64, connID string)) {
	if p == nil {
		return
	}
	p.guardBindingInvalidatorMu.Lock()
	p.guardBindingInvalidator = invalidator
	p.guardBindingInvalidatorMu.Unlock()
}

func (p *openAIWSConnPool) notifyGuardBindingInvalidated(accountID int64, connID string) {
	if p == nil || accountID <= 0 || strings.TrimSpace(connID) == "" {
		return
	}
	p.guardBindingInvalidatorMu.RLock()
	invalidator := p.guardBindingInvalidator
	p.guardBindingInvalidatorMu.RUnlock()
	if invalidator != nil {
		invalidator(accountID, strings.TrimSpace(connID))
	}
}

// Close 停止后台 worker 并关闭所有空闲连接，应在优雅关闭时调用。
func (p *openAIWSConnPool) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		if p.workerStopCh != nil {
			close(p.workerStopCh)
		}
		p.workerWg.Wait()
		// 遍历所有账户池，关闭全部空闲连接。
		p.accounts.Range(func(key, value any) bool {
			ap, ok := value.(*openAIWSAccountPool)
			if !ok || ap == nil {
				return true
			}
			ap.mu.Lock()
			for _, conn := range ap.conns {
				if conn != nil && !conn.isLeased() {
					conn.close()
				}
			}
			ap.mu.Unlock()
			return true
		})
	})
}

func (p *openAIWSConnPool) startBackgroundWorkers() {
	if p == nil || p.workerStopCh == nil {
		return
	}
	p.workerWg.Add(2)
	go func() {
		defer p.workerWg.Done()
		p.runBackgroundPingWorker()
	}()
	go func() {
		defer p.workerWg.Done()
		p.runBackgroundCleanupWorker()
	}()
}

type openAIWSIdlePingCandidate struct {
	accountID int64
	conn      *openAIWSConn
}

func (p *openAIWSConnPool) runBackgroundPingWorker() {
	if p == nil {
		return
	}
	ticker := time.NewTicker(openAIWSBackgroundPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.runBackgroundPingSweep()
		case <-p.workerStopCh:
			return
		}
	}
}

func (p *openAIWSConnPool) runBackgroundPingSweep() {
	if p == nil {
		return
	}
	candidates := p.snapshotIdleConnsForPing()
	var g errgroup.Group
	g.SetLimit(10)
	for _, item := range candidates {
		item := item
		if item.conn == nil || item.conn.isLeased() || item.conn.waiters.Load() > 0 || !item.conn.supportsIdlePingWithoutReader() {
			continue
		}
		g.Go(func() error {
			if err := item.conn.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
				p.evictConn(item.accountID, item.conn.id)
			}
			return nil
		})
	}
	_ = g.Wait()
}

func (p *openAIWSConnPool) snapshotIdleConnsForPing() []openAIWSIdlePingCandidate {
	if p == nil {
		return nil
	}
	candidates := make([]openAIWSIdlePingCandidate, 0)
	p.accounts.Range(func(key, value any) bool {
		accountID, ok := key.(int64)
		if !ok || accountID <= 0 {
			return true
		}
		ap, ok := value.(*openAIWSAccountPool)
		if !ok || ap == nil {
			return true
		}
		ap.mu.Lock()
		for _, conn := range ap.conns {
			if conn == nil || conn.isLeased() || conn.waiters.Load() > 0 {
				continue
			}
			candidates = append(candidates, openAIWSIdlePingCandidate{
				accountID: accountID,
				conn:      conn,
			})
		}
		ap.mu.Unlock()
		return true
	})
	return candidates
}

func (p *openAIWSConnPool) runBackgroundCleanupWorker() {
	if p == nil {
		return
	}
	ticker := time.NewTicker(openAIWSBackgroundSweepTicker)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.runBackgroundCleanupSweep(time.Now())
		case <-p.workerStopCh:
			return
		}
	}
}

func (p *openAIWSConnPool) runBackgroundCleanupSweep(now time.Time) {
	if p == nil {
		return
	}
	type cleanupResult struct {
		evicted []*openAIWSConn
	}
	results := make([]cleanupResult, 0)
	p.accounts.Range(func(_ any, value any) bool {
		ap, ok := value.(*openAIWSAccountPool)
		if !ok || ap == nil {
			return true
		}
		maxConns := p.maxConnsHardCap()
		ap.mu.Lock()
		if ap.lastAcquire != nil && ap.lastAcquire.Account != nil {
			maxConns = p.effectiveMaxConnsByAccount(ap.lastAcquire.Account)
		}
		evicted := p.cleanupAccountLocked(ap, now, maxConns)
		ap.lastCleanupAt = now
		ap.mu.Unlock()
		if len(evicted) > 0 {
			results = append(results, cleanupResult{evicted: evicted})
		}
		return true
	})
	for _, result := range results {
		closeOpenAIWSConns(result.evicted)
	}
}

func (p *openAIWSConnPool) Acquire(ctx context.Context, req openAIWSAcquireRequest) (*openAIWSConnLease, error) {
	if p != nil {
		p.metrics.acquireTotal.Add(1)
	}
	return p.acquire(ctx, cloneOpenAIWSAcquireRequest(req), 0)
}

func (p *openAIWSConnPool) acquire(ctx context.Context, req openAIWSAcquireRequest, retry int) (*openAIWSConnLease, error) {
	if p == nil || req.Account == nil || req.Account.ID <= 0 {
		return nil, errors.New("invalid ws acquire request")
	}
	if stringsTrim(req.WSURL) == "" {
		return nil, errors.New("ws url is empty")
	}

retryAcquire:
	accountID := req.Account.ID
	compatibility := normalizeOpenAIWSHandshakeCompatibilityForRequest(req)
	routingAffinity := normalizeOpenAIWSRoutingAffinity(req.Headers)
	effectiveMaxConns := p.effectiveMaxConnsByAccount(req.Account)
	if effectiveMaxConns <= 0 {
		return nil, errOpenAIWSConnQueueFull
	}
	var evicted []*openAIWSConn
	ap := p.getOrCreateAccountPool(accountID)
	ap.mu.Lock()
	acquireGeneration := ap.generation
	now := time.Now()
	if ap.lastCleanupAt.IsZero() || now.Sub(ap.lastCleanupAt) >= openAIWSAcquireCleanupInterval {
		evicted = p.cleanupAccountLocked(ap, now, effectiveMaxConns)
		ap.lastCleanupAt = now
	}
	pickStartedAt := time.Now()
	allowReuse := !req.ForceNewConn
	preferredConnID := stringsTrim(req.PreferredConnID)
	forcePreferredConn := allowReuse && req.ForcePreferredConn

	if allowReuse {
		if forcePreferredConn {
			if preferredConnID == "" {
				p.recordConnPickDuration(time.Since(pickStartedAt))
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				return nil, errOpenAIWSPreferredConnUnavailable
			}
			preferredConn, ok := ap.conns[preferredConnID]
			guardPinned := ok && p.isPermanentGuardConnLocked(ap, preferredConnID)
			// A confirmed 429 guard keeps one already-negotiated socket alive.
			// Its handshake capability snapshot belongs to that socket, so a later
			// turn must not lose the exact continuation merely because the current
			// client no longer advertises an optional beta feature. Proxy and target
			// URL remain strict: the retained connection can never bypass the
			// account's configured network route or endpoint boundary.
			if !ok ||
				!preferredConn.matchesHandshakeIdentity(compatibility) ||
				(!guardPinned && !preferredConn.matchesHandshakeCompatibility(compatibility)) ||
				!preferredConn.matchesProxyURL(req.ProxyURL) ||
				!preferredConn.matchesWSURL(req.WSURL) {
				p.recordConnPickDuration(time.Since(pickStartedAt))
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				return nil, errOpenAIWSPreferredConnUnavailable
			}
			if preferredConn.tryAcquire() {
				connPick := time.Since(pickStartedAt)
				p.recordConnPickDuration(connPick)
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				if p.shouldHealthCheckConn(preferredConn) {
					if err := preferredConn.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
						preferredConn.close()
						p.evictConn(accountID, preferredConn.id)
						if retry < 1 {
							return p.acquire(ctx, req, retry+1)
						}
						return nil, err
					}
				}
				lease := &openAIWSConnLease{
					pool:      p,
					accountID: accountID,
					conn:      preferredConn,
					connPick:  connPick,
					reused:    true,
				}
				p.metrics.acquireReuseTotal.Add(1)
				p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
				p.ensureTargetIdleAsync(accountID)
				return lease, nil
			}

			connPick := time.Since(pickStartedAt)
			p.recordConnPickDuration(connPick)
			if int(preferredConn.waiters.Load()) >= p.queueLimitPerConn() {
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				return nil, errOpenAIWSConnQueueFull
			}
			preferredConn.waiters.Add(1)
			ap.mu.Unlock()
			closeOpenAIWSConns(evicted)
			defer preferredConn.waiters.Add(-1)
			waitStart := time.Now()
			p.metrics.acquireQueueWaitTotal.Add(1)

			if err := preferredConn.acquire(ctx); err != nil {
				if errors.Is(err, errOpenAIWSConnClosed) && retry < 1 {
					return p.acquire(ctx, req, retry+1)
				}
				return nil, err
			}
			if p.shouldHealthCheckConn(preferredConn) {
				if err := preferredConn.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
					preferredConn.release()
					preferredConn.close()
					p.evictConn(accountID, preferredConn.id)
					if retry < 1 {
						return p.acquire(ctx, req, retry+1)
					}
					return nil, err
				}
			}

			queueWait := time.Since(waitStart)
			p.metrics.acquireQueueWaitMs.Add(queueWait.Milliseconds())
			lease := &openAIWSConnLease{
				pool:      p,
				accountID: accountID,
				conn:      preferredConn,
				queueWait: queueWait,
				connPick:  connPick,
				reused:    true,
			}
			p.metrics.acquireReuseTotal.Add(1)
			p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
			p.ensureTargetIdleAsync(accountID)
			return lease, nil
		}

		if preferredConnID != "" {
			if conn, ok := ap.conns[preferredConnID]; ok && !p.isPermanentGuardConnLocked(ap, conn.id) && conn.matchesHandshakeCompatibility(compatibility) && conn.matchesProxyURL(req.ProxyURL) && conn.matchesWSURL(req.WSURL) && conn.tryAcquire() {
				connPick := time.Since(pickStartedAt)
				p.recordConnPickDuration(connPick)
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				if p.shouldHealthCheckConn(conn) {
					if err := conn.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
						conn.close()
						p.evictConn(accountID, conn.id)
						if retry < 1 {
							return p.acquire(ctx, req, retry+1)
						}
						return nil, err
					}
				}
				lease := &openAIWSConnLease{pool: p, accountID: accountID, conn: conn, connPick: connPick, reused: true}
				p.metrics.acquireReuseTotal.Add(1)
				p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
				p.ensureTargetIdleAsync(accountID)
				return lease, nil
			}
		}

		// A routing hint is advisory at WebSocket dial time. Prefer a pooled
		// connection whose handshake used the same hint, but do not make that
		// preference a continuation compatibility requirement.
		best := p.pickLeastBusyConnWithRoutingAffinityLocked(ap, compatibility, routingAffinity, req.ProxyURL, req.WSURL)
		if best != nil && best.tryAcquire() {
			connPick := time.Since(pickStartedAt)
			p.recordConnPickDuration(connPick)
			ap.mu.Unlock()
			closeOpenAIWSConns(evicted)
			if p.shouldHealthCheckConn(best) {
				if err := best.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
					best.close()
					p.evictConn(accountID, best.id)
					if retry < 1 {
						return p.acquire(ctx, req, retry+1)
					}
					return nil, err
				}
			}
			lease := &openAIWSConnLease{pool: p, accountID: accountID, conn: best, connPick: connPick, reused: true}
			p.metrics.acquireReuseTotal.Add(1)
			p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
			p.ensureTargetIdleAsync(accountID)
			return lease, nil
		}
		if routingAffinity == "" || len(ap.conns)+ap.creating >= effectiveMaxConns {
			for _, conn := range ap.conns {
				if conn == nil || conn == best || p.isPermanentGuardConnLocked(ap, conn.id) ||
					!conn.matchesHandshakeCompatibility(compatibility) ||
					!conn.matchesProxyURL(req.ProxyURL) ||
					!conn.matchesWSURL(req.WSURL) {
					continue
				}
				if conn.tryAcquire() {
					connPick := time.Since(pickStartedAt)
					p.recordConnPickDuration(connPick)
					ap.mu.Unlock()
					closeOpenAIWSConns(evicted)
					if p.shouldHealthCheckConn(conn) {
						if err := conn.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
							conn.close()
							p.evictConn(accountID, conn.id)
							if retry < 1 {
								return p.acquire(ctx, req, retry+1)
							}
							return nil, err
						}
					}
					lease := &openAIWSConnLease{pool: p, accountID: accountID, conn: conn, connPick: connPick, reused: true}
					p.metrics.acquireReuseTotal.Add(1)
					p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
					p.ensureTargetIdleAsync(accountID)
					return lease, nil
				}
			}
		}
	}

	if !req.ForceNewConn && len(ap.conns)+ap.creating >= effectiveMaxConns {
		affine := p.pickLeastBusyConnWithRoutingAffinityLocked(ap, compatibility, routingAffinity, req.ProxyURL, req.WSURL)
		if idle := p.pickOldestIdleConnWithoutHandshakeCompatibilityOrRoutingAffinityLocked(ap, compatibility, routingAffinity, req.ProxyURL, req.WSURL); idle != nil {
			delete(ap.conns, idle.id)
			evicted = append(evicted, idle)
			p.metrics.scaleDownTotal.Add(1)
		} else if affine == nil {
			compatible := p.pickLeastBusyConnLocked(ap, "", compatibility, req.ProxyURL, req.WSURL)
			if compatible != nil {
				// Capacity is full and every compatible connection is busy. The
				// hint remains soft here: queue on a compatible connection below.
				goto acquireAtCapacity
			}
			hasConnection := false
			for _, conn := range ap.conns {
				if conn != nil {
					hasConnection = true
					break
				}
			}
			if !hasConnection && ap.creating == 0 {
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				return nil, errOpenAIWSConnClosed
			}
			if !p.hasNonGuardConnLocked(ap) && p.hasPermanentGuardPinLocked(ap) {
				p.recordConnPickDuration(time.Since(pickStartedAt))
				ap.mu.Unlock()
				closeOpenAIWSConns(evicted)
				return nil, errOpenAIWSConnQueueFull
			}
			changedCh := ap.changeChannelLocked()
			ap.mu.Unlock()
			closeOpenAIWSConns(evicted)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changedCh:
				goto retryAcquire
			}
		}
	}

	if req.ForceNewConn && len(ap.conns)+ap.creating >= effectiveMaxConns {
		if idle := p.pickOldestIdleConnLocked(ap); idle != nil {
			delete(ap.conns, idle.id)
			evicted = append(evicted, idle)
			p.metrics.scaleDownTotal.Add(1)
		}
	}

	if len(ap.conns)+ap.creating < effectiveMaxConns {
		connPick := time.Since(pickStartedAt)
		p.recordConnPickDuration(connPick)
		ap.creating++
		ap.mu.Unlock()
		closeOpenAIWSConns(evicted)

		conn, dialErr := p.dialConn(ctx, req)

		ap = p.getOrCreateAccountPool(accountID)
		ap.mu.Lock()
		ap.creating--
		if ap.generation != acquireGeneration {
			ap.signalChangedLocked()
			ap.mu.Unlock()
			if conn != nil {
				conn.close()
			}
			if retry < 1 {
				return p.acquire(ctx, req, retry+1)
			}
			return nil, errOpenAIWSConnClosed
		}
		if dialErr != nil {
			ap.prewarmFails++
			ap.prewarmFailAt = time.Now()
			ap.signalChangedLocked()
			ap.mu.Unlock()
			return nil, dialErr
		}
		// Claim the freshly dialed connection before publishing it. Otherwise a
		// topology waiter awakened below can take the free semaphore first and
		// make the caller that paid for the dial queue behind it.
		if !conn.tryAcquire() {
			ap.signalChangedLocked()
			ap.mu.Unlock()
			conn.close()
			return nil, errOpenAIWSConnClosed
		}
		ap.conns[conn.id] = conn
		ap.prewarmFails = 0
		ap.prewarmFailAt = time.Time{}
		// Wake acquires that observed creating>0 with no compatible connection.
		// Without this signal they can remain asleep until the new lease is
		// released, even though the pool topology already changed.
		ap.signalChangedLocked()
		ap.mu.Unlock()
		p.metrics.acquireCreateTotal.Add(1)
		lease := &openAIWSConnLease{pool: p, accountID: accountID, conn: conn, connPick: connPick}
		p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
		p.ensureTargetIdleAsync(accountID)
		return lease, nil
	}

	if req.ForceNewConn {
		p.recordConnPickDuration(time.Since(pickStartedAt))
		ap.mu.Unlock()
		closeOpenAIWSConns(evicted)
		return nil, errOpenAIWSConnQueueFull
	}

acquireAtCapacity:
	target := p.pickLeastBusyConnLocked(ap, req.PreferredConnID, compatibility, req.ProxyURL, req.WSURL)
	connPick := time.Since(pickStartedAt)
	p.recordConnPickDuration(connPick)
	if target == nil {
		ap.mu.Unlock()
		closeOpenAIWSConns(evicted)
		return nil, errOpenAIWSConnClosed
	}
	if int(target.waiters.Load()) >= p.queueLimitPerConn() {
		ap.mu.Unlock()
		closeOpenAIWSConns(evicted)
		return nil, errOpenAIWSConnQueueFull
	}
	target.waiters.Add(1)
	ap.mu.Unlock()
	closeOpenAIWSConns(evicted)
	defer target.waiters.Add(-1)
	waitStart := time.Now()
	p.metrics.acquireQueueWaitTotal.Add(1)

	if err := target.acquire(ctx); err != nil {
		if errors.Is(err, errOpenAIWSConnClosed) && retry < 1 {
			return p.acquire(ctx, req, retry+1)
		}
		return nil, err
	}
	if p.shouldHealthCheckConn(target) {
		if err := target.pingWithTimeout(openAIWSConnHealthCheckTO); err != nil {
			target.release()
			target.close()
			p.evictConn(accountID, target.id)
			if retry < 1 {
				return p.acquire(ctx, req, retry+1)
			}
			return nil, err
		}
	}

	queueWait := time.Since(waitStart)
	p.metrics.acquireQueueWaitMs.Add(queueWait.Milliseconds())
	lease := &openAIWSConnLease{pool: p, accountID: accountID, conn: target, queueWait: queueWait, connPick: connPick, reused: true}
	p.metrics.acquireReuseTotal.Add(1)
	p.recordLastSuccessfulAcquire(accountID, acquireGeneration, req)
	p.ensureTargetIdleAsync(accountID)
	return lease, nil
}

func (p *openAIWSConnPool) recordConnPickDuration(duration time.Duration) {
	if p == nil {
		return
	}
	if duration < 0 {
		duration = 0
	}
	p.metrics.connPickTotal.Add(1)
	p.metrics.connPickMs.Add(duration.Milliseconds())
}

func (p *openAIWSConnPool) recordLastSuccessfulAcquire(accountID int64, generation uint64, req openAIWSAcquireRequest) {
	if p == nil || accountID <= 0 {
		return
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	if ap.generation != generation {
		ap.mu.Unlock()
		return
	}
	ap.lastAcquire = cloneOpenAIWSAcquireRequestPtr(&req)
	ap.mu.Unlock()
}

func (p *openAIWSConnPool) pickOldestIdleConnLocked(ap *openAIWSAccountPool) *openAIWSConn {
	if ap == nil || len(ap.conns) == 0 {
		return nil
	}
	var oldest *openAIWSConn
	for _, conn := range ap.conns {
		if conn == nil || conn.isLeased() || conn.waiters.Load() > 0 || p.isConnPinnedLocked(ap, conn.id) {
			continue
		}
		if oldest == nil || conn.lastUsedAt().Before(oldest.lastUsedAt()) {
			oldest = conn
		}
	}
	return oldest
}

func (p *openAIWSConnPool) pickOldestIdleConnWithoutHandshakeCompatibilityOrRoutingAffinityLocked(
	ap *openAIWSAccountPool,
	compatibility openAIWSHandshakeCompatibilityKey,
	_ string,
	proxyURL string,
	wsURL string,
) *openAIWSConn {
	if ap == nil || len(ap.conns) == 0 {
		return nil
	}
	var oldest *openAIWSConn
	for _, conn := range ap.conns {
		if conn == nil ||
			(conn.matchesHandshakeCompatibility(compatibility) && conn.matchesProxyURL(proxyURL) && conn.matchesWSURL(wsURL)) ||
			conn.isLeased() || conn.waiters.Load() > 0 {
			continue
		}
		// A guard pin protects a still-valid upstream identity. If the account's
		// proxy or WS target changed, that identity is stale and must not block
		// a replacement dial indefinitely; only matching targets remain pinned.
		if p.isConnPinnedLocked(ap, conn.id) && conn.matchesProxyURL(proxyURL) && conn.matchesWSURL(wsURL) {
			continue
		}
		if oldest == nil || conn.lastUsedAt().Before(oldest.lastUsedAt()) {
			oldest = conn
		}
	}
	return oldest
}

func (p *openAIWSConnPool) getOrCreateAccountPool(accountID int64) *openAIWSAccountPool {
	if p == nil || accountID <= 0 {
		return nil
	}
	if existing, ok := p.accounts.Load(accountID); ok {
		if ap, typed := existing.(*openAIWSAccountPool); typed && ap != nil {
			return ap
		}
	}
	ap := &openAIWSAccountPool{
		conns:            make(map[string]*openAIWSConn),
		pinnedConns:      make(map[string]int),
		guardPinnedUntil: make(map[string]time.Time),
		changedCh:        make(chan struct{}),
	}
	actual, _ := p.accounts.LoadOrStore(accountID, ap)
	if typed, ok := actual.(*openAIWSAccountPool); ok && typed != nil {
		return typed
	}
	return ap
}

// ensureAccountPoolLocked 兼容旧调用。
func (p *openAIWSConnPool) ensureAccountPoolLocked(accountID int64) *openAIWSAccountPool {
	return p.getOrCreateAccountPool(accountID)
}

func (p *openAIWSConnPool) getAccountPool(accountID int64) (*openAIWSAccountPool, bool) {
	if p == nil || accountID <= 0 {
		return nil, false
	}
	value, ok := p.accounts.Load(accountID)
	if !ok || value == nil {
		return nil, false
	}
	ap, typed := value.(*openAIWSAccountPool)
	return ap, typed && ap != nil
}

func (p *openAIWSConnPool) notifyAccountPoolChanged(accountID int64) {
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	ap.signalChangedLocked()
	ap.mu.Unlock()
}

func (p *openAIWSConnPool) isConnPinnedLocked(ap *openAIWSAccountPool, connID string) bool {
	if ap == nil || connID == "" {
		return false
	}
	if ap.pinnedConns[connID] > 0 {
		return true
	}
	if until, ok := ap.guardPinnedUntil[connID]; ok {
		// A zero expiry is a deliberate permanent guard pin. It is released
		// only when the connection is evicted/closed or the account pool is
		// explicitly invalidated.
		if until.IsZero() || time.Now().Before(until) {
			return true
		}
		delete(ap.guardPinnedUntil, connID)
	}
	return false
}

// isPermanentGuardConnLocked reports the sentinel pin used by a confirmed
// Codex 429 continuation. Normal Acquire paths must never reuse this socket:
// only a ForcePreferredConn continuation with the exact local binding may use
// it, otherwise unrelated clients can inherit upstream conversation state.
func (p *openAIWSConnPool) isPermanentGuardConnLocked(ap *openAIWSAccountPool, connID string) bool {
	if ap == nil || connID == "" {
		return false
	}
	until, pinned := ap.guardPinnedUntil[connID]
	return pinned && until.IsZero()
}

func (p *openAIWSConnPool) hasNonGuardConnLocked(ap *openAIWSAccountPool) bool {
	if ap == nil {
		return false
	}
	for connID, conn := range ap.conns {
		if conn != nil && !p.isPermanentGuardConnLocked(ap, connID) {
			return true
		}
	}
	return false
}

// hasPermanentGuardPinLocked reports whether an account still has a live
// confirmed-429 socket. Expired TTL pins do not suppress prewarming; stale
// permanent entries are removed here so an evicted socket cannot strand the
// account's creation budget indefinitely.
func (p *openAIWSConnPool) hasPermanentGuardPinLocked(ap *openAIWSAccountPool) bool {
	if ap == nil || len(ap.guardPinnedUntil) == 0 {
		return false
	}
	removed := false
	for connID, until := range ap.guardPinnedUntil {
		if !until.IsZero() {
			continue
		}
		conn, exists := ap.conns[connID]
		if !exists || conn == nil {
			delete(ap.guardPinnedUntil, connID)
			removed = true
			continue
		}
		select {
		case <-conn.closedCh:
			delete(ap.guardPinnedUntil, connID)
			removed = true
		default:
			return true
		}
	}
	if removed {
		ap.signalChangedLocked()
	}
	return false
}

// HasPermanentGuardPin reports whether an account has a live connection
// reserved by the confirmed Codex OAuth 429 guard. The caller still needs the
// exact local connection identifier and ForcePreferredConn to acquire it.
func (p *openAIWSConnPool) HasPermanentGuardPin(accountID int64) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return p.hasPermanentGuardPinLocked(ap)
}

func (p *openAIWSConnPool) cleanupAccountLocked(ap *openAIWSAccountPool, now time.Time, maxConns int) []*openAIWSConn {
	if ap == nil {
		return nil
	}
	maxAge := p.maxConnAge()

	evicted := make([]*openAIWSConn, 0)
	for id, conn := range ap.conns {
		if conn == nil {
			delete(ap.conns, id)
			if len(ap.pinnedConns) > 0 {
				delete(ap.pinnedConns, id)
			}
			if len(ap.guardPinnedUntil) > 0 {
				delete(ap.guardPinnedUntil, id)
			}
			continue
		}
		select {
		case <-conn.closedCh:
			delete(ap.conns, id)
			if len(ap.pinnedConns) > 0 {
				delete(ap.pinnedConns, id)
			}
			if len(ap.guardPinnedUntil) > 0 {
				delete(ap.guardPinnedUntil, id)
			}
			evicted = append(evicted, conn)
			continue
		default:
		}
		if p.isConnPinnedLocked(ap, id) {
			continue
		}
		if maxAge > 0 && !conn.isLeased() && conn.age(now) > maxAge {
			delete(ap.conns, id)
			if len(ap.pinnedConns) > 0 {
				delete(ap.pinnedConns, id)
			}
			if len(ap.guardPinnedUntil) > 0 {
				delete(ap.guardPinnedUntil, id)
			}
			evicted = append(evicted, conn)
		}
	}

	if maxConns <= 0 {
		maxConns = p.maxConnsHardCap()
	}
	maxIdle := p.maxIdlePerAccount()
	if maxIdle < 0 || maxIdle > maxConns {
		maxIdle = maxConns
	}
	if maxIdle >= 0 && len(ap.conns) > maxIdle {
		idleConns := make([]*openAIWSConn, 0, len(ap.conns))
		for id, conn := range ap.conns {
			if conn == nil {
				delete(ap.conns, id)
				if len(ap.pinnedConns) > 0 {
					delete(ap.pinnedConns, id)
				}
				if len(ap.guardPinnedUntil) > 0 {
					delete(ap.guardPinnedUntil, id)
				}
				continue
			}
			// 有等待者的连接不能在清理阶段被淘汰，否则等待中的 acquire 会收到 closed 错误。
			if conn.isLeased() || conn.waiters.Load() > 0 || p.isConnPinnedLocked(ap, conn.id) {
				continue
			}
			idleConns = append(idleConns, conn)
		}
		sort.SliceStable(idleConns, func(i, j int) bool {
			return idleConns[i].lastUsedAt().Before(idleConns[j].lastUsedAt())
		})
		redundant := len(ap.conns) - maxIdle
		if redundant > len(idleConns) {
			redundant = len(idleConns)
		}
		for i := 0; i < redundant; i++ {
			conn := idleConns[i]
			delete(ap.conns, conn.id)
			if len(ap.pinnedConns) > 0 {
				delete(ap.pinnedConns, conn.id)
			}
			if len(ap.guardPinnedUntil) > 0 {
				delete(ap.guardPinnedUntil, conn.id)
			}
			evicted = append(evicted, conn)
		}
		if redundant > 0 {
			p.metrics.scaleDownTotal.Add(int64(redundant))
		}
	}
	if len(evicted) > 0 {
		ap.signalChangedLocked()
	}

	return evicted
}

func (p *openAIWSConnPool) pickLeastBusyConnLocked(
	ap *openAIWSAccountPool,
	preferredConnID string,
	compatibility openAIWSHandshakeCompatibilityKey,
	proxyURL string,
	wsURL string,
) *openAIWSConn {
	if ap == nil || len(ap.conns) == 0 {
		return nil
	}
	preferredConnID = stringsTrim(preferredConnID)
	if preferredConnID != "" {
		if conn, ok := ap.conns[preferredConnID]; ok && !p.isPermanentGuardConnLocked(ap, conn.id) && conn.matchesHandshakeCompatibility(compatibility) && conn.matchesProxyURL(proxyURL) && conn.matchesWSURL(wsURL) {
			return conn
		}
	}
	var best *openAIWSConn
	var bestWaiters int32
	var bestLastUsed time.Time
	for _, conn := range ap.conns {
		if conn == nil || p.isPermanentGuardConnLocked(ap, conn.id) || !conn.matchesHandshakeCompatibility(compatibility) || !conn.matchesProxyURL(proxyURL) || !conn.matchesWSURL(wsURL) {
			continue
		}
		waiters := conn.waiters.Load()
		lastUsed := conn.lastUsedAt()
		if best == nil ||
			waiters < bestWaiters ||
			(waiters == bestWaiters && lastUsed.Before(bestLastUsed)) {
			best = conn
			bestWaiters = waiters
			bestLastUsed = lastUsed
		}
	}
	return best
}

func (p *openAIWSConnPool) pickLeastBusyConnWithRoutingAffinityLocked(
	ap *openAIWSAccountPool,
	compatibility openAIWSHandshakeCompatibilityKey,
	routingAffinity string,
	proxyURL string,
	wsURL string,
) *openAIWSConn {
	if ap == nil || len(ap.conns) == 0 {
		return nil
	}
	var best *openAIWSConn
	var bestWaiters int32
	var bestLastUsed time.Time
	for _, conn := range ap.conns {
		if conn == nil ||
			p.isPermanentGuardConnLocked(ap, conn.id) ||
			!conn.matchesHandshakeCompatibility(compatibility) ||
			!conn.matchesProxyURL(proxyURL) ||
			!conn.matchesWSURL(wsURL) ||
			!conn.matchesRoutingAffinity(routingAffinity) {
			continue
		}
		waiters := conn.waiters.Load()
		lastUsed := conn.lastUsedAt()
		if best == nil ||
			waiters < bestWaiters ||
			(waiters == bestWaiters && lastUsed.Before(bestLastUsed)) {
			best = conn
			bestWaiters = waiters
			bestLastUsed = lastUsed
		}
	}
	return best
}

func accountPoolLoadLocked(ap *openAIWSAccountPool) (inflight int, waiters int) {
	if ap == nil {
		return 0, 0
	}
	for _, conn := range ap.conns {
		if conn == nil {
			continue
		}
		if conn.isLeased() {
			inflight++
		}
		waiters += int(conn.waiters.Load())
	}
	return inflight, waiters
}

// AccountPoolLoad 返回指定账号连接池的并发与排队快照。
func (p *openAIWSConnPool) AccountPoolLoad(accountID int64) (inflight int, waiters int, conns int) {
	if p == nil || accountID <= 0 {
		return 0, 0, 0
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return 0, 0, 0
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	inflight, waiters = accountPoolLoadLocked(ap)
	return inflight, waiters, len(ap.conns)
}

func (p *openAIWSConnPool) ensureTargetIdleAsync(accountID int64) {
	if p == nil || accountID <= 0 {
		return
	}

	var req openAIWSAcquireRequest
	generation := uint64(0)
	need := 0
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.lastAcquire == nil {
		return
	}
	if p.hasPermanentGuardPinLocked(ap) {
		return
	}
	if ap.prewarmActive {
		return
	}
	now := time.Now()
	if !ap.prewarmUntil.IsZero() && now.Before(ap.prewarmUntil) {
		return
	}
	if p.shouldSuppressPrewarmLocked(ap, now) {
		return
	}
	effectiveMaxConns := p.maxConnsHardCap()
	if ap.lastAcquire != nil && ap.lastAcquire.Account != nil {
		effectiveMaxConns = p.effectiveMaxConnsByAccount(ap.lastAcquire.Account)
	}
	target := p.targetConnCountLocked(ap, effectiveMaxConns)
	current := len(ap.conns) + ap.creating
	if current >= target {
		return
	}
	need = target - current
	if need <= 0 {
		return
	}
	req = cloneOpenAIWSAcquireRequest(*ap.lastAcquire)
	generation = ap.generation
	ap.prewarmActive = true
	if cooldown := p.prewarmCooldown(); cooldown > 0 {
		ap.prewarmUntil = now.Add(cooldown)
	}
	ap.creating += need
	p.metrics.scaleUpTotal.Add(int64(need))

	go p.prewarmConns(accountID, req, need, generation)
}

func (p *openAIWSConnPool) targetConnCountLocked(ap *openAIWSAccountPool, maxConns int) int {
	if ap == nil {
		return 0
	}

	if maxConns <= 0 {
		return 0
	}

	minIdle := p.minIdlePerAccount()
	if minIdle < 0 {
		minIdle = 0
	}
	if minIdle > maxConns {
		minIdle = maxConns
	}

	inflight, waiters := accountPoolLoadLocked(ap)
	utilization := p.targetUtilization()
	demand := inflight + waiters
	if demand <= 0 {
		return minIdle
	}

	target := 1
	if demand > 1 {
		target = int(math.Ceil(float64(demand) / utilization))
	}
	if waiters > 0 && target < len(ap.conns)+1 {
		target = len(ap.conns) + 1
	}
	if target < minIdle {
		target = minIdle
	}
	if target > maxConns {
		target = maxConns
	}
	return target
}

func (p *openAIWSConnPool) prewarmConns(accountID int64, req openAIWSAcquireRequest, total int, generations ...uint64) {
	generation := uint64(0)
	if len(generations) > 0 {
		generation = generations[0]
	}
	staleTarget := false
	remainingReservations := total
	releaseReservationsLocked := func(ap *openAIWSAccountPool) {
		if ap == nil || remainingReservations <= 0 {
			return
		}
		// ensureTargetIdleAsync reserves all requested slots up front. If a
		// guard pin appears while a dial is in flight, release every slot that
		// has not yet been consumed; otherwise the account can remain stuck at
		// `creating > 0` and suppress future legitimate acquires.
		if ap.creating >= remainingReservations {
			ap.creating -= remainingReservations
		} else {
			ap.creating = 0
		}
		remainingReservations = 0
	}
	defer func() {
		if ap, ok := p.getAccountPool(accountID); ok && ap != nil {
			ap.mu.Lock()
			releaseReservationsLocked(ap)
			ap.prewarmActive = false
			ap.signalChangedLocked()
			ap.mu.Unlock()
		}
		if staleTarget {
			// A newer acquire arrived while the old dial was in flight. Re-run
			// target selection only after clearing prewarmActive so the latest
			// beta/hint target can fill the idle budget.
			p.ensureTargetIdleAsync(accountID)
		}
	}()

	for i := 0; i < total; i++ {
		// Avoid starting any new dial after a confirmed guard socket exists.
		// The in-flight result is checked again below, because the pin can be
		// established while dialConn is blocked in the handshake.
		if ap, ok := p.getAccountPool(accountID); ok && ap != nil {
			ap.mu.Lock()
			guardPinned := p.hasPermanentGuardPinLocked(ap)
			if guardPinned {
				releaseReservationsLocked(ap)
				ap.signalChangedLocked()
			}
			ap.mu.Unlock()
			if guardPinned {
				return
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout()+openAIWSConnPrewarmExtraDelay)
		conn, err := p.dialConn(ctx, req)
		cancel()

		ap, ok := p.getAccountPool(accountID)
		if !ok || ap == nil {
			if conn != nil {
				conn.close()
			}
			return
		}
		ap.mu.Lock()
		if remainingReservations > 0 {
			if ap.creating > 0 {
				ap.creating--
			}
			remainingReservations--
		}
		if err != nil {
			ap.prewarmFails++
			ap.prewarmFailAt = time.Now()
			ap.signalChangedLocked()
			ap.mu.Unlock()
			continue
		}
		if p.hasPermanentGuardPinLocked(ap) {
			ap.signalChangedLocked()
			ap.mu.Unlock()
			conn.close()
			return
		}
		if ap.generation != generation || ap.lastAcquire == nil {
			ap.mu.Unlock()
			conn.close()
			continue
		}
		if !sameOpenAIWSPrewarmTarget(req, *ap.lastAcquire) {
			staleTarget = true
			ap.signalChangedLocked()
			ap.mu.Unlock()
			conn.close()
			continue
		}
		if len(ap.conns) >= p.effectiveMaxConnsByAccount(req.Account) {
			ap.signalChangedLocked()
			ap.mu.Unlock()
			conn.close()
			continue
		}
		ap.conns[conn.id] = conn
		ap.prewarmFails = 0
		ap.prewarmFailAt = time.Time{}
		ap.signalChangedLocked()
		ap.mu.Unlock()
	}
}

// ClearAccount closes all pooled connections and discards delayed prewarm
// state for one account. The generation guard prevents an in-flight prewarm
// started before credential recovery from re-entering the pool afterwards.
func (p *openAIWSConnPool) ClearAccount(accountID int64) {
	if p == nil || accountID <= 0 {
		return
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	ap.generation++
	conns := make([]*openAIWSConn, 0, len(ap.conns))
	for id, conn := range ap.conns {
		delete(ap.conns, id)
		delete(ap.pinnedConns, id)
		delete(ap.guardPinnedUntil, id)
		if conn != nil {
			conns = append(conns, conn)
		}
	}
	ap.lastAcquire = nil
	ap.prewarmUntil = time.Time{}
	ap.prewarmFails = 0
	ap.prewarmFailAt = time.Time{}
	ap.signalChangedLocked()
	ap.mu.Unlock()
	closeOpenAIWSConns(conns)
}

func (p *openAIWSConnPool) evictConn(accountID int64, connID string) {
	if p == nil || accountID <= 0 || stringsTrim(connID) == "" {
		return
	}
	var conn *openAIWSConn
	ap, ok := p.getAccountPool(accountID)
	if ok && ap != nil {
		ap.mu.Lock()
		if c, exists := ap.conns[connID]; exists {
			conn = c
			delete(ap.conns, connID)
			if len(ap.pinnedConns) > 0 {
				delete(ap.pinnedConns, connID)
			}
			if len(ap.guardPinnedUntil) > 0 {
				delete(ap.guardPinnedUntil, connID)
			}
			ap.signalChangedLocked()
		}
		ap.mu.Unlock()
	}
	if conn != nil {
		conn.close()
	}
}

func (p *openAIWSConnPool) PinConn(accountID int64, connID string) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if _, exists := ap.conns[connID]; !exists {
		return false
	}
	if ap.pinnedConns == nil {
		ap.pinnedConns = make(map[string]int)
	}
	ap.pinnedConns[connID]++
	return true
}

// PinConnUntil keeps a guard continuation connection out of idle cleanup until
// the response sticky binding expires. It is separate from session pins so a
// normal unpin cannot release a guard pin early.
func (p *openAIWSConnPool) PinConnUntil(accountID int64, connID string, until time.Time) bool {
	if p == nil || accountID <= 0 || until.IsZero() || !until.After(time.Now()) {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if _, exists := ap.conns[connID]; !exists {
		return false
	}
	if ap.guardPinnedUntil == nil {
		ap.guardPinnedUntil = make(map[string]time.Time)
	}
	if previous, exists := ap.guardPinnedUntil[connID]; !exists || (!previous.IsZero() && until.After(previous)) {
		ap.guardPinnedUntil[connID] = until
	}
	return true
}

func markGuardConnConfirmedLocked(ap *openAIWSAccountPool, connID string, generation uint64) bool {
	if ap == nil || generation == 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	conn, exists := ap.conns[connID]
	if !exists || conn == nil {
		return false
	}
	select {
	case <-conn.closedCh:
		return false
	default:
	}
	conn.guardConfirmed429Generation.Store(generation)
	return true
}

// MarkGuardConnConfirmed records positive evidence on one exact pooled socket.
// A generation of zero is never valid. The proof is intentionally stored on
// the connection rather than in account/session state so a later ordinary
// response binding cannot promote a newly dialed socket into the guard route.
func (p *openAIWSConnPool) MarkGuardConnConfirmed(accountID int64, connID string, generation uint64) bool {
	if p == nil || accountID <= 0 || generation == 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return markGuardConnConfirmedLocked(ap, connID, generation)
}

// MarkExistingConnsAs429GuardCandidates records the boundary between old and
// new sockets for one account. It is called immediately before a confirmed
// Codex OAuth 429 runtime block becomes visible. The pool lock makes a dial
// published afterwards ineligible, even if another Acquire later reuses it.
// A zero generation is ignored so callers cannot create unscoped proof.
func (p *openAIWSConnPool) MarkExistingConnsAs429GuardCandidates(accountID int64, generations ...uint64) {
	p.markExistingConnsAs429GuardCandidatesAt(accountID, time.Time{}, generations...)
}

// markExistingConnsAs429GuardCandidatesAt optionally applies a creation-time
// cutoff. The runtime blocker uses the cutoff captured before it waits on the
// pool mutex, so a socket created during that handoff cannot be promoted into
// the old-connection guard merely because it published before the pool lock
// became available.
func (p *openAIWSConnPool) markExistingConnsAs429GuardCandidatesAt(accountID int64, cutoff time.Time, generations ...uint64) {
	if p == nil || accountID <= 0 || len(generations) == 0 || generations[0] == 0 {
		return
	}
	generation := generations[0]
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	for _, conn := range ap.conns {
		if conn == nil {
			continue
		}
		select {
		case <-conn.closedCh:
			continue
		default:
			if !cutoff.IsZero() {
				createdAt := conn.createdAt()
				if !createdAt.IsZero() && createdAt.After(cutoff) {
					continue
				}
			}
			conn.guard429CandidateGeneration.Store(generation)
		}
	}
}

// MarkAndPinGuardConnConfirmed atomically records the proof and installs the
// permanent pin under one account-pool lock. This closes the small handoff
// window where a caller could otherwise mark a socket and then lose the
// generation before a separate pin operation.
func (p *openAIWSConnPool) MarkAndPinGuardConnConfirmed(accountID int64, connID string, generation uint64) bool {
	if p == nil || accountID <= 0 || generation == 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	conn, exists := ap.conns[connID]
	if !exists || conn == nil || conn.guard429CandidateGeneration.Load() != generation {
		return false
	}
	if !markGuardConnConfirmedLocked(ap, connID, generation) {
		return false
	}
	return p.pinGuardConnLocked(ap, connID, generation, true)
}

func (p *openAIWSConnPool) pinGuardConnLocked(ap *openAIWSAccountPool, connID string, generation uint64, requireProof bool) bool {
	if ap == nil || stringsTrim(connID) == "" {
		return false
	}
	connID = stringsTrim(connID)
	conn, exists := ap.conns[connID]
	if !exists || conn == nil {
		return false
	}
	select {
	case <-conn.closedCh:
		return false
	default:
	}
	if requireProof && (generation == 0 || conn.guardConfirmed429Generation.Load() != generation) {
		return false
	}
	if ap.guardPinnedUntil == nil {
		ap.guardPinnedUntil = make(map[string]time.Time)
	}
	// A Codex account has one retained 429 continuation socket. Keep the
	// first live permanent pin; a replacement connection must fail over rather
	// than silently creating a second long-lived route for the same account.
	for existingID, until := range ap.guardPinnedUntil {
		if existingID == connID || !until.IsZero() {
			continue
		}
		existing, exists := ap.conns[existingID]
		if !exists || existing == nil {
			delete(ap.guardPinnedUntil, existingID)
			continue
		}
		select {
		case <-existing.closedCh:
			delete(ap.guardPinnedUntil, existingID)
		default:
			return false
		}
	}
	// time.Time{} is the permanent-pin sentinel. Do not downgrade an
	// already-permanent pin if an older caller still supplies a TTL.
	ap.guardPinnedUntil[connID] = time.Time{}
	ap.signalChangedLocked()
	return true
}

// PinGuardConnForGeneration permanently protects a socket only after the
// socket itself has recorded the same confirmed-429 generation.
func (p *openAIWSConnPool) PinGuardConnForGeneration(accountID int64, connID string, generation uint64) bool {
	if p == nil || accountID <= 0 || generation == 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return p.pinGuardConnLocked(ap, connID, generation, true)
}

// PinGuardConn is retained for narrow legacy/test callers that already have
// an independently validated connection. Production 429 handling uses
// PinGuardConnForGeneration, which requires the per-connection proof.
func (p *openAIWSConnPool) PinGuardConn(accountID int64, connID string) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return p.pinGuardConnLocked(ap, connID, 0, false)
}

// detachGuardConns removes every connection retained by the 429 guard for one
// account and returns them for closing by the caller. Callers that transition
// account runtime state should invoke this while holding the per-account
// runtime lock, then close the returned sockets after releasing that lock.
func (p *openAIWSConnPool) detachGuardConns(accountID int64) []*openAIWSConn {
	if p == nil || accountID <= 0 {
		return nil
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return nil
	}
	ap.mu.Lock()
	toClose := make([]*openAIWSConn, 0, len(ap.guardPinnedUntil))
	changed := false
	for connID, until := range ap.guardPinnedUntil {
		if !until.IsZero() {
			// PinConnUntil is a normal TTL retention mechanism. A confirmed-429
			// failure must only detach permanent guard reservations.
			continue
		}
		changed = true
		delete(ap.guardPinnedUntil, connID)
		if len(ap.pinnedConns) > 0 {
			delete(ap.pinnedConns, connID)
		}
		if conn, exists := ap.conns[connID]; exists {
			delete(ap.conns, connID)
			if conn != nil {
				toClose = append(toClose, conn)
			}
		}
	}
	if changed {
		ap.generation++
		ap.signalChangedLocked()
	}
	ap.mu.Unlock()
	return toClose
}

// InvalidateGuardConns removes every connection retained by the 429 guard for
// one account. It is used when a stronger non-429 account failure is observed:
// the old socket must not be reused after the failure, even if its lease is
// otherwise healthy. Connections are detached under the pool lock and closed
// afterwards so close callbacks never run while the pool mutex is held.
func (p *openAIWSConnPool) InvalidateGuardConns(accountID int64) {
	toClose := p.detachGuardConns(accountID)
	closeOpenAIWSConns(toClose)
}

// IsGuardConnPinned reports whether the exact connection is still present and
// permanently retained by the 429 guard. A normal session pin or an expired
// TTL pin does not satisfy this check.
func (p *openAIWSConnPool) IsGuardConnPinned(accountID int64, connID string) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	conn, exists := ap.conns[connID]
	if !exists || conn == nil {
		return false
	}
	select {
	case <-conn.closedCh:
		return false
	default:
	}
	until, pinned := ap.guardPinnedUntil[connID]
	if !pinned {
		return false
	}
	if until.IsZero() || time.Now().Before(until) {
		return until.IsZero()
	}
	delete(ap.guardPinnedUntil, connID)
	ap.signalChangedLocked()
	return false
}

// PermanentGuardConnID returns the single live socket retained by the Codex
// 429 guard, when one exists.  The guard deliberately permits only one
// permanent reservation per account; exposing its id lets the request-level
// forwarder continue an already-selected account even when an HTTP request
// does not carry a response_id/session marker.  Callers must still set
// ForcePreferredConn so an ordinary pool acquire can never borrow the socket.
func (p *openAIWSConnPool) PermanentGuardConnID(accountID int64) (string, bool) {
	if p == nil || accountID <= 0 {
		return "", false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return "", false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	now := time.Now()
	for connID, until := range ap.guardPinnedUntil {
		// A zero expiry is the permanent guard sentinel.  TTL pins are normal
		// sticky retention and must not become a 429 continuation route.
		if !until.IsZero() {
			if !now.Before(until) {
				delete(ap.guardPinnedUntil, connID)
				ap.signalChangedLocked()
			}
			continue
		}
		conn, exists := ap.conns[connID]
		if !exists || conn == nil {
			delete(ap.guardPinnedUntil, connID)
			ap.signalChangedLocked()
			continue
		}
		select {
		case <-conn.closedCh:
			delete(ap.guardPinnedUntil, connID)
			ap.signalChangedLocked()
			continue
		default:
			return connID, true
		}
	}
	return "", false
}

// IsGuardConnCandidate reports whether the exact live socket existed when the
// current guard transition began. A pinned socket is also a candidate. This is
// intentionally distinct from ordinary session pins and is used only while a
// confirming 429 event is being classified.
func (p *openAIWSConnPool) IsGuardConnCandidate(accountID int64, connID string, generations ...uint64) bool {
	if p == nil || accountID <= 0 {
		return false
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	conn, exists := ap.conns[connID]
	if !exists || conn == nil {
		return false
	}
	select {
	case <-conn.closedCh:
		return false
	default:
	}
	candidateGeneration := conn.guard429CandidateGeneration.Load()
	if len(generations) > 0 && generations[0] != 0 {
		return candidateGeneration == generations[0]
	}
	if candidateGeneration != 0 {
		return true
	}
	until, pinned := ap.guardPinnedUntil[connID]
	return pinned && until.IsZero()
}

func (p *openAIWSConnPool) UnpinConn(accountID int64, connID string) {
	if p == nil || accountID <= 0 {
		return
	}
	connID = stringsTrim(connID)
	if connID == "" {
		return
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if len(ap.pinnedConns) == 0 {
		return
	}
	count := ap.pinnedConns[connID]
	if count <= 1 {
		delete(ap.pinnedConns, connID)
		ap.signalChangedLocked()
		return
	}
	ap.pinnedConns[connID] = count - 1
	ap.signalChangedLocked()
}

func (p *openAIWSConnPool) dialConn(ctx context.Context, req openAIWSAcquireRequest) (*openAIWSConn, error) {
	if p == nil || p.clientDialer == nil {
		return nil, errors.New("openai ws client dialer is nil")
	}
	headers := cloneHeader(req.Headers)
	var err error
	if req.HeadersFactory != nil {
		headers, err = req.HeadersFactory(ctx, headers)
		if err != nil {
			return nil, err
		}
	}
	conn, status, handshakeHeaders, err := p.clientDialer.Dial(ctx, req.WSURL, headers, req.ProxyURL)
	if err != nil {
		var handshakeErr *openAIWSHandshakeError
		var responseBody []byte
		if errors.As(err, &handshakeErr) && handshakeErr != nil {
			responseBody = append([]byte(nil), handshakeErr.Body...)
		}
		return nil, &openAIWSDialError{
			StatusCode:      status,
			ResponseHeaders: cloneHeader(handshakeHeaders),
			ResponseBody:    responseBody,
			Err:             err,
		}
	}
	if conn == nil {
		return nil, &openAIWSDialError{
			StatusCode:      status,
			ResponseHeaders: cloneHeader(handshakeHeaders),
			Err:             errors.New("openai ws dialer returned nil connection"),
		}
	}
	id := p.nextConnID(req.Account.ID)
	pooledConn := newOpenAIWSConn(id, req.Account.ID, conn, handshakeHeaders)
	pooledConn.onClose = p.notifyGuardBindingInvalidated
	pooledConn.handshakeCompatibility = normalizeOpenAIWSHandshakeCompatibilityForRequest(req)
	pooledConn.routingAffinity = normalizeOpenAIWSRoutingAffinity(req.Headers)
	pooledConn.proxyURL = normalizeOpenAIWSProxyURL(req.ProxyURL)
	pooledConn.wsURL = normalizeOpenAIWSURL(req.WSURL)
	pooledConn.proxyURLKnown = true
	pooledConn.wsURLKnown = true
	return pooledConn, nil
}

func (p *openAIWSConnPool) nextConnID(accountID int64) string {
	seq := p.seq.Add(1)
	buf := make([]byte, 0, 32)
	buf = append(buf, "oa_ws_"...)
	buf = strconv.AppendInt(buf, accountID, 10)
	buf = append(buf, '_')
	buf = strconv.AppendUint(buf, seq, 10)
	return string(buf)
}

func (p *openAIWSConnPool) shouldHealthCheckConn(conn *openAIWSConn) bool {
	if conn == nil || !conn.supportsIdlePingWithoutReader() {
		return false
	}
	return conn.idleDuration(time.Now()) >= openAIWSConnHealthCheckIdle
}

func (p *openAIWSConnPool) maxConnsHardCap() int {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.MaxConnsPerAccount > 0 {
		return p.cfg.Gateway.OpenAIWS.MaxConnsPerAccount
	}
	return 8
}

func (p *openAIWSConnPool) dynamicMaxConnsEnabled() bool {
	if p != nil && p.cfg != nil {
		return p.cfg.Gateway.OpenAIWS.DynamicMaxConnsByAccountConcurrencyEnabled
	}
	return false
}

func (p *openAIWSConnPool) modeRouterV2Enabled() bool {
	if p != nil && p.cfg != nil {
		return p.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled
	}
	return false
}

func (p *openAIWSConnPool) maxConnsFactorByAccount(account *Account) float64 {
	if p == nil || p.cfg == nil || account == nil {
		return 1.0
	}
	switch account.Type {
	case AccountTypeOAuth:
		if p.cfg.Gateway.OpenAIWS.OAuthMaxConnsFactor > 0 {
			return p.cfg.Gateway.OpenAIWS.OAuthMaxConnsFactor
		}
	case AccountTypeAPIKey:
		if p.cfg.Gateway.OpenAIWS.APIKeyMaxConnsFactor > 0 {
			return p.cfg.Gateway.OpenAIWS.APIKeyMaxConnsFactor
		}
	}
	return 1.0
}

func (p *openAIWSConnPool) effectiveMaxConnsByAccount(account *Account) int {
	hardCap := p.maxConnsHardCap()
	if hardCap <= 0 {
		return 0
	}
	if p.modeRouterV2Enabled() {
		if account == nil {
			return hardCap
		}
		if account.Concurrency <= 0 {
			return 0
		}
		return min(account.Concurrency, hardCap)
	}
	if account == nil || !p.dynamicMaxConnsEnabled() {
		return hardCap
	}
	if account.Concurrency <= 0 {
		// 0/-1 等“无限制”并发场景下，仍由全局硬上限兜底。
		return hardCap
	}
	factor := p.maxConnsFactorByAccount(account)
	if factor <= 0 {
		factor = 1.0
	}
	effective := int(math.Ceil(float64(account.Concurrency) * factor))
	if effective < 1 {
		effective = 1
	}
	if effective > hardCap {
		effective = hardCap
	}
	return effective
}

func (p *openAIWSConnPool) minIdlePerAccount() int {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.MinIdlePerAccount >= 0 {
		return p.cfg.Gateway.OpenAIWS.MinIdlePerAccount
	}
	return 0
}

func (p *openAIWSConnPool) maxIdlePerAccount() int {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.MaxIdlePerAccount >= 0 {
		return p.cfg.Gateway.OpenAIWS.MaxIdlePerAccount
	}
	return 4
}

func (p *openAIWSConnPool) maxConnAge() time.Duration {
	return openAIWSConnMaxAge
}

func (p *openAIWSConnPool) queueLimitPerConn() int {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.QueueLimitPerConn > 0 {
		return p.cfg.Gateway.OpenAIWS.QueueLimitPerConn
	}
	return 256
}

func (p *openAIWSConnPool) targetUtilization() float64 {
	if p != nil && p.cfg != nil {
		ratio := p.cfg.Gateway.OpenAIWS.PoolTargetUtilization
		if ratio > 0 && ratio <= 1 {
			return ratio
		}
	}
	return 0.7
}

func (p *openAIWSConnPool) prewarmCooldown() time.Duration {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.PrewarmCooldownMS > 0 {
		return time.Duration(p.cfg.Gateway.OpenAIWS.PrewarmCooldownMS) * time.Millisecond
	}
	return 0
}

func (p *openAIWSConnPool) shouldSuppressPrewarmLocked(ap *openAIWSAccountPool, now time.Time) bool {
	if ap == nil {
		return true
	}
	if ap.prewarmFails <= 0 {
		return false
	}
	if ap.prewarmFailAt.IsZero() {
		ap.prewarmFails = 0
		return false
	}
	if now.Sub(ap.prewarmFailAt) > openAIWSPrewarmFailureWindow {
		ap.prewarmFails = 0
		ap.prewarmFailAt = time.Time{}
		return false
	}
	return ap.prewarmFails >= openAIWSPrewarmFailureSuppress
}

func (p *openAIWSConnPool) dialTimeout() time.Duration {
	if p != nil && p.cfg != nil && p.cfg.Gateway.OpenAIWS.DialTimeoutSeconds > 0 {
		return time.Duration(p.cfg.Gateway.OpenAIWS.DialTimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}

func cloneOpenAIWSAcquireRequest(req openAIWSAcquireRequest) openAIWSAcquireRequest {
	copied := req
	copied.Headers = cloneHeader(req.Headers)
	copied.WSURL = normalizeOpenAIWSURL(req.WSURL)
	copied.ProxyURL = normalizeOpenAIWSProxyURL(req.ProxyURL)
	copied.PreferredConnID = stringsTrim(req.PreferredConnID)
	return copied
}

func cloneOpenAIWSAcquireRequestPtr(req *openAIWSAcquireRequest) *openAIWSAcquireRequest {
	if req == nil {
		return nil
	}
	copied := cloneOpenAIWSAcquireRequest(*req)
	return &copied
}

func sameOpenAIWSPrewarmTarget(a, b openAIWSAcquireRequest) bool {
	return normalizeOpenAIWSURL(a.WSURL) == normalizeOpenAIWSURL(b.WSURL) &&
		normalizeOpenAIWSProxyURL(a.ProxyURL) == normalizeOpenAIWSProxyURL(b.ProxyURL) &&
		normalizeOpenAIWSHandshakeCompatibilityForRequest(a) == normalizeOpenAIWSHandshakeCompatibilityForRequest(b)
}

func normalizeOpenAIWSBetaFeatures(headers http.Header) string {
	features := make(map[string]struct{})
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), "x-codex-beta-features") {
			continue
		}
		for _, value := range values {
			for _, feature := range strings.Split(value, ",") {
				if feature = strings.TrimSpace(feature); feature != "" {
					features[feature] = struct{}{}
				}
			}
		}
	}
	if len(features) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(features))
	for feature := range features {
		normalized = append(normalized, feature)
	}
	sort.Strings(normalized)
	return strings.Join(normalized, ",")
}

// normalizeOpenAIWSHandshakeCompatibility retains the historic helper shape
// for callers that only need beta feature normalization. Actual pool acquires
// use the account-aware variant below so opt-in fingerprint modes participate
// in compatibility without changing the default/off behavior.
func normalizeOpenAIWSHandshakeCompatibility(headers http.Header) openAIWSHandshakeCompatibilityKey {
	return normalizeOpenAIWSHandshakeCompatibilityForAccount(nil, headers)
}

func normalizeOpenAIWSHandshakeCompatibilityForAccount(account *Account, headers http.Header) openAIWSHandshakeCompatibilityKey {
	key := openAIWSHandshakeCompatibilityKey{
		betaFeatures: normalizeOpenAIWSBetaFeatures(headers),
	}
	mode := activeCodexFingerprintMode(account)
	if mode == codexFingerprintOff {
		return key
	}
	key.codexInstallationID = normalizeOpenAIWSStableIdentityHeader(headers, "x-codex-installation-id")
	if mode == codexFingerprintDevice {
		return key
	}
	key.sessionIDHyphen = normalizeOpenAIWSStableIdentityHeader(headers, "session-id")
	key.sessionIDUnderscore = normalizeOpenAIWSStableIdentityHeader(headers, "session_id")
	key.threadID = normalizeOpenAIWSStableIdentityHeader(headers, "thread-id")
	key.clientRequestID = normalizeOpenAIWSStableIdentityHeader(headers, "x-client-request-id")
	key.codexWindowID = normalizeOpenAIWSStableIdentityHeader(headers, "x-codex-window-id")
	return key
}

func activeCodexFingerprintMode(account *Account) codexFingerprintMode {
	if account == nil || account.GetCodexFingerprintMode() == codexFingerprintOff {
		return codexFingerprintOff
	}
	if _, ok := codexFingerprintSeed(account.Extra); !ok {
		return codexFingerprintOff
	}
	return account.GetCodexFingerprintMode()
}

func normalizeOpenAIWSStableIdentityHeader(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get(name))
}

func normalizeOpenAIWSHandshakeCompatibilityForRequest(req openAIWSAcquireRequest) openAIWSHandshakeCompatibilityKey {
	compatibility := normalizeOpenAIWSHandshakeCompatibilityForAccount(req.Account, req.Headers)
	compatibility.identity = strings.TrimSpace(req.IdentityCompatibility)
	return compatibility
}

// normalizeOpenAIWSProxyURL gives pooled connections the same proxy identity
// as the dialer. In particular, socks5 is canonicalized to socks5h by the
// shared parser, while malformed values remain distinct instead of matching a
// valid connection accidentally.
func normalizeOpenAIWSProxyURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if normalized, _, err := proxyurl.Parse(trimmed); err == nil {
		return normalized
	}
	return trimmed
}

func normalizeOpenAIWSURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String()
}

func normalizeOpenAIWSRoutingAffinity(headers http.Header) string {
	canonicalName := http.CanonicalHeaderKey(openAICodexRoutingHintHeader)
	if values, ok := headers[canonicalName]; ok {
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}

	variantNames := make([]string, 0)
	for name := range headers {
		if name != canonicalName && strings.EqualFold(strings.TrimSpace(name), openAICodexRoutingHintHeader) {
			variantNames = append(variantNames, name)
		}
	}
	sort.Strings(variantNames)
	for _, name := range variantNames {
		for _, value := range headers[name] {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for k, vals := range src {
		if len(vals) == 0 {
			dst[k] = nil
			continue
		}
		copied := make([]string, len(vals))
		copy(copied, vals)
		dst[k] = copied
	}
	return dst
}

func closeOpenAIWSConns(conns []*openAIWSConn) {
	if len(conns) == 0 {
		return
	}
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		conn.close()
	}
}

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}
