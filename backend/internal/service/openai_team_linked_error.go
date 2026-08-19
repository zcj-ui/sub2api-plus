package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openAITeamLinkedErrorDedupTTL      = 60 * time.Second
	openAITeamLinkedErrorFanoutTimeout = 30 * time.Second
	openAITeamLinkedErrorBlockReason   = "team_linked_error"
)

// maybeHandleOpenAITeamLinkedError propagates a deactivated_workspace error
// to other OAuth accounts belonging to the same ChatGPT workspace. This is
// deliberately limited to OpenAI OAuth accounts and excludes Spark shadows.
func (s *RateLimitService) maybeHandleOpenAITeamLinkedError(ctx context.Context, account *Account, statusCode int, responseBody []byte) {
	if s == nil || s.accountRepo == nil || statusCode != http.StatusPaymentRequired ||
		account == nil || !account.IsOpenAIOAuth() {
		return
	}
	if strings.TrimSpace(gjson.GetBytes(responseBody, "detail.code").String()) != "deactivated_workspace" {
		return
	}
	teamID := strings.TrimSpace(account.GetChatGPTAccountID())
	if teamID == "" || !s.markOpenAITeamLinkedFired(teamID) {
		return
	}

	base := ctx
	if base == nil {
		base = context.Background()
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(base), openAITeamLinkedErrorFanoutTimeout)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(opCtx, PlatformOpenAI)
	if err != nil {
		slog.Warn("openai_team_linked_error_list_failed", "trigger_account_id", account.ID, "error", err)
		return
	}
	targets := make([]*Account, 0, len(accounts))
	for i := range accounts {
		candidate := &accounts[i]
		if candidate.ID == account.ID || candidate.IsShadow() ||
			strings.TrimSpace(candidate.GetChatGPTAccountID()) != teamID {
			continue
		}
		targets = append(targets, candidate)
	}
	if len(targets) == 0 {
		return
	}
	for _, candidate := range targets {
		s.notifyAccountSchedulingBlocked(candidate, time.Time{}, openAITeamLinkedErrorBlockReason)
	}
	errorMessage := fmt.Sprintf("Workspace deactivated (402): team-linked error triggered by account #%d", account.ID)
	marked := 0
	for _, candidate := range targets {
		if err := s.accountRepo.SetError(opCtx, candidate.ID, errorMessage); err != nil {
			slog.Warn("openai_team_linked_error_set_error_failed", "account_id", candidate.ID, "error", err)
			continue
		}
		marked++
	}
	slog.Warn("openai_team_linked_error_fanout",
		"trigger_account_id", account.ID,
		"chatgpt_account_id", teamID,
		"affected", marked,
		"targets", len(targets),
	)
}

func (s *RateLimitService) markOpenAITeamLinkedFired(teamID string) bool {
	if s == nil || strings.TrimSpace(teamID) == "" {
		return false
	}
	now := time.Now()
	s.openaiTeamLinkedMu.Lock()
	defer s.openaiTeamLinkedMu.Unlock()
	if s.openaiTeamLinkedRecent == nil {
		s.openaiTeamLinkedRecent = make(map[string]time.Time)
	}
	if expiry, ok := s.openaiTeamLinkedRecent[teamID]; ok && expiry.After(now) {
		return false
	}
	for key, expiry := range s.openaiTeamLinkedRecent {
		if !expiry.After(now) {
			delete(s.openaiTeamLinkedRecent, key)
		}
	}
	s.openaiTeamLinkedRecent[teamID] = now.Add(openAITeamLinkedErrorDedupTTL)
	return true
}
