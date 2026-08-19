package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAICodexTurnStateHeader = "x-codex-turn-state"

// openAICodexTurnStateSessionContextKey caches the canonical client session
// only for the lifetime of one gateway request. A few reverse proxies retain
// the official Codex client_metadata envelope while removing session-id from
// the HTTP/WS headers. Turn-state provenance must use that same session in
// both directions, otherwise a failed-over state can be replayed to the next
// OAuth account.
const openAICodexTurnStateSessionContextKey = "openai_codex_turn_state_session_id"

const (
	openAICodexTurnStateBindingPrefix = "openai:codex:turn-state:v2:"
	openAICodexTurnStateCacheTimeout  = 500 * time.Millisecond
)

// Each exact opaque state is bound to the account that minted it. Only a
// scoped digest is persisted in the shared cache; the state itself never is.
type openAICodexTurnStateOrigin struct {
	accountID int64
	expiresAt time.Time
}

func openAICodexTurnStateSeed(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	sessionID := extractClientSessionID(c.Request.Header)
	if sessionID == "" {
		if value, ok := c.Get(openAICodexTurnStateSessionContextKey); ok {
			sessionID, _ = value.(string)
			sessionID = strings.TrimSpace(sessionID)
		}
	}
	apiKeyID := getAPIKeyIDFromContext(c)
	if sessionID == "" || apiKeyID <= 0 {
		return ""
	}
	return strconv.FormatInt(apiKeyID, 10) + "\x00" + sessionID
}

// stageOpenAICodexTurnStateSession records the body-carried official Codex
// session before a response can mint or validate x-codex-turn-state. Header
// values remain authoritative when present. A frame without client_metadata
// keeps a previously staged value, because an official WS client can send its
// identity on the first response.create frame and omit it on continuation
// frames. An explicit malformed or session-less metadata object clears it.
func stageOpenAICodexTurnStateSession(c *gin.Context, body []byte) {
	if c == nil {
		return
	}

	if c.Request != nil {
		if sessionID := extractClientSessionID(c.Request.Header); sessionID != "" {
			c.Set(openAICodexTurnStateSessionContextKey, sessionID)
			return
		}
	}

	if !gjson.ValidBytes(body) {
		return
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.Exists() {
		return
	}
	projection := codexIdentityFromBody(body)
	if !projection.valid {
		c.Set(openAICodexTurnStateSessionContextKey, "")
		return
	}
	c.Set(openAICodexTurnStateSessionContextKey, strings.TrimSpace(projection.tuple.sessionID))
}

func openAICodexTurnStateBindingKey(c *gin.Context, state string) string {
	seed := openAICodexTurnStateSeed(c)
	state = strings.TrimSpace(state)
	if seed == "" || state == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(seed + "\x00" + state))
	return openAICodexTurnStateBindingPrefix + hex.EncodeToString(digest[:])
}

func (s *OpenAIGatewayService) relayOpenAICodexTurnState(c *gin.Context, account *Account, upstream http.Header) {
	if c == nil || c.Writer == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		c.Writer.Header().Del(canonical)
		return
	}
	c.Writer.Header().Set(canonical, state)
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

func stageOpenAICodexTurnState(dst *http.Header, upstream http.Header) {
	if dst == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		if *dst != nil {
			(*dst).Del(canonical)
		}
		return
	}
	if *dst == nil {
		*dst = http.Header{}
	}
	(*dst).Set(canonical, state)
}

