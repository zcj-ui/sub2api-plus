//go:build unit

package admin

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestResolveTestSMTPUseTLS_InheritsSavedConfigWhenOmitted(t *testing.T) {
	saved := &service.SMTPConfig{UseTLS: true}
	require.True(t, resolveTestSMTPUseTLS(nil, saved))
}

func TestResolveTestSMTPUseTLS_ExplicitFalseOverridesSavedConfig(t *testing.T) {
	value := false
	saved := &service.SMTPConfig{UseTLS: true}
	require.False(t, resolveTestSMTPUseTLS(&value, saved))
}

func TestTestSMTPRequestJSONDistinguishesOmittedAndExplicitFalse(t *testing.T) {
	var omitted TestSMTPRequest
	require.NoError(t, json.Unmarshal([]byte(`{"smtp_host":"smtp.example"}`), &omitted))
	require.Nil(t, omitted.SMTPUseTLS)

	var explicit TestSMTPRequest
	require.NoError(t, json.Unmarshal([]byte(`{"smtp_host":"smtp.example","smtp_use_tls":false}`), &explicit))
	require.NotNil(t, explicit.SMTPUseTLS)
	require.False(t, *explicit.SMTPUseTLS)
}
