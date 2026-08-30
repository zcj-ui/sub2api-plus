package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/imroc/req/v3"
)

// ErrSparkShadowResetNotSupported is returned when ResetCredit is called on a
// spark shadow account. Shadow accounts do not hold credentials of their own;
// the caller must reset the parent account directly. It is a structured
// infraerrors value so the handler maps it to 409 Conflict (not a bare 500);
// errors.Is still matches it by identity since ResetCredit returns this var.
var ErrSparkShadowResetNotSupported = infraerrors.New(http.StatusConflict, "SPARK_SHADOW_RESET_NOT_SUPPORTED", "spark shadow account does not support credit reset; reset the parent account")

// Endpoints used by the OpenAI/ChatGPT/Codex quota query and reset feature.
const (
	chatGPTUsageURL             = "https://chatgpt.com/backend-api/wham/usage"
	chatGPTRateLimitCreditsURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	chatGPTRateLimitResetURL    = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	openaiQuotaUpstreamTimeout  = 20 * time.Second
	openaiQuotaCodexBeta        = "codex-1"
	openaiQuotaCodexOriginator  = "Codex Desktop"
	openaiQuotaCodexLanguageTag = "zh-CN"
	openaiQuotaSecFetchSite     = "none"
	openaiQuotaSecFetchMode     = "no-cors"
	openaiQuotaSecFetchDest     = "empty"
	openaiQuotaResetCreditsKey  = OpenAIQuotaResetCreditsExtraKey
	openaiQuotaCreditBalanceKey = OpenAIQuotaCreditBalanceExtraKey
)

// OpenAIRateLimitWindow describes a single rate-limit window returned by
// /wham/usage. The upstream returns an explicit `null` window when the slot
// is unused, so consumers should treat a nil pointer as "no data".
type OpenAIRateLimitWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// OpenAIRateLimit is a rate-limit envelope (primary + optional secondary window).
type OpenAIRateLimit struct {
	Allowed         bool                   `json:"allowed"`
	LimitReached    bool                   `json:"limit_reached"`
	PrimaryWindow   *OpenAIRateLimitWindow `json:"primary_window,omitempty"`
	SecondaryWindow *OpenAIRateLimitWindow `json:"secondary_window,omitempty"`
}

// OpenAIAdditionalRateLimit describes a per-feature rate limit (e.g. Codex Spark).
type OpenAIAdditionalRateLimit struct {
	LimitName      string           `json:"limit_name"`
	MeteredFeature string           `json:"metered_feature"`
	RateLimit      *OpenAIRateLimit `json:"rate_limit,omitempty"`
}

// OpenAIRateLimitResetCreditDetail is the sanitized metadata surfaced for one
// available reset credit. Do not add upstream ids or tokens here.
type OpenAIRateLimitResetCreditDetail struct {
	ExpiresAt string `json:"expires_at,omitempty"`
}

// OpenAIRateLimitResetCredits captures the "available_count" surfaced for the
// rate_limit_reset_credit grant type, which the reset action consumes.
type OpenAIRateLimitResetCredits struct {
	AvailableCount           int                                `json:"available_count"`
	ApplicableAvailableCount *int                               `json:"applicable_available_count,omitempty"`
	Credits                  []OpenAIRateLimitResetCreditDetail `json:"credits,omitempty"`
}

// OpenAICodexCredits is the paid-credit balance returned by /wham/usage.
// Balance remains a string so the upstream decimal is not rounded in transit.
type OpenAICodexCredits struct {
	HasCredits          bool   `json:"has_credits"`
	Unlimited           bool   `json:"unlimited"`
	OverageLimitReached bool   `json:"overage_limit_reached"`
	Balance             string `json:"balance"`
}

// OpenAISpendControl describes a workspace spend-control envelope returned by
// newer ChatGPT/Codex plans (team, business, enterprise, edu and k12).  It is
// deliberately kept separate from the normal 5h/7d rate-limit windows: a
// workspace can expose a spend-control limit even when rate_limit is null.
//
// Limit/Used/Remaining are strings because the upstream represents monetary
// values as decimal strings on some plans and JSON numbers on others.  The
// custom decoder below accepts both forms without a float round-trip.
type OpenAISpendControl struct {
	Reached         bool                     `json:"reached"`
	IndividualLimit *OpenAISpendControlLimit `json:"individual_limit,omitempty"`
}

type OpenAISpendControlLimit struct {
	Source            string  `json:"source,omitempty"`
	Limit             string  `json:"limit,omitempty"`
	Used              string  `json:"used,omitempty"`
	Remaining         string  `json:"remaining,omitempty"`
	UsedPercent       float64 `json:"used_percent,omitempty"`
	RemainingPercent  float64 `json:"remaining_percent,omitempty"`
	ResetAfterSeconds int64   `json:"reset_after_seconds,omitempty"`
	ResetAt           int64   `json:"reset_at,omitempty"`
}

