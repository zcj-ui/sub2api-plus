package repository

import (
	"context"
	"database/sql"
	"io/fs"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/migrations"
)

// TestPlusMigrationSequenceKeepsPublished231Immutable verifies the upgrade
// contract used by the Plus branch: 231_repair_active_codex_fingerprint_seed
// is already shipped and therefore stays at its original filename, while the
// two upstream migrations receive fresh, monotonic filenames.  The runner
// keys history by filename, so renaming the published file would turn a
// harmless upgrade into a checksum/history break.
func TestPlusMigrationSequenceKeepsPublished231Immutable(t *testing.T) {
	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(files)

	require.Contains(t, files, "231_repair_active_codex_fingerprint_seed.sql")
	require.Contains(t, files, "232_add_usage_log_requested_reasoning_effort.sql")
	require.Contains(t, files, "233_user_restrict_public_groups.sql")
	require.NotContains(t, files, "231_add_usage_log_requested_reasoning_effort.sql")
	require.NotContains(t, files, "231_user_restrict_public_groups.sql")

	indexOf := func(name string) int {
		for i, file := range files {
			if file == name {
				return i
			}
		}
		return -1
	}
	require.Less(t,
		indexOf("231_repair_active_codex_fingerprint_seed.sql"),
		indexOf("232_add_usage_log_requested_reasoning_effort.sql"),
	)
	require.Less(t,
		indexOf("232_add_usage_log_requested_reasoning_effort.sql"),
		indexOf("233_user_restrict_public_groups.sql"),
	)

	usageSQL, err := fs.ReadFile(migrations.FS, "232_add_usage_log_requested_reasoning_effort.sql")
	require.NoError(t, err)
	require.Contains(t, string(usageSQL), "ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS requested_reasoning_effort")
	userSQL, err := fs.ReadFile(migrations.FS, "233_user_restrict_public_groups.sql")
	require.NoError(t, err)
	require.Contains(t, string(userSQL), "ALTER TABLE users ADD COLUMN IF NOT EXISTS restrict_public_groups")
}

// TestApplyMigrationsFS_Plus231FollowupsExecuteInOrder exercises the actual
// runner (rather than only inspecting filenames) with the three migration
// shapes involved in the collision.  This catches accidental omissions from
// embed.FS and proves both fresh follow-ups are recorded after the published
// Plus migration.
func TestApplyMigrationsFS_Plus231FollowupsExecuteInOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	prepareMigrationsBootstrapExpectations(mock)
	files := []struct {
		name string
		stmt string
	}{
		{
			name: "231_repair_active_codex_fingerprint_seed.sql",
			stmt: "(?s)UPDATE accounts.*gen_random_uuid",
		},
		{
			name: "232_add_usage_log_requested_reasoning_effort.sql",
			stmt: "(?s)ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS requested_reasoning_effort VARCHAR\\(20\\)",
		},
		{
			name: "233_user_restrict_public_groups.sql",
			stmt: "(?s)ALTER TABLE users ADD COLUMN IF NOT EXISTS restrict_public_groups BOOLEAN NOT NULL DEFAULT false",
		},
	}
	fsys := fstest.MapFS{}
	for _, file := range files {
		content, readErr := fs.ReadFile(migrations.FS, file.name)
		require.NoError(t, readErr)
		fsys[file.name] = &fstest.MapFile{Data: content}
		mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").
			WithArgs(file.name).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectBegin()
		mock.ExpectExec(file.stmt).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO schema_migrations \\(filename, checksum\\) VALUES \\(\\$1, \\$2\\)").
			WithArgs(file.name, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
	}
	mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").
		WithArgs(migrationsAdvisoryLockID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, applyMigrationsFS(context.Background(), db, fsys))
	require.NoError(t, mock.ExpectationsWereMet())
}
