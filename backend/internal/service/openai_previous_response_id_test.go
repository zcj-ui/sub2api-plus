package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIPreviousResponseIDKind(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "empty", id: " ", want: OpenAIPreviousResponseIDKindEmpty},
		{name: "response_id", id: "resp_0906a621bc423a8d0169a108637ef88197b74b0e2f37ba358f", want: OpenAIPreviousResponseIDKindResponseID},
		{name: "message_id", id: "msg_123456", want: OpenAIPreviousResponseIDKindMessageID},
		{name: "item_id", id: "item_abcdef", want: OpenAIPreviousResponseIDKindMessageID},
		{name: "unknown", id: "foo_123456", want: OpenAIPreviousResponseIDKindUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyOpenAIPreviousResponseIDKind(tc.id); got != tc.want {
				t.Fatalf("ClassifyOpenAIPreviousResponseIDKind(%q)=%q want=%q", tc.id, got, tc.want)
			}
		})
	}
}

func TestIsOpenAIPreviousResponseIDLikelyMessageID(t *testing.T) {
	if !IsOpenAIPreviousResponseIDLikelyMessageID("msg_123") {
		t.Fatal("expected msg_123 to be identified as message id")
	}
	if IsOpenAIPreviousResponseIDLikelyMessageID("resp_123") {
		t.Fatal("expected resp_123 not to be identified as message id")
	}
}

func TestParseOpenAIPreviousResponseIDField(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "missing", body: `{}`, want: ""},
		{name: "null", body: `{"previous_response_id":null}`, want: ""},
		{name: "trimmed string", body: `{"previous_response_id":"  resp_abc  "}`, want: "resp_abc"},
		{name: "number", body: `{"previous_response_id":123}`, wantErr: "string or null"},
		{name: "object", body: `{"previous_response_id":{}}`, wantErr: "string or null"},
		{name: "duplicate", body: `{"previous_response_id":"resp_a","previous_response_id":"resp_b"}`, wantErr: "only once"},
		{name: "control", body: "{\"previous_response_id\":\"resp_abc" + "\n" + "\"}", wantErr: "control"},
		{name: "oversized", body: `{"previous_response_id":"` + strings.Repeat("x", OpenAIPreviousResponseIDMaxBytes+1) + `"}`, wantErr: "at most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOpenAIPreviousResponseIDField([]byte(tt.body))
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