// UnmarshalJSON accepts the number-or-string forms emitted by WHAM for spend
// control values.  Optional malformed fields are ignored so a newly added
// upstream field cannot make an otherwise valid quota response unusable.
func (l *OpenAISpendControlLimit) UnmarshalJSON(data []byte) error {
	if l == nil {
		return fmt.Errorf("unmarshal OpenAI spend-control limit into nil receiver")
	}
	var raw struct {
		Source            string          `json:"source"`
		Limit             json.RawMessage `json:"limit"`
		Used              json.RawMessage `json:"used"`
		Remaining         json.RawMessage `json:"remaining"`
		UsedPercent       json.RawMessage `json:"used_percent"`
		RemainingPercent  json.RawMessage `json:"remaining_percent"`
		ResetAfterSeconds json.RawMessage `json:"reset_after_seconds"`
		ResetAt           json.RawMessage `json:"reset_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	l.Source = strings.TrimSpace(raw.Source)
	l.Limit = openAIQuotaJSONScalarString(raw.Limit)
	l.Used = openAIQuotaJSONScalarString(raw.Used)
	l.Remaining = openAIQuotaJSONScalarString(raw.Remaining)
	l.UsedPercent = openAIQuotaJSONFloat(raw.UsedPercent)
	l.RemainingPercent = openAIQuotaJSONFloat(raw.RemainingPercent)
	l.ResetAfterSeconds = openAIQuotaJSONInt64(raw.ResetAfterSeconds)
	l.ResetAt = openAIQuotaJSONInt64(raw.ResetAt)
	return nil
}

// UnmarshalJSON makes `reached` tolerant of the string/number variants seen
// in compatibility proxies while retaining the ordinary bool wire shape.
func (s *OpenAISpendControl) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("unmarshal OpenAI spend-control into nil receiver")
	}
	var raw struct {
		Reached         json.RawMessage `json:"reached"`
		IndividualLimit json.RawMessage `json:"individual_limit"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Reached = openAIQuotaJSONBool(raw.Reached)
	s.IndividualLimit = nil
	if len(bytes.TrimSpace(raw.IndividualLimit)) > 0 && !bytes.Equal(bytes.TrimSpace(raw.IndividualLimit), []byte("null")) {
		var limit OpenAISpendControlLimit
		// A newly introduced/relay-specific shape must not invalidate the whole
		// quota response; keep the envelope and simply omit an unparseable limit.
		if err := json.Unmarshal(raw.IndividualLimit, &limit); err == nil {
			s.IndividualLimit = &limit
		}
	}
	return nil
}

func openAIQuotaJSONScalarString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) == nil {
			return strings.TrimSpace(value)
		}
		return ""
	}
	var number json.Number
	if json.Unmarshal(trimmed, &number) == nil {
		return number.String()
	}
	return ""
}

func openAIQuotaJSONFloat(raw json.RawMessage) float64 {
	value := openAIQuotaJSONScalarString(raw)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0
	}
	return parsed
}

func openAIQuotaJSONInt64(raw json.RawMessage) int64 {
	value := openAIQuotaJSONScalarString(raw)
	if value == "" {
		return 0
	}
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		return parsed
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed > math.MaxInt64 || parsed < math.MinInt64 {
		return 0
	}
	return int64(parsed)
}

func openAIQuotaJSONBool(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var value bool
	if json.Unmarshal(trimmed, &value) == nil {
		return value
	}
	text := strings.ToLower(strings.TrimSpace(openAIQuotaJSONScalarString(trimmed)))
	return text == "true" || text == "1"
}

// UnmarshalJSON accepts both forms currently returned by /wham/usage for
// credits.balance. Keeping Balance as a string preserves decimal precision for
// the UI's Credit / 25 reference conversion and avoids a float round trip.
func (c *OpenAICodexCredits) UnmarshalJSON(data []byte) error {
	if c == nil {
		return fmt.Errorf("unmarshal OpenAI Codex credits into nil receiver")
	}

	type creditsPayload struct {
		HasCredits          bool            `json:"has_credits"`
		Unlimited           bool            `json:"unlimited"`
		OverageLimitReached bool            `json:"overage_limit_reached"`
		Balance             json.RawMessage `json:"balance"`
	}
	var payload creditsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	c.HasCredits = payload.HasCredits
	c.Unlimited = payload.Unlimited
	c.OverageLimitReached = payload.OverageLimitReached
	c.Balance = ""
	rawBalance := bytes.TrimSpace(payload.Balance)
	if len(rawBalance) == 0 || bytes.Equal(rawBalance, []byte("null")) {
		return nil
	}
	if rawBalance[0] == '"' {
		return json.Unmarshal(rawBalance, &c.Balance)
	}

	var number json.Number
	if err := json.Unmarshal(rawBalance, &number); err != nil {
		return fmt.Errorf("decode credits.balance: %w", err)
	}
	c.Balance = number.String()
	return nil
}

// OpenAICodexCreditSnapshot is persisted in account.extra after a successful
// quota refresh. UpdatedAt distinguishes a real zero balance from no probe yet.
type OpenAICodexCreditSnapshot struct {
	OpenAICodexCredits
	UpdatedAt string `json:"updated_at"`
}

// OpenAIQuotaUsage is the typed projection of /wham/usage we expose to the UI.
// Fields not relevant to the quota card are intentionally omitted to keep the
// surface narrow; full upstream payload preservation is unnecessary.
type OpenAIQuotaUsage struct {
	UserID                string                       `json:"user_id,omitempty"`
	AccountID             string                       `json:"account_id,omitempty"`
	Email                 string                       `json:"email,omitempty"`
	PlanType              string                       `json:"plan_type,omitempty"`
	RateLimit             *OpenAIRateLimit             `json:"rate_limit,omitempty"`
	AdditionalRateLimits  []OpenAIAdditionalRateLimit  `json:"additional_rate_limits,omitempty"`
	Credits               *OpenAICodexCredits          `json:"credits,omitempty"`
	SpendControl          *OpenAISpendControl          `json:"spend_control,omitempty"`
	RateLimitResetCredits *OpenAIRateLimitResetCredits `json:"rate_limit_reset_credits,omitempty"`
	FetchedAt             int64                        `json:"fetched_at"`
	autoResetCandidates   []openAIAutoResetCreditCandidate
}