func (s *OpenAIGatewayService) noteStagedOpenAICodexTurnStateCommitted(c *gin.Context, account *Account, staged http.Header) {
	if staged == nil {
		return
	}
	state := extractOpenAICodexTurnState(staged)
	if state == "" {
		return
	}
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

func extractOpenAICodexTurnState(upstream http.Header) string {
	if upstream == nil {
		return ""
	}
	return strings.TrimSpace(upstream.Get(openAICodexTurnStateHeader))
}

// The optional state argument keeps source compatibility for old internal
// callers; new call sites always pass the exact value being committed.
func (s *OpenAIGatewayService) noteOpenAICodexTurnStateProvenance(c *gin.Context, account *Account, states ...string) {
	if s == nil || account == nil || account.ID <= 0 || len(states) == 0 {
		return
	}
	// Zero-value accounts are used by a few internal adapters before hydration;
	// real persisted accounts always carry both fields. Treat that transient
	// shape as OAuth-compatible, while explicit API-key accounts stay transparent.
	if (account.Platform != "" || account.Type != "") && !account.IsOpenAIOAuth() {
		return
	}
	state := strings.TrimSpace(states[0])
	bindingKey := openAICodexTurnStateBindingKey(c, state)
	if bindingKey == "" {
		return
	}
	ttl := s.openAIWSSessionStickyTTL()
	s.openaiCodexTurnStateOrigins.Store(bindingKey, openAICodexTurnStateOrigin{
		accountID: account.ID,
		expiresAt: time.Now().Add(ttl),
	})
	if account.Platform == "" && account.Type == "" {
		// Compatibility for pre-hydration test/adaptor records. Persisted
		// accounts use only the exact-state digest above.
		if legacy := openAICodexTurnStateSeed(c); legacy != "" {
			s.openaiCodexTurnStateOrigins.Store(legacy, openAICodexTurnStateOrigin{accountID: account.ID, expiresAt: time.Now().Add(ttl)})
		}
	}
	s.sweepOpenAICodexTurnStateOrigins()
	if s.cache == nil {
		return
	}
	cacheCtx, cancel := context.WithTimeout(context.Background(), openAICodexTurnStateCacheTimeout)
	defer cancel()
	_ = s.cache.SetSessionAccountID(cacheCtx, getOpenAIGroupIDFromContext(c), bindingKey, account.ID, ttl)
}

// guardOpenAICodexTurnStateEcho strips a known state only when it was minted
// by a different OAuth account. Unknown states remain transparent so a cache
// outage does not destroy a valid client conversation.
func (s *OpenAIGatewayService) guardOpenAICodexTurnStateEcho(c *gin.Context, account *Account, h http.Header) {
	if s == nil || h == nil || account == nil {
		return
	}
	state := extractOpenAICodexTurnState(h)
	if s.isForeignOpenAICodexTurnState(c, account, state) {
		h.Del(openAICodexTurnStateHeader)
	}
}

// scrubForeignOpenAICodexTurnStateFromBody applies the same provenance check
// to the canonical Responses/WS client_metadata carrier. The official CLI
// sends x-codex-turn-state in this body map for websocket turns, so guarding
// only the direct compatibility header would allow a state from a failed-over
// account to reach the next upstream request. Unknown states deliberately
// remain intact: cache availability must not destroy a valid client turn.
func (s *OpenAIGatewayService) scrubForeignOpenAICodexTurnStateFromBody(c *gin.Context, account *Account, body []byte) ([]byte, bool) {
	stageOpenAICodexTurnStateSession(c, body)
	if s == nil || account == nil || !gjson.ValidBytes(body) {
		return body, false
	}
	const turnStatePath = "client_metadata.x-codex-turn-state"
	state := strings.TrimSpace(gjson.GetBytes(body, turnStatePath).String())
	if !s.isForeignOpenAICodexTurnState(c, account, state) {
		return body, false
	}
	scrubbed, err := sjson.DeleteBytes(body, turnStatePath)
	if err != nil {
		return body, false
	}
	return scrubbed, true
}

func (s *OpenAIGatewayService) isForeignOpenAICodexTurnState(c *gin.Context, account *Account, state string) bool {
	if s == nil || account == nil {
		return false
	}
	if (account.Platform != "" || account.Type != "") && !account.IsOpenAIOAuth() {
		return false
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	bindingKey := openAICodexTurnStateBindingKey(c, state)
	if bindingKey == "" {
		return false
	}
	origin, known := s.loadOpenAICodexTurnStateOrigin(c, bindingKey)
	if !known && account.Platform == "" && account.Type == "" {
		if legacy := openAICodexTurnStateSeed(c); legacy != "" {
			origin, known = s.loadOpenAICodexTurnStateOrigin(c, legacy)
		}
	}
	return known && origin.accountID != account.ID
}

func (s *OpenAIGatewayService) loadOpenAICodexTurnStateOrigin(c *gin.Context, bindingKey string) (openAICodexTurnStateOrigin, bool) {
	if raw, ok := s.openaiCodexTurnStateOrigins.Load(bindingKey); ok {
		origin, valid := raw.(openAICodexTurnStateOrigin)
		if !valid {
			s.openaiCodexTurnStateOrigins.Delete(bindingKey)
		} else if origin.expiresAt.IsZero() || time.Now().Before(origin.expiresAt) {
			return origin, true
		} else {
			s.openaiCodexTurnStateOrigins.Delete(bindingKey)
		}
	}
	if s.cache == nil {
		return openAICodexTurnStateOrigin{}, false
	}
	cacheCtx, cancel := context.WithTimeout(context.Background(), openAICodexTurnStateCacheTimeout)
	defer cancel()
	accountID, err := s.cache.GetSessionAccountID(cacheCtx, getOpenAIGroupIDFromContext(c), bindingKey)
	if err != nil || accountID <= 0 {
		return openAICodexTurnStateOrigin{}, false
	}
	origin := openAICodexTurnStateOrigin{accountID: accountID, expiresAt: time.Now().Add(s.openAIWSSessionStickyTTL())}
	s.openaiCodexTurnStateOrigins.Store(bindingKey, origin)
	return origin, true
}

func (s *OpenAIGatewayService) sweepOpenAICodexTurnStateOrigins() {
	if s.openaiCodexTurnStateWrites.Add(1)%256 != 0 {
		return
	}
	now := time.Now()
	s.openaiCodexTurnStateOrigins.Range(func(key, value any) bool {
		origin, ok := value.(openAICodexTurnStateOrigin)
		if !ok || (!origin.expiresAt.IsZero() && now.After(origin.expiresAt)) {
			s.openaiCodexTurnStateOrigins.Delete(key)
		}
		return true
	})
}
