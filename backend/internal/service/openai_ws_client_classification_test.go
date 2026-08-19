package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestIsOpenAIOfficialCodexWSClientUsesOnlyInboundIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(userAgent, originator string) *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		if userAgent != "" {
			c.Request.Header.Set("User-Agent", userAgent)
		}
		if originator != "" {
			c.Request.Header.Set("originator", originator)
		}
		return c
	}

	require.False(t, isOpenAIOfficialCodexWSClient(newContext("curl/8.0", "")))
	require.False(t, isOpenAIOfficialCodexWSClient(newContext("", "")))
	require.True(t, isOpenAIOfficialCodexWSClient(newContext("codex_cli_rs/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color", "codex_cli_rs")))
	// A gateway-level ForceCodexCLI setting is intentionally not an input to
	// this classifier; generic traffic remains generic even when old deployments
	// still carry that setting.
}