// OpenAIQuotaResetCredit captures the redeemed credit metadata returned by the
// reset endpoint.
type OpenAIQuotaResetCredit struct {
	ID              string `json:"id,omitempty"`
	ResetType       string `json:"reset_type,omitempty"`
	Status          string `json:"status,omitempty"`
	GrantedAt       string `json:"granted_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	RedeemStartedAt string `json:"redeem_started_at,omitempty"`
	RedeemedAt      string `json:"redeemed_at,omitempty"`
}

// OpenAIQuotaResetResult is the typed projection of /wham/rate-limit-reset-credits/consume.
// The inner Credit also carries `redeemed_at` (RFC3339 string); we deliberately do
// NOT add a top-level redeemed_at to avoid ambiguity with the nested field.
type OpenAIQuotaResetResult struct {
	Code         string                  `json:"code"`
	Credit       *OpenAIQuotaResetCredit `json:"credit,omitempty"`
	WindowsReset int                     `json:"windows_reset"`
}

// OpenAIQuotaService queries and consumes ChatGPT/Codex rate-limit reset credits
// for OpenAI OAuth accounts. It reuses the privacy client factory so all calls
// flow through the impersonated HTTP client (Cloudflare-friendly TLS fingerprint).
type OpenAIQuotaService struct {
	accountRepo          AccountRepository
	proxyRepo            ProxyRepository
	tokenProvider        *OpenAITokenProvider
	privacyClientFactory PrivacyClientFactory
	agentIdentityTaskMu  sync.Mutex
	agentIdentityWS      agentIdentityWSConnectionInvalidator
}

// NewOpenAIQuotaService constructs a quota service. token provider is required —
// it ensures we always invoke upstream with a valid (refreshed-if-needed)
// access_token, sharing the same refresh/locking machinery used by the gateway.
func NewOpenAIQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *OpenAITokenProvider,
	privacyClientFactory PrivacyClientFactory,
) *OpenAIQuotaService {
	return &OpenAIQuotaService{
		accountRepo:          accountRepo,
		proxyRepo:            proxyRepo,
		tokenProvider:        tokenProvider,
		privacyClientFactory: privacyClientFactory,
	}
}

// QueryUsage fetches the latest rate-limit/usage snapshot for the given OpenAI
// OAuth account. Returns infraerrors so the handler layer can map them to
// stable error codes / HTTP statuses.
func (s *OpenAIQuotaService) QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error) {
	return s.queryUsage(ctx, accountID, false)
}

func (s *OpenAIQuotaService) queryUsage(ctx context.Context, accountID int64, requireResetCredits bool) (*OpenAIQuotaUsage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return nil, ErrOpenAIQuotaInvalidAccountID
	}
	accessToken, chatGPTAccountID, proxyURL, fedRAMP, err := s.prepareUpstreamCall(ctx, accountID)
	if err != nil {
		return nil, err
	}

	client, err := s.privacyClientFactory(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_CLIENT_ERROR", "failed to build upstream client: %v", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, openaiQuotaUpstreamTimeout)
	defer cancel()
	agentIdentity := s.isAgentIdentityAccount(ctx, accountID)

	var payload OpenAIQuotaUsage
	for recovered := false; ; {
		quotaHeaders, expectedTaskID, headerErr := s.buildCodexQuotaHeaders(callCtx, accountID, accessToken, chatGPTAccountID, fedRAMP)
		if headerErr != nil {
			return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_AUTH_FAILED", "failed to build upstream authentication: %v", headerErr)
		}
		resp, responseBody, err := requestOpenAIQuotaJSON(
			callCtx,
			client,
			http.MethodGet,
			chatGPTUsageURL,
			quotaHeaders,
			nil,
			&payload,
		)
		if err != nil {
			if isOpenAIQuotaResponseTooLarge(err) {
				return nil, openAIQuotaResponseTooLargeError(err)
			}
			return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_REQUEST_FAILED", "upstream request failed: %v", err)
		}
		if !resp.IsSuccessState() {
			if agentIdentity && !recovered && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, responseBody) {
				recovered = true
				if err := s.recoverAgentIdentityTask(ctx, accountID, expectedTaskID); err != nil {
					return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_AUTH_FAILED", "agent identity task recovery failed: %v", err)
				}
				continue
			}
			status := resp.StatusCode
			if isOpenAIAutoResetContext(ctx) {
				slog.Warn("openai_quota_query_failed", "account_id", accountID, "status", status, "source", "auto_reset")
				return nil, infraerrors.Newf(mapUpstreamStatus(status), "OPENAI_QUOTA_UPSTREAM_ERROR", "upstream returned %d", status)
			}
			body := truncate(s.redactQuotaErrorBody(ctx, accountID, string(responseBody)), 240)
			slog.Warn("openai_quota_query_failed", "account_id", accountID, "status", status, "body", body)
			return nil, infraerrors.Newf(mapUpstreamStatus(status), "OPENAI_QUOTA_UPSTREAM_ERROR", "upstream returned %d: %s", status, body)
		}
		break
	}
	if !hasStructurallyValidOpenAIQuotaPayload(&payload) {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_QUOTA_INVALID_RESPONSE", "upstream usage response does not contain a valid rate-limit window or quota payload")
	}

	payload.FetchedAt = time.Now().Unix()
	details, detailsErr := s.queryResetCreditDetails(callCtx, client, accessToken, chatGPTAccountID, fedRAMP, accountID)
	if detailsErr != nil {
		// A bounded-body violation is a deterministic protocol/relay failure,
		// unlike a transient 401/5xx on the optional details endpoint. Surface it
		// even for the regular best-effort query so callers do not cache or display
		// an unbounded/ambiguous response as healthy data.
		if isOpenAIQuotaResponseTooLarge(detailsErr) || isOpenAIQuotaResetCreditEntriesTooMany(detailsErr) {
			return nil, detailsErr
		}
		if requireResetCredits {
			return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_RESET_CREDITS_QUERY_FAILED", "reset-credit query failed: %v", detailsErr)
		}
		slog.Warn("openai_quota_reset_credit_details_unavailable", "account_id", accountID, "error", detailsErr)
	} else if details != nil {
		payload.autoResetCandidates = details.AutoResetCandidates
		hasDetailCount := details.AvailableCount != nil
		if payload.RateLimitResetCredits == nil {
			payload.RateLimitResetCredits = &OpenAIRateLimitResetCredits{}
		}
		if details.CreditListPresent {
			payload.RateLimitResetCredits.Credits = details.Credits
		}
		if details.ApplicableAvailableCount != nil {
			payload.RateLimitResetCredits.ApplicableAvailableCount = details.ApplicableAvailableCount
		}
		switch {
		case hasDetailCount:
			payload.RateLimitResetCredits.AvailableCount = *details.AvailableCount
		case details.CreditListPresent:
			payload.RateLimitResetCredits.AvailableCount = details.AvailableCreditCount
		}
	}
	return &payload, nil
}

// hasStructurallyValidOpenAIQuotaPayload accepts the sparse payloads used by
// workspace plans.  WHAM is also the authentication probe, so a successful
// response with only credits, spend_control, or an explicit empty rate-limit
// envelope is still a valid account snapshot.  Completely empty/malformed
// JSON remains rejected to avoid caching an HTML/proxy success page.
func hasStructurallyValidOpenAIQuotaPayload(usage *OpenAIQuotaUsage) bool {
	if usage == nil {
		return false
	}
	if hasStructurallyValidOpenAIQuotaWindow(usage) {
		return true
	}
	if usage.Credits != nil && (usage.Credits.HasCredits || usage.Credits.Unlimited || usage.Credits.OverageLimitReached || strings.TrimSpace(usage.Credits.Balance) != "") {
		return true
	}
	if usage.SpendControl != nil {
		// `reached:false` with no individual limit is a legitimate enterprise
		// response; the presence of the envelope itself is the useful signal.
		return true
	}
	if usage.RateLimitResetCredits != nil {
		return true
	}
	return false
}

func hasStructurallyValidOpenAIQuotaWindow(usage *OpenAIQuotaUsage) bool {
	if usage == nil {
		return false
	}
	if hasStructurallyValidOpenAIRateLimit(usage.RateLimit) {
		return true
	}
	for i := range usage.AdditionalRateLimits {
		if hasStructurallyValidOpenAIRateLimit(usage.AdditionalRateLimits[i].RateLimit) {
			return true
		}
	}
	return false
}

func hasStructurallyValidOpenAIRateLimit(rateLimit *OpenAIRateLimit) bool {
	if rateLimit == nil {
		return false
	}
	return hasStructurallyValidOpenAIQuotaWindowValue(rateLimit.PrimaryWindow) ||
		hasStructurallyValidOpenAIQuotaWindowValue(rateLimit.SecondaryWindow)
}

func hasStructurallyValidOpenAIQuotaWindowValue(window *OpenAIRateLimitWindow) bool {
	return window != nil && (window.LimitWindowSeconds > 0 || window.ResetAfterSeconds > 0 || window.ResetAt > 0)
}

// CacheResetCreditsSnapshot persists a complete reset-credit snapshot after an
// explicit UI refresh. The snapshot is written to the account that was queried
// (for a spark shadow that is the shadow row, even though the credits belong to
// its parent) because it is a per-row display cache: each row caches exactly
// what its own card renders, and shadows cannot consume credits anyway.
//
// Missing expiration details leave the old cache intact:
// a snapshot claiming N>0 available credits without their expiration timestamps
// cannot be aged out by readers, so it would keep showing (and offering to
// consume) credits that already expired. Callers must treat this rejection as a
// partial success — the upstream read itself is still valid.
func (s *OpenAIQuotaService) CacheResetCreditsSnapshot(ctx context.Context, accountID int64, credits *OpenAIRateLimitResetCredits) error {
	return s.cacheResetCreditsSnapshot(ctx, accountID, credits, nil)
}

// CachePostResetSnapshot persists the credits and usage windows observed after a reset.
func (s *OpenAIQuotaService) CachePostResetSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error {
	if usage == nil {
		return s.cacheResetCreditsSnapshot(ctx, accountID, nil, nil)
	}
	return s.cacheResetCreditsSnapshot(
		ctx,
		accountID,
		usage.RateLimitResetCredits,
		buildOpenAIAutoResetUsageUpdates(usage, time.Now()),
	)
}

func (s *OpenAIQuotaService) cacheResetCreditsSnapshot(ctx context.Context, accountID int64, credits *OpenAIRateLimitResetCredits, updates map[string]any) error {
	if s == nil || s.accountRepo == nil {
		return infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_CACHE_WRITE_FAILED", "openai quota account repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return ErrOpenAIQuotaInvalidAccountID
	}
	if credits == nil || (credits.AvailableCount > 0 && len(credits.Credits) == 0) {
		return infraerrors.New(
			http.StatusBadGateway,
			"OPENAI_QUOTA_RESET_CREDITS_REFRESH_FAILED",
			"failed to refresh reset-credit expiration details; cached data was preserved",
		)
	}
	if updates == nil {
		updates = make(map[string]any, 2)
	}
	updates[openaiQuotaResetCreditsKey] = credits
	// Reset-credit refreshes are part of the same Codex quota observation as
	// the usage windows. Carry a monotonic capture time so a delayed response
	// cannot replace a newer cached credit list.
	if _, ok := updates[OpenAICodexUsageObservedAtUnixNanoExtraKey]; !ok {
		updates[OpenAICodexUsageObservedAtUnixNanoExtraKey] = time.Now().UnixNano()
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return infraerrors.New(
			http.StatusInternalServerError,
			"OPENAI_QUOTA_CACHE_WRITE_FAILED",
			"failed to cache reset-credit details",
		).WithCause(err)
	}
	return nil
}

// CacheUsageSnapshot persists the ordinary Codex usage windows and paid-credit
// balance returned by /wham/usage. Spark shadows retain their dedicated
// codex_bengalfox windows and never inherit the parent account's paid credits.
func (s *OpenAIQuotaService) CacheUsageSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error {
	if usage == nil {
		return infraerrors.New(http.StatusBadGateway, "OPENAI_QUOTA_REFRESH_FAILED", "openai quota usage is empty")
	}
	if s == nil || s.accountRepo == nil {
		return infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_CACHE_WRITE_FAILED", "openai quota account repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return ErrOpenAIQuotaInvalidAccountID
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return infraerrors.Newf(http.StatusNotFound, "OPENAI_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	updates := buildOpenAIQuotaExtraUpdates(account, usage, time.Now())
	if len(updates) == 0 {
		return nil
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_CACHE_WRITE_FAILED", "failed to cache quota usage").WithCause(err)
	}
	// UpdateExtra uses an observation-time CAS for Codex usage fields. A stale
	// response still affects one SQL row, so its caller cannot infer that the
	// candidate snapshot won from RowsAffected alone. Re-read the durable row
	// before mutating the in-memory account or clearing a threshold pause; this
	// prevents an older probe from overwriting a newer credit snapshot locally
	// (and from incorrectly reopening an account on another concurrent probe).
	persisted, verifyErr := s.accountRepo.GetByID(ctx, accountID)
	if verifyErr != nil || persisted == nil {
		// The quota write already succeeded. Keep the successful upstream result
		// and defer local reconciliation to the next query rather than turning a
		// transient verification read failure into a false health-probe failure.
		slog.Warn("openai_quota_cache_verify_failed", "account_id", accountID, "error", verifyErr)
		return nil
	}
	account = persisted
	if account.HasAvailableCodexCredits() &&
		account.TempUnschedulableUntil != nil && time.Now().Before(*account.TempUnschedulableUntil) &&
		IsAccountSchedulingThresholdReason(account.TempUnschedulableReason) {
		if err := s.accountRepo.ClearTempUnschedulable(ctx, accountID); err != nil {
			return infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_THRESHOLD_CLEAR_FAILED", "failed to restore scheduling after paid credits became available").WithCause(err)
		}
		account.TempUnschedulableUntil = nil
		account.TempUnschedulableReason = ""
	}
	return nil
}

func (s *OpenAIQuotaService) queryResetCreditDetails(ctx context.Context, client *req.Client, accessToken, chatGPTAccountID string, fedRAMP bool, accountID int64) (*openAIRateLimitResetCreditDetails, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return nil, ErrOpenAIQuotaInvalidAccountID
	}
	if client == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_CLIENT_ERROR", "openai quota client is unavailable")
	}
	quotaHeaders, _, headerErr := s.buildCodexQuotaHeaders(ctx, accountID, accessToken, chatGPTAccountID, fedRAMP)
	if headerErr != nil {
		slog.Warn("openai_quota_reset_credit_details_auth_failed", "account_id", accountID, "error", headerErr)
		return nil, headerErr
	}
	resp, responseBody, err := requestOpenAIQuotaJSON(
		ctx,
		client,
		http.MethodGet,
		chatGPTRateLimitCreditsURL,
		quotaHeaders,
		nil,
		nil,
	)
	if err != nil {
		if isOpenAIQuotaResponseTooLarge(err) {
			return nil, openAIQuotaResponseTooLargeError(err)
		}
		slog.Warn("openai_quota_reset_credit_details_failed", "account_id", accountID, "error", err)
		return nil, err
	}
	if !resp.IsSuccessState() {
		slog.Warn("openai_quota_reset_credit_details_failed", "account_id", accountID, "status", resp.StatusCode)
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	details, err := parseOpenAIRateLimitResetCreditDetails(responseBody)
	if err != nil {
		slog.Warn("openai_quota_reset_credit_details_parse_failed", "account_id", accountID, "error", err)
		if isOpenAIQuotaResetCreditEntriesTooMany(err) {
			return nil, infraerrors.Newf(
				http.StatusBadGateway,
				"OPENAI_QUOTA_RESET_CREDITS_TOO_MANY",
				"reset-credit response contains more than %d entries",
				openAIQuotaMaxResetCreditEntries,
			).WithCause(err)
		}
		if details.AvailableCount == nil && details.ApplicableAvailableCount == nil {
			return nil, err
		}
	}
	if details.AvailableCount == nil && details.ApplicableAvailableCount == nil && !details.CreditListPresent {
		return nil, fmt.Errorf("reset-credit response does not contain a count or credit list")
	}
	return &details, nil
}

// QueryUsageForHealth requires both quota windows and reset-credit metadata.
// The regular UI query remains best-effort for reset credits, while health
// probing must fail closed so a partially dead account enters the failure pool.
func (s *OpenAIQuotaService) QueryUsageForHealth(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error) {
	return s.queryUsage(ctx, accountID, true)
}

// ResetCredit consumes one rate_limit_reset_credit for the given OpenAI account.
// The redeem_request_id is auto-generated (uuid-like) — upstream uses it for
// idempotency. Returns the consumed credit metadata so the UI can refresh.
func (s *OpenAIQuotaService) ResetCredit(ctx context.Context, accountID int64) (*OpenAIQuotaResetResult, error) {
	redeemRequestID, err := generateRedeemRequestID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_QUOTA_REDEEM_ID_FAILED", "failed to generate redeem id: %v", err)
	}
	return s.resetCredit(ctx, accountID, "", redeemRequestID, false)
}

// ResetCreditTargeted 使用固定卡 ID 与兑换 ID执行自动消费。调用方必须在重试时
// 复用同一组参数；本方法不会回退到不带 credit_id 的旧消费方式。
func (s *OpenAIQuotaService) ResetCreditTargeted(ctx context.Context, accountID int64, creditID, redeemRequestID string) (*OpenAIQuotaResetResult, error) {
	creditID = strings.TrimSpace(creditID)
	redeemRequestID = strings.TrimSpace(redeemRequestID)
	if creditID == "" || redeemRequestID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_QUOTA_TARGETED_RESET_INVALID", "credit_id and redeem_request_id are required")
	}
	return s.resetCredit(ctx, accountID, creditID, redeemRequestID, true)
}

func (s *OpenAIQuotaService) resetCredit(ctx context.Context, accountID int64, creditID, redeemRequestID string, targeted bool) (*OpenAIQuotaResetResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return nil, ErrOpenAIQuotaInvalidAccountID
	}
	// Shadow guard: resetting credits via a shadow account would silently
	// operate on the parent's quota; that is surprising and unwanted. Callers
	// must reset the parent account directly.
	//
	// Fail-closed: if the account cannot be loaded (transient DB error), we
	// must NOT fall through to prepareUpstreamCall. That function resolves a
	// shadow to its parent and would perform a parent-level reset — exactly
	// what this guard must prevent. Return the load error instead.
	if s.accountRepo != nil {
		acc, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, infraerrors.Newf(http.StatusNotFound, "OPENAI_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", loadErr)
		}
		if acc.IsShadow() {
			return nil, ErrSparkShadowResetNotSupported
		}
	}

	accessToken, chatGPTAccountID, proxyURL, fedRAMP, err := s.prepareUpstreamCall(ctx, accountID)
	if err != nil {
		return nil, err
	}

	client, err := s.privacyClientFactory(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_CLIENT_ERROR", "failed to build upstream client: %v", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, openaiQuotaUpstreamTimeout)
	defer cancel()
	agentIdentity := s.isAgentIdentityAccount(ctx, accountID)

	var payload OpenAIQuotaResetResult
	for recovered := false; ; {
		headers, expectedTaskID, headerErr := s.buildCodexQuotaHeaders(callCtx, accountID, accessToken, chatGPTAccountID, fedRAMP)
		if headerErr != nil {
			return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_AUTH_FAILED", "failed to build upstream authentication: %v", headerErr)
		}
		headers["content-type"] = "application/json"
		body := map[string]string{"redeem_request_id": redeemRequestID}
		if targeted {
			body["credit_id"] = creditID
		}
		resp, responseBody, err := requestOpenAIQuotaJSON(
			callCtx,
			client,
			http.MethodPost,
			chatGPTRateLimitResetURL,
			headers,
			body,
			&payload,
		)
		if err != nil {
			if isOpenAIQuotaResponseTooLarge(err) {
				return nil, openAIQuotaResponseTooLargeError(err)
			}
			return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_RESET_REQUEST_FAILED", "upstream request failed: %v", err)
		}
		if !resp.IsSuccessState() {
			if agentIdentity && !recovered && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, responseBody) {
				recovered = true
				if err := s.recoverAgentIdentityTask(ctx, accountID, expectedTaskID); err != nil {
					return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_AUTH_FAILED", "agent identity task recovery failed: %v", err)
				}
				continue
			}
			status := resp.StatusCode
			if targeted {
				slog.Warn("openai_quota_targeted_reset_failed", "account_id", accountID, "status", status)
				return nil, infraerrors.Newf(mapUpstreamStatus(status), "OPENAI_QUOTA_RESET_UPSTREAM_ERROR", "upstream returned %d", status)
			}
			responseText := truncate(s.redactQuotaErrorBody(callCtx, accountID, string(responseBody)), 240)
			slog.Warn("openai_quota_reset_failed", "account_id", accountID, "status", status, "body", responseText)
			return nil, infraerrors.Newf(mapUpstreamStatus(status), "OPENAI_QUOTA_RESET_UPSTREAM_ERROR", "upstream returned %d: %s", status, responseText)
		}
		break
	}

	slog.Info("openai_quota_reset_success",
		"account_id", accountID,
		"code", payload.Code,
		"windows_reset", payload.WindowsReset,
	)
	return &payload, nil
}

// prepareUpstreamCall loads the account, validates it, obtains a fresh access
// token via the shared TokenProvider, and resolves the chatgpt-account-id and
// proxy URL. Centralized so QueryUsage / ResetCredit share validation.
func (s *OpenAIQuotaService) prepareUpstreamCall(ctx context.Context, accountID int64) (accessToken, chatGPTAccountID, proxyURL string, fedRAMP bool, err error) {
	if s == nil || s.accountRepo == nil || s.privacyClientFactory == nil {
		return "", "", "", false, infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_NOT_CONFIGURED", "openai quota service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID <= 0 {
		return "", "", "", false, ErrOpenAIQuotaInvalidAccountID
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return "", "", "", false, infraerrors.Newf(http.StatusNotFound, "OPENAI_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return "", "", "", false, infraerrors.New(http.StatusNotFound, "OPENAI_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account.Platform != PlatformOpenAI {
		return "", "", "", false, infraerrors.New(http.StatusBadRequest, "OPENAI_QUOTA_INVALID_PLATFORM", "account is not an OpenAI account")
	}
	if account.Type != AccountTypeOAuth {
		return "", "", "", false, infraerrors.New(http.StatusBadRequest, "OPENAI_QUOTA_INVALID_TYPE", "account is not an OAuth account")
	}

	// Spark shadow accounts do not hold their own credentials; resolve to the
	// parent account so that chatgpt_account_id / access_token / proxy all come
	// from the parent. This must happen BEFORE the chatgpt_account_id check.
	if account.IsShadow() {
		resolved, rerr := resolveCredentialAccount(ctx, s.accountRepo, account)
		if rerr != nil {
			return "", "", "", false, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_SHADOW_RESOLVE_FAILED", "failed to resolve shadow account: %v", rerr)
		}
		account = resolved
	}

	chatGPTAccountID = strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	if chatGPTAccountID == "" {
		// Fall back to organization_id — some legacy accounts only persisted poid.
		chatGPTAccountID = strings.TrimSpace(account.GetCredential("organization_id"))
	}
	if chatGPTAccountID == "" {
		return "", "", "", false, infraerrors.New(http.StatusBadRequest, "OPENAI_QUOTA_MISSING_ACCOUNT_ID", "chatgpt_account_id is missing; please re-authorize this account")
	}

	if !account.IsOpenAIAgentIdentity() {
		if s.tokenProvider == nil {
			return "", "", "", false, infraerrors.New(http.StatusInternalServerError, "OPENAI_QUOTA_NOT_CONFIGURED", "openai quota token provider is not configured")
		}
		accessToken, err = s.tokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			return "", "", "", false, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_TOKEN_UNAVAILABLE", "failed to acquire access token: %v", err)
		}
		if strings.TrimSpace(accessToken) == "" {
			return "", "", "", false, infraerrors.New(http.StatusBadGateway, "OPENAI_QUOTA_TOKEN_UNAVAILABLE", "access token is empty")
		}
	}
	fedRAMP = account.IsChatGPTAccountFedRAMP()

	// account.Proxy is eager-loaded by accountRepo.GetByID (see
	// repository.accountsToService), so we can read the proxy URL directly
	// instead of round-tripping the DB again. Fall back to proxyRepo only
	// when Proxy isn't pre-populated (defensive — e.g. callers that built
	// the Account by hand).
	if account.ProxyID != nil {
		if account.IsOpenAI() && s.proxyRepo != nil {
			// Refresh a matching relation as well: scheduler/account snapshots can
			// outlive an administrator disabling or expiring the proxy.
			proxy, perr := s.proxyRepo.GetByID(ctx, *account.ProxyID)
			if perr != nil || proxy == nil {
				return "", "", "", false, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_PROXY_UNAVAILABLE", "account proxy is configured but unavailable: %v", perr)
			}
			if proxy.ID != *account.ProxyID {
				return "", "", "", false, infraerrors.New(http.StatusBadGateway, "OPENAI_QUOTA_PROXY_UNAVAILABLE", "account proxy relation is inconsistent")
			}
			if proxyErr := validateConfiguredOpenAIProxy(proxy); proxyErr != nil {
				return "", "", "", false, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_PROXY_UNAVAILABLE", "%v", proxyErr)
			}
			proxyURL = proxy.URL()
		} else if account.Proxy != nil && account.Proxy.ID == *account.ProxyID {
			if account.IsOpenAI() {
				if proxyErr := validateConfiguredOpenAIProxy(account.Proxy); proxyErr != nil {
					return "", "", "", false, infraerrors.Newf(http.StatusBadGateway, "OPENAI_QUOTA_PROXY_UNAVAILABLE", "%v", proxyErr)
				}
			}
			proxyURL = account.Proxy.URL()
		}
		if strings.TrimSpace(proxyURL) == "" {
			return "", "", "", false, infraerrors.New(
				http.StatusBadGateway,
				"OPENAI_QUOTA_PROXY_UNAVAILABLE",
				"account proxy is configured but unavailable",
			)
		}
	}
	return accessToken, chatGPTAccountID, proxyURL, fedRAMP, nil
}

func (s *OpenAIQuotaService) recoverAgentIdentityTask(ctx context.Context, accountID int64, expectedTaskID string) error {
	if s == nil || s.accountRepo == nil {
		return fmt.Errorf("account repository is unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return fmt.Errorf("account is unavailable")
	}
	if account.IsShadow() {
		account, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil || account == nil {
			return fmt.Errorf("credential account is unavailable")
		}
	}
	if !account.IsOpenAIAgentIdentity() {
		return nil
	}
	return ensureAgentIdentityTaskForAccount(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, account, expectedTaskID)
}

func (s *OpenAIQuotaService) isAgentIdentityAccount(ctx context.Context, accountID int64) bool {
	if s == nil || s.accountRepo == nil {
		return false
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return false
	}
	if account.IsShadow() {
		account, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil || account == nil {
			return false
		}
	}
	return account.IsOpenAIAgentIdentity()
}

func (s *OpenAIQuotaService) buildCodexQuotaHeaders(ctx context.Context, accountID int64, accessToken, chatGPTAccountID string, fedRAMP bool) (map[string]string, string, error) {
	headers := buildCodexCommonHeaders(accessToken, chatGPTAccountID, fedRAMP)
	if s == nil || s.accountRepo == nil {
		return headers, "", nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		if strings.TrimSpace(accessToken) == "" {
			return nil, "", fmt.Errorf("agent identity account credentials are unavailable")
		}
		return headers, "", nil
	}
	if account.IsShadow() {
		if resolved, resolveErr := resolveCredentialAccount(ctx, s.accountRepo, account); resolveErr == nil && resolved != nil {
			account = resolved
		} else if strings.TrimSpace(accessToken) == "" {
			return nil, "", fmt.Errorf("agent identity shadow credentials are unavailable")
		}
	}
	if !account.IsOpenAIAgentIdentity() {
		return headers, "", nil
	}
	if err := ensureAgentIdentityTaskForAccount(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, account, ""); err != nil {
		return nil, "", err
	}
	key, err := agentIdentityKeyFromAccount(account)
	if err != nil {
		return nil, "", err
	}
	assertion, err := buildAgentAssertion(key, time.Now())
	if err != nil {
		return nil, "", err
	}
	headers["authorization"] = assertion
	return headers, key.taskID, nil
}

func (s *OpenAIQuotaService) redactQuotaErrorBody(ctx context.Context, accountID int64, body string) string {
	if s == nil || s.accountRepo == nil {
		return body
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return body
	}
	return string(redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, account, []byte(body)))
}

// buildCodexCommonHeaders sets the request headers expected by the chatgpt.com
// backend so calls succeed past Cloudflare/WASM checks.
func buildCodexCommonHeaders(accessToken, chatGPTAccountID string, fedRAMP bool) map[string]string {
	headers := map[string]string{
		"authorization":      "Bearer " + accessToken,
		"chatgpt-account-id": chatGPTAccountID,
		"openai-beta":        openaiQuotaCodexBeta,
		"oai-language":       openaiQuotaCodexLanguageTag,
		"originator":         openaiQuotaCodexOriginator,
		"accept":             "application/json",
		"sec-fetch-site":     openaiQuotaSecFetchSite,
		"sec-fetch-mode":     openaiQuotaSecFetchMode,
		"sec-fetch-dest":     openaiQuotaSecFetchDest,
		"priority":           "u=4, i",
	}
	if fedRAMP {
		headers["x-openai-fedramp"] = "true"
	}
	return headers
}

// generateRedeemRequestID produces a UUID-v4-shaped string without pulling in a
// new dependency. ChatGPT uses this as an idempotency key for the consume call.
func generateRedeemRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Set version (4) and variant (RFC 4122) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:]), nil
}

// buildCodexSparkWindowExtraUpdates extracts Codex Spark usage windows from the
// /wham/usage response body's additional_rate_limits, matching the entry with
// MeteredFeature == "codex_bengalfox". It produces plain codex_* keys (NOT the
// Method-Z "codex_spark_" prefix) so that a spark shadow account's extra map
// is populated with the same key names used by the scheduling / frontend layers.
// Returns nil when no codex_bengalfox entry is present or when the RateLimit
// yields no window data.
func buildCodexSparkWindowExtraUpdates(usage *OpenAIQuotaUsage, now time.Time) map[string]any {
	if usage == nil {
		return nil
	}
	var spark *OpenAIRateLimit
	for i := range usage.AdditionalRateLimits {
		a := usage.AdditionalRateLimits[i]
		if a.MeteredFeature == "codex_bengalfox" {
			spark = a.RateLimit
			break
		}
	}
	return buildCodexRateLimitWindowExtraUpdates(spark, now)
}

// buildCodexWindowExtraUpdates maps the ordinary /wham/usage rate_limit into
// the same canonical keys consumed by scheduling and the account table.
func buildCodexWindowExtraUpdates(usage *OpenAIQuotaUsage, now time.Time) map[string]any {
	if usage == nil {
		return nil
	}
	return buildCodexRateLimitWindowExtraUpdates(usage.RateLimit, now)
}

func buildCodexRateLimitWindowExtraUpdates(rateLimit *OpenAIRateLimit, now time.Time) map[string]any {
	if rateLimit == nil {
		return nil
	}

	// Reuse OpenAICodexUsageSnapshot / Normalize to map primary/secondary windows
	// to canonical 5h/7d buckets (same logic as probeOpenAICodexSnapshot).
	snap := &OpenAICodexUsageSnapshot{}
	if w := rateLimit.PrimaryWindow; w != nil {
		p := w.UsedPercent
		snap.PrimaryUsedPercent = &p
		ra := openAIQuotaWindowResetAfterSeconds(w, now)
		snap.PrimaryResetAfterSeconds = &ra
		wm := int(w.LimitWindowSeconds / 60)
		snap.PrimaryWindowMinutes = &wm
	}
	if w := rateLimit.SecondaryWindow; w != nil {
		p := w.UsedPercent
		snap.SecondaryUsedPercent = &p
		ra := openAIQuotaWindowResetAfterSeconds(w, now)
		snap.SecondaryResetAfterSeconds = &ra
		wm := int(w.LimitWindowSeconds / 60)
		snap.SecondaryWindowMinutes = &wm
	}

	normalized := snap.Normalize()
	if normalized == nil {
		return nil
	}

	updates := make(map[string]any)
	if normalized.Used5hPercent != nil {
		updates[OpenAIQuotaUsed5hPercentExtraKey] = *normalized.Used5hPercent
	}
	if normalized.Reset5hSeconds != nil {
		updates[OpenAIQuotaReset5hSecondsExtraKey] = *normalized.Reset5hSeconds
	}
	if normalized.Window5hMinutes != nil {
		updates[OpenAIQuotaWindow5hMinutesExtraKey] = *normalized.Window5hMinutes
	}
	if normalized.Used7dPercent != nil {
		updates[OpenAIQuotaUsed7dPercentExtraKey] = *normalized.Used7dPercent
	}
	if normalized.Reset7dSeconds != nil {
		updates[OpenAIQuotaReset7dSecondsExtraKey] = *normalized.Reset7dSeconds
	}
	if normalized.Window7dMinutes != nil {
		updates[OpenAIQuotaWindow7dMinutesExtraKey] = *normalized.Window7dMinutes
	}
	if r := codexResetAtRFC3339(now, normalized.Reset5hSeconds); r != nil {
		updates[OpenAIQuotaReset5hAtExtraKey] = *r
	}
	if r := codexResetAtRFC3339(now, normalized.Reset7dSeconds); r != nil {
		updates[OpenAIQuotaReset7dAtExtraKey] = *r
	}
	addCodexUsageWindowTombstones(updates, snap, normalized)
	if len(updates) == 0 {
		return nil
	}
	updates[OpenAIQuotaUsageUpdatedAtExtraKey] = now.Format(time.RFC3339)
	if !now.IsZero() {
		updates[OpenAICodexUsageObservedAtUnixNanoExtraKey] = now.UnixNano()
	}
	return updates
}

func openAIQuotaWindowResetAfterSeconds(window *OpenAIRateLimitWindow, now time.Time) int {
	if window == nil {
		return 0
	}
	if window.ResetAt > now.Unix() {
		remaining := window.ResetAt - now.Unix()
		if remaining >= 0 && remaining <= maxCodexResetAfterSeconds {
			return int(remaining)
		}
	}
	if window.ResetAfterSeconds < 0 || window.ResetAfterSeconds > maxCodexResetAfterSeconds {
		return 0
	}
	return int(window.ResetAfterSeconds)
}

func buildOpenAIQuotaExtraUpdates(account *Account, usage *OpenAIQuotaUsage, now time.Time) map[string]any {
	if account == nil || usage == nil || !account.IsOpenAIOAuth() {
		return nil
	}
	var updates map[string]any
	if account.IsShadow() {
		updates = buildCodexSparkWindowExtraUpdates(usage, now)
	} else {
		updates = buildCodexWindowExtraUpdates(usage, now)
		if usage.Credits != nil {
			if updates == nil {
				updates = make(map[string]any)
			}
			updates[openaiQuotaCreditBalanceKey] = &OpenAICodexCreditSnapshot{
				OpenAICodexCredits: *usage.Credits,
				UpdatedAt:          now.UTC().Format(time.RFC3339),
			}
		}
		if planType := strings.TrimSpace(usage.PlanType); planType != "" {
			if updates == nil {
				updates = make(map[string]any)
			}
			updates[OpenAIQuotaPlanTypeExtraKey] = planType
		}
		if usage.SpendControl != nil {
			if updates == nil {
				updates = make(map[string]any)
			}
			// Store a value copy so callers cannot mutate the cached snapshot
			// through the response object after persistence is scheduled.
			spendControl := *usage.SpendControl
			if usage.SpendControl.IndividualLimit != nil {
				limitCopy := *usage.SpendControl.IndividualLimit
				spendControl.IndividualLimit = &limitCopy
			}
			updates[OpenAIQuotaSpendControlExtraKey] = &spendControl
		}
	}
	if len(updates) > 0 {
		// Credits and spend-control-only WHAM responses are valid snapshots too.
		// Stamp them even when no rate-limit window was returned so the repository
		// can order those writes against concurrent window observations.
		if _, ok := updates[OpenAICodexUsageObservedAtUnixNanoExtraKey]; !ok && !now.IsZero() {
			updates[OpenAICodexUsageObservedAtUnixNanoExtraKey] = now.UnixNano()
		}
	}
	return updates
}

// mapUpstreamStatus collapses upstream HTTP statuses into a stable set we
// surface from the admin handler. 4xx upstream errors are surfaced as 502
// (BadGateway) so callers can distinguish "your input is bad" (400) from
// "upstream said no" (502); 401/403 are bubbled directly to hint at re-auth.
func mapUpstreamStatus(status int) int {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status
	case status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case status >= 400 && status < 500:
		return http.StatusBadGateway
	case status >= 500:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}
