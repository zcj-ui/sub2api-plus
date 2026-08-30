package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepositorySetErrorCapturesOnlyPreviouslySchedulableState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)

	mock.ExpectExec(`(?s)UPDATE accounts AS a.*_sub2api_error_restore_schedulable.*WHERE a\.id = \$3`).
		WithArgs(service.StatusError, "upstream failed", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.SetError(context.Background(), 42, "upstream failed"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepositoryClearErrorRestoresMarkerAndLeavesLegacyManualDisable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)

	mock.ExpectExec(`(?s)UPDATE accounts AS a.*schedulable = CASE.*_sub2api_error_restore_schedulable.*extra = .* - '_sub2api_error_restore_schedulable'.*WHERE a\.id = \$2`).
		WithArgs(service.StatusActive, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.ClearError(context.Background(), 42))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepositorySetSchedulableClearsPendingAutomaticRestore(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)

	mock.ExpectExec(`(?s)UPDATE accounts AS a.*schedulable = \$1.*extra = .* - '_sub2api_error_restore_schedulable'.*WHERE a\.id = \$2`).
		WithArgs(false, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.SetSchedulable(context.Background(), 42, false))
	require.NoError(t, mock.ExpectationsWereMet())
}
