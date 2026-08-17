package service

import "testing"

func TestAccountCodex429GuardEnabled(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil", account: nil, want: false},
		{name: "claude oauth", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, want: false},
		{name: "openai api key", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: false},
		{name: "openai oauth missing setting is opt-in off", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, want: false},
		{name: "openai oauth shadow excluded", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: int64PtrForCodexGuardTest(1)}, want: false},
		{name: "openai oauth spark dimension excluded without parent", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, QuotaDimension: QuotaDimensionSpark}, want: false},
		{name: "enabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAICodex429GuardEnabledExtraKey: true}}, want: true},
		{name: "disabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAICodex429GuardEnabledExtraKey: false}}, want: false},
		{name: "string disabled", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAICodex429GuardEnabledExtraKey: "false"}}, want: false},
		{name: "invalid string fails closed", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAICodex429GuardEnabledExtraKey: "unexpected"}}, want: false},
		{name: "nil setting is opt-in off", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAICodex429GuardEnabledExtraKey: nil}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.Codex429GuardEnabled(); got != tt.want {
				t.Fatalf("Codex429GuardEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func int64PtrForCodexGuardTest(value int64) *int64 { return &value }
