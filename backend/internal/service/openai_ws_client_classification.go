package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

// isOpenAIOfficialCodexWSClient reports only what the inbound client declared.
// Deprecated gateway settings are intentionally excluded: selecting a request
// path must never manufacture an official Codex identity or enable its
// client-only mutations for a generic caller.
func isOpenAIOfficialCodexWSClient(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return openai.IsCodexOfficialClientByHeaders(
		c.GetHeader("User-Agent"),
		c.GetHeader("originator"),
	)
}
