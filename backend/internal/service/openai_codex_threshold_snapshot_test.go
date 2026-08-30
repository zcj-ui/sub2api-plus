package service

import (
	"context"
	"testing"
	"time"
)

func TestUpdateCodexUsageSnapshot_CriticalThresholdBypassesOrdinaryThrottle(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 2)}
	svc := &OpenAIGatewayService{
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Hour),
	}
	account := &Account{
		ID:       9101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"auto_pause_5h_threshold": 0.9,
		},
	}
	used := 95.0
	reset := 1800
	window := 300
	snapshot := &OpenAICodexUsageSnapshot{
		SecondaryUsedPercent:       &used,
		SecondaryResetAfterSeconds: &reset,
		SecondaryWindowMinutes:     &window,
	}

	// An ordinary write consumes the normal throttle slot first.
	svc.updateCodexUsageSnapshot(context.Background(), account.ID, snapshot)
	// The threshold crossing must still persist immediately despite that slot.
	svc.updateCodexUsageSnapshot(context.Background(), account.ID, snapshot, account)

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary snapshot write did not complete")
	}
	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("critical threshold snapshot was throttled")
	}
}

func TestUpdateCodexUsageSnapshot_CriticalWriteUsesAccountContextAndIsSynchronous(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &OpenAIGatewayService{
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Hour),
	}
	account := &Account{
		ID:       9102,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"auto_pause_5h_threshold": 0.9},
	}
	used := 100.0
	reset := 1800
	window := 300
	snapshot := &OpenAICodexUsageSnapshot{
		SecondaryUsedPercent:       &used,
		SecondaryResetAfterSeconds: &reset,
		SecondaryWindowMinutes:     &window,
	}

	svc.updateCodexUsageSnapshot(context.Background(), account.ID, snapshot, account)
	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_5h_used_percent"]; got != 100.0 {
			t.Fatalf("critical snapshot used percent = %v, want 100", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("critical threshold snapshot was not persisted synchronously")
	}
}
