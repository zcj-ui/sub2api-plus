//go:build unit

package sysutil

import (
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestRestartServiceSupportError(t *testing.T) {
	require.NoError(t, restartServiceSupportError("linux"))

	for _, goos := range []string{"windows", "darwin", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			err := restartServiceSupportError(goos)
			require.ErrorIs(t, err, ErrRestartUnsupportedPlatform)
			require.Equal(t, http.StatusConflict, infraerrors.Code(err))
			require.Equal(t, "SYSTEM_RESTART_UNSUPPORTED_PLATFORM", infraerrors.Reason(err))
			require.NotContains(t, infraerrors.Message(err), goos)
		})
	}
}
