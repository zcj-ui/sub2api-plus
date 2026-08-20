//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func requireCanonicalUUIDString(t *testing.T, value string) {
	t.Helper()
	parsed, err := uuid.Parse(value)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, parsed)
	require.Equal(t, parsed.String(), value)
}

func TestCodexFingerprintSeedMigrationsPreservePublished225AndRepairActiveRows(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	migrationSQL := make(map[string]string, 4)
	for _, name := range []string{
		"225_backfill_codex_fingerprint_seed.sql",
		"228_repair_codex_fingerprint_seed.sql",
		"229_repair_codex_fingerprint_mode_format.sql",
		"231_repair_active_codex_fingerprint_seed.sql",
	} {
		content, err := dbmigrations.FS.ReadFile(name)
		require.NoError(t, err)
		migrationSQL[name] = string(content)
	}

	insertAccount := func(name, accountType, extra string, deleted bool) int64 {
		t.Helper()
		query := `
INSERT INTO accounts (name, platform, type, extra)
VALUES ($1, 'openai', $2, $3::jsonb)
RETURNING id
`
		if deleted {
			query = `
INSERT INTO accounts (name, platform, type, extra, deleted_at)
VALUES ($1, 'openai', $2, $3::jsonb, NOW())
RETURNING id
`
		}
		var id int64
		require.NoError(t, tx.QueryRowContext(ctx, query, name, accountType, extra).Scan(&id))
		return id
	}
	readSeed := func(id int64) string {
		t.Helper()
		var seed string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT COALESCE(extra->>'codex_fingerprint_seed', '') FROM accounts WHERE id = $1`, id).Scan(&seed))
		return seed
	}
	requireNoSeed := func(id int64) {
		t.Helper()
		var hasSeed bool
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra ? 'codex_fingerprint_seed' FROM accounts WHERE id = $1`, id).Scan(&hasSeed))
		require.False(t, hasSeed)
	}

	missingID := insertAccount("migration-225-missing", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"session"}`, false)
	blankID := insertAccount("migration-225-blank", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"device","codex_fingerprint_seed":""}`, false)
	malformedID := insertAccount("migration-225-malformed", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"full","codex_fingerprint_seed":"BAD"}`, false)
	validID := insertAccount("migration-225-valid", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"session","codex_fingerprint_seed":"11111111-1111-4111-8111-111111111111"}`, false)
	upperCaseID := insertAccount("migration-225-uppercase", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"full","codex_fingerprint_seed":"11111111-1111-4111-8111-11111111111A"}`, false)
	offID := insertAccount("migration-225-off", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"off"}`, false)
	apiKeyID := insertAccount("migration-225-apikey", service.AccountTypeAPIKey, `{"codex_fingerprint_mode":"session"}`, false)

	// 225 is immutable in deployed databases. Its original semantics only fill
	// blank values; malformed seeds are repaired by later forward migrations.
	_, err := tx.ExecContext(ctx, migrationSQL["225_backfill_codex_fingerprint_seed.sql"])
	require.NoError(t, err)
	for _, id := range []int64{missingID, blankID} {
		requireCanonicalUUIDString(t, readSeed(id))
	}
	require.Equal(t, "BAD", readSeed(malformedID))
	require.Equal(t, "11111111-1111-4111-8111-111111111111", readSeed(validID))
	require.Equal(t, "11111111-1111-4111-8111-11111111111A", readSeed(upperCaseID))
	requireNoSeed(offID)
	requireNoSeed(apiKeyID)

	for _, name := range []string{
		"228_repair_codex_fingerprint_seed.sql",
		"229_repair_codex_fingerprint_mode_format.sql",
	} {
		_, err := tx.ExecContext(ctx, migrationSQL[name])
		require.NoError(t, err)
	}
	lateMalformedID := insertAccount("migration-231-late-malformed", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"session","codex_fingerprint_seed":"BAD"}`, false)
	_, err = tx.ExecContext(ctx, migrationSQL["231_repair_active_codex_fingerprint_seed.sql"])
	require.NoError(t, err)

	seedsAfterRepair := map[int64]string{}
	for _, id := range []int64{missingID, blankID, malformedID, validID, upperCaseID, lateMalformedID} {
		seed := readSeed(id)
		requireCanonicalUUIDString(t, seed)
		seedsAfterRepair[id] = seed
	}
	require.Equal(t, "11111111-1111-4111-8111-111111111111", seedsAfterRepair[validID])
	require.Equal(t, "11111111-1111-4111-8111-11111111111a", seedsAfterRepair[upperCaseID])
	requireNoSeed(offID)
	requireNoSeed(apiKeyID)

	// The forward repair is idempotent and deliberately skips soft-deleted rows.
	deletedID := insertAccount("migration-231-deleted", service.AccountTypeOAuth, `{"codex_fingerprint_mode":"session","codex_fingerprint_seed":"BAD"}`, true)
	_, err = tx.ExecContext(ctx, migrationSQL["231_repair_active_codex_fingerprint_seed.sql"])
	require.NoError(t, err)
	require.Equal(t, "BAD", readSeed(deletedID))
	for id, want := range seedsAfterRepair {
		require.Equal(t, want, readSeed(id))
	}
}

func TestBulkUpdateGeneratesDistinctStableCodexFingerprintSeedsPerEligibleRow(t *testing.T) {
	ctx := context.Background()
	testName := "bulk-codex-seed-" + uuid.NewString()
	type fixture struct {
		name        string
		accountType string
		extra       string
	}
	fixtures := []fixture{
		{name: testName + "-missing-a", accountType: service.AccountTypeOAuth, extra: `{}`},
		{name: testName + "-missing-b", accountType: service.AccountTypeOAuth, extra: `{"codex_fingerprint_seed":"BAD"}`},
		{name: testName + "-valid", accountType: service.AccountTypeOAuth, extra: `{"codex_fingerprint_seed":"11111111-1111-4111-8111-111111111111"}`},
		{name: testName + "-apikey", accountType: service.AccountTypeAPIKey, extra: `{}`},
	}

	ids := make([]int64, 0, len(fixtures))
	for _, f := range fixtures {
		var id int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, extra)
VALUES ($1, 'openai', $2, $3::jsonb)
RETURNING id
`, f.name, f.accountType, f.extra).Scan(&id))
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id = ANY($1)`, pq.Array(ids))
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, pq.Array(ids))
	})

	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	updates := service.AccountBulkUpdate{
		Extra: map[string]any{
			"codex_fingerprint_mode": "session",
		},
		EnsureCodexFingerprintSeed: true,
	}
	rows, err := repo.BulkUpdate(ctx, ids, updates)
	require.NoError(t, err)
	require.Equal(t, int64(len(ids)), rows)

	readSeed := func(id int64) string {
		t.Helper()
		var seed string
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(extra->>'codex_fingerprint_seed', '') FROM accounts WHERE id = $1`, id).Scan(&seed))
		return seed
	}
	firstSeeds := []string{readSeed(ids[0]), readSeed(ids[1]), readSeed(ids[2]), readSeed(ids[3])}
	requireCanonicalUUIDString(t, firstSeeds[0])
	requireCanonicalUUIDString(t, firstSeeds[1])
	require.NotEqual(t, firstSeeds[0], firstSeeds[1], "gen_random_uuid must be evaluated per eligible row")
	require.Equal(t, "11111111-1111-4111-8111-111111111111", firstSeeds[2])
	require.Empty(t, firstSeeds[3], "API-key accounts must not receive a Codex fingerprint seed")

	rows, err = repo.BulkUpdate(ctx, ids, updates)
	require.NoError(t, err)
	require.Equal(t, int64(len(ids)), rows)
	for i, want := range firstSeeds {
		require.Equal(t, want, readSeed(ids[i]), "retry must not rotate an existing valid seed")
	}
}
