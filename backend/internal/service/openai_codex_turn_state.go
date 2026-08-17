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
)

const openAICodexTurnStateHeader = "x-codex-turn-state"

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
	apiKeyID := getAPIKeyIDFromContext(c)
	if sessionID == "" || apiKeyID <= 0 {
		return ""
	}
	return strconv.FormatInt(apiKeyID, 10) + "\x00" + sessionID
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
	if (account.Platform != "" || account.Type != "") && !account.IsOpenAIOAuth() {
		return
	}
	state := extractOpenAICodexTurnState(h)
	if state == "" {
		return
	}
	bindingKey := openAICodexTurnStateBindingKey(c, state)
	if bindingKey == "" {
		return
	}
	origin, known := s.loadOpenAICodexTurnStateOrigin(c, bindingKey)
	if !known && account.Platform == "" && account.Type == "" {
		if legacy := openAICodexTurnStateSeed(c); legacy != "" {
			origin, known = s.loadOpenAICodexTurnStateOrigin(c, legacy)
		}
	}
	if known && origin.accountID != account.ID {
		h.Del(openAICodexTurnStateHeader)
	}
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
