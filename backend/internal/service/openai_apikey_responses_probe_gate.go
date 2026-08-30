package service

import "sync"

// The admin create/update paths intentionally trigger the Responses probe in
// the background.  A rapid edit followed by a retry can otherwise start two
// full multi-model probe loops for the same API-key account.  Keep one probe
// in flight per account; this is process-local coordination only and does not
// change the persisted legacy capability contract.
var openaiResponsesProbeInFlight = struct {
	sync.Mutex
	accounts map[int64]struct{}
}{accounts: make(map[int64]struct{})}

func beginOpenAIResponsesProbe(accountID int64) bool {
	if accountID <= 0 {
		return true
	}
	openaiResponsesProbeInFlight.Lock()
	defer openaiResponsesProbeInFlight.Unlock()
	if _, exists := openaiResponsesProbeInFlight.accounts[accountID]; exists {
		return false
	}
	openaiResponsesProbeInFlight.accounts[accountID] = struct{}{}
	return true
}

func endOpenAIResponsesProbe(accountID int64) {
	if accountID <= 0 {
		return
	}
	openaiResponsesProbeInFlight.Lock()
	delete(openaiResponsesProbeInFlight.accounts, accountID)
	openaiResponsesProbeInFlight.Unlock()
}
