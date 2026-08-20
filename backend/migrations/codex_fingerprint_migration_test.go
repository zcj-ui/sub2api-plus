package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration229CanonicalizesCodexFingerprintModesAndRepairsSeeds(t *testing.T) {
	content, err := FS.ReadFile("229_repair_codex_fingerprint_mode_format.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "LOWER(BTRIM(extra->>'codex_fingerprint_mode'))")
	require.Contains(t, sql, "IN ('off', 'device', 'session', 'full')")
	require.NotContains(t, sql, "THEN 'device'")
	require.Contains(t, sql, "IS DISTINCT FROM LOWER(BTRIM(extra->>'codex_fingerprint_mode'))")
	require.Contains(t, sql, "gen_random_uuid()")
	require.Contains(t, sql, "LOWER(BTRIM(extra->>'codex_fingerprint_seed')) !~ '")
	require.NotContains(t, sql, "-4[0-9a-f]")
	require.Contains(t, sql, "00000000-0000-0000-0000-000000000000")
	require.Contains(t, sql, "IN ('device', 'session', 'full')")
	require.Contains(t, sql, "platform = 'openai'")
	require.Contains(t, sql, "type = 'oauth'")
	require.GreaterOrEqual(t, strings.Count(sql, "deleted_at IS NULL"), 3)
	require.Contains(t, sql, "to_jsonb(LOWER(BTRIM(extra->>'codex_fingerprint_seed')))")
}

func TestMigration227DoesNotCollapseCodexFingerprintModes(t *testing.T) {
	content, err := FS.ReadFile("227_codex_fingerprint_device_only.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "SELECT 1;")
	require.NotContains(t, sql, "UPDATE accounts")
	require.NotContains(t, sql, "to_jsonb('device'::text)")
}

func TestMigration228PreservesAnyCanonicalNonNilUUIDSeed(t *testing.T) {
	content, err := FS.ReadFile("228_repair_codex_fingerprint_seed.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "!~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'")
	require.NotContains(t, sql, "-4[0-9a-f]")
	require.Contains(t, sql, "LOWER(BTRIM(extra->>'codex_fingerprint_seed')) = '00000000-0000-0000-0000-000000000000'")
}

func TestMigration230MarksOnlyAmbiguousHistoricalCodexRows(t *testing.T) {
	content, err := FS.ReadFile("230_mark_codex_fingerprint_recovery_required.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "FROM schema_migrations")
	require.Contains(t, sql, "226_converge_codex_fingerprint_modes_to_device.sql")
	require.Contains(t, sql, "227_codex_fingerprint_device_only.sql")
	require.Contains(t, sql, "229_repair_codex_fingerprint_mode_format.sql")
	require.Contains(t, sql, "codex_fingerprint_recovery_required")
	require.Contains(t, sql, "MAX(applied_at)")
	require.Contains(t, sql, "a.created_at <= d.applied_at")
	require.Contains(t, sql, "a.platform = 'openai'")
	require.Contains(t, sql, "a.type = 'oauth'")
	require.Contains(t, sql, "LOWER(BTRIM(a.extra->>'codex_fingerprint_mode')) = 'device'")
	// The migration must never infer session/full or rewrite the selected mode.
	require.NotContains(t, sql, "to_jsonb('session'::text)")
	require.NotContains(t, sql, "to_jsonb('full'::text)")
	require.NotContains(t, sql, "'{codex_fingerprint_mode}'")
}

func TestMigration231RepairsOnlyActiveCodexFingerprintSeeds(t *testing.T) {
	content, err := FS.ReadFile("231_repair_active_codex_fingerprint_seed.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "Keep 225 immutable")
	require.GreaterOrEqual(t, strings.Count(sql, "deleted_at IS NULL"), 2)
	require.Contains(t, sql, "LOWER(BTRIM(COALESCE(extra->>'codex_fingerprint_mode', ''))) IN ('device', 'session', 'full')")
	require.Contains(t, sql, "to_jsonb(LOWER(BTRIM(extra->>'codex_fingerprint_seed')))")
	require.Contains(t, sql, "gen_random_uuid()")
	require.NotContains(t, sql, "-4[0-9a-f]")
	require.Contains(t, sql, "00000000-0000-0000-0000-000000000000")
}
