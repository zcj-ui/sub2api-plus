package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAICodexCredits_UnmarshalBalanceStringOrNumber(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		balance string
	}{
		{
			name:    "string preserves decimal precision",
			payload: `{"credits":{"has_credits":true,"balance":"25.000000000000000001"}}`,
			balance: "25.000000000000000001",
		},
		{
			name:    "number preserves source representation",
			payload: `{"credits":{"has_credits":true,"balance":25.000000000000000001}}`,
			balance: "25.000000000000000001",
		},
		{
			name:    "integer number",
			payload: `{"credits":{"has_credits":true,"balance":25}}`,
			balance: "25",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var usage OpenAIQuotaUsage
			require.NoError(t, json.Unmarshal([]byte(tt.payload), &usage))
			require.NotNil(t, usage.Credits)
			require.Equal(t, tt.balance, usage.Credits.Balance)
		})
	}
}

func TestOpenAICodexCredits_UnmarshalRejectsNonNumericBalance(t *testing.T) {
	var usage OpenAIQuotaUsage
	err := json.Unmarshal([]byte(`{"credits":{"balance":false}}`), &usage)
	require.Error(t, err)
}
