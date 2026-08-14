package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildAnthropicRelayEndpointURL(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		endpoint string
		want     string
	}{
		{name: "root messages", base: "https://relay.example", endpoint: "/v1/messages", want: "https://relay.example/v1/messages"},
		{name: "version messages", base: "https://relay.example/prefix/v1", endpoint: "/v1/messages", want: "https://relay.example/prefix/v1/messages"},
		{name: "complete messages idempotent", base: "https://relay.example/prefix/v1/messages?channel=cc", endpoint: "/v1/messages", want: "https://relay.example/prefix/v1/messages?channel=cc"},
		{name: "messages to models sibling", base: "https://relay.example/prefix/v1/messages?channel=cc", endpoint: "/v1/models", want: "https://relay.example/prefix/v1/models?channel=cc"},
		{name: "models to messages sibling", base: "https://relay.example/prefix/v1/models", endpoint: "/v1/messages", want: "https://relay.example/prefix/v1/messages"},
		{name: "complete models idempotent", base: "https://relay.example/prefix/v1/models", endpoint: "/v1/models", want: "https://relay.example/prefix/v1/models"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, buildAnthropicRelayEndpointURL(tt.base, tt.endpoint))
		})
	}
}
