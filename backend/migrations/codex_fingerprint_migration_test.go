package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration229CanonicalizesCodexFingerprintModesAndRepairsSeeds(t *testing.T) {
	content, err := FS.ReadFile("229_repair_codex_fingerprint_mode_format.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "LOWER(BTRIM(extra->>'codex_fingerprint_mode'))")
	require.Contains(t, sql, "IN ('off', 'device', 'session', 'full')")
	require.Contains(t, sql, "THEN 'device'")
	require.Contains(t, sql, "IS DISTINCT FROM CASE")
	require.Contains(t, sql, "gen_random_uuid()")
	require.Contains(t, sql, "!~*")
	require.Contains(t, sql, "platform = 'openai'")
	require.Contains(t, sql, "type = 'oauth'")
}
