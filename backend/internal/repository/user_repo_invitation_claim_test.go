//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestCreateWithEmailAliasGuardJoinsOuterTransaction verifies that user creation
// participates in the caller's transaction. The registration path relies on this
// to roll back an account when invitation-code claim loses a concurrent race.
func TestCreateWithEmailAliasGuardJoinsOuterTransaction(t *testing.T) {
	client := testEntClient(t)
	userRepo := NewUserRepository(client, integrationDB)
	redeemRepo := NewRedeemCodeRepository(client)
	ctx := context.Background()

	var committedEmails []string
	var codeIDs []int64
	t.Cleanup(func() {
		if len(committedEmails) > 0 {
			_, _ = client.User.Delete().Where(user.EmailIn(committedEmails...)).Exec(ctx)
		}
		if len(codeIDs) > 0 {
			_, _ = client.RedeemCode.Delete().Where(redeemcode.IDIn(codeIDs...)).Exec(ctx)
		}
	})

	seedCode := func(code string) int64 {
		created, err := client.RedeemCode.Create().
			SetCode(code).
			SetType(service.RedeemTypeInvitation).
			SetStatus(service.StatusUnused).
			SetValue(0).
			Save(ctx)
		require.NoError(t, err)
		codeIDs = append(codeIDs, created.ID)
		return created.ID
	}
	suffix := time.Now().UnixNano()

	t.Run("rollback", func(t *testing.T) {
		codeID := seedCode(fmt.Sprintf("R-%d", suffix))
		tx, err := client.Tx(ctx)
		require.NoError(t, err)
		txCtx := dbent.NewTxContext(ctx, tx)
		created := &service.User{
			Email:        fmt.Sprintf("tx-inv-rollback-%d@example.com", suffix),
			PasswordHash: "test-hash",
			Role:         service.RoleUser,
			Status:       service.StatusActive,
			Concurrency:  1,
		}
		require.NoError(t, userRepo.CreateWithEmailAliasGuard(txCtx, created))
		require.Greater(t, created.ID, int64(0))
		require.NoError(t, redeemRepo.Use(txCtx, codeID, created.ID))
		require.NoError(t, tx.Rollback())

		exists, err := userRepo.ExistsByEmail(ctx, created.Email)
		require.NoError(t, err)
		require.False(t, exists)
		code, err := client.RedeemCode.Get(ctx, codeID)
		require.NoError(t, err)
		require.Equal(t, service.StatusUnused, code.Status)
	})

	t.Run("commit", func(t *testing.T) {
		codeID := seedCode(fmt.Sprintf("C-%d", suffix))
		tx, err := client.Tx(ctx)
		require.NoError(t, err)
		txCtx := dbent.NewTxContext(ctx, tx)
		created := &service.User{
			Email:        fmt.Sprintf("tx-inv-commit-%d@example.com", suffix),
			PasswordHash: "test-hash",
			Role:         service.RoleUser,
			Status:       service.StatusActive,
			Concurrency:  1,
		}
		require.NoError(t, userRepo.CreateWithEmailAliasGuard(txCtx, created))
		require.Greater(t, created.ID, int64(0))
		require.NoError(t, redeemRepo.Use(txCtx, codeID, created.ID))
		require.NoError(t, tx.Commit())
		committedEmails = append(committedEmails, created.Email)

		exists, err := userRepo.ExistsByEmail(ctx, created.Email)
		require.NoError(t, err)
		require.True(t, exists)
		code, err := client.RedeemCode.Get(ctx, codeID)
		require.NoError(t, err)
		require.Equal(t, service.StatusUsed, code.Status)
	})
}
