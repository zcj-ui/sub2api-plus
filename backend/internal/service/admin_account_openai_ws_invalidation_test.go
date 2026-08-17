//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func newOpenAICredentialFamilyInvalidationFixture(t *testing.T) (*adminServiceImpl, *sparkShadowRepoStub, *Account, *Account, *runtimeBlockerWithOpenAIWSInvalidator) {
	t.Helper()

	ctx := context.Background()
	repo := newSparkShadowRepoStub()
	parentProxyID := int64(11)
	parent := &Account{
		Name:        "parent",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		ProxyID:     &parentProxyID,
		Credentials: map[string]any{"access_token": "parent-token"},
	}
	require.NoError(t, repo.Create(ctx, parent))

	seed := &adminServiceImpl{accountRepo: repo}
	shadow, err := seed.CreateShadow(ctx, parent.ID, ShadowOptions{Name: "shadow"})
	require.NoError(t, err)

	runtime := &runtimeBlockerWithOpenAIWSInvalidator{}
	return &adminServiceImpl{accountRepo: repo, runtimeBlocker: runtime}, repo, parent, shadow, runtime
}

func TestAdminAccountCredentialFamilyInvalidatesOpenAIWSConnections(t *testing.T) {
	t.Run("single proxy update", func(t *testing.T) {
		svc, _, parent, shadow, runtime := newOpenAICredentialFamilyInvalidationFixture(t)
		proxyID := int64(12)

		_, err := svc.UpdateAccount(context.Background(), parent.ID, &UpdateAccountInput{ProxyID: &proxyID})

		require.NoError(t, err)
		require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
	})

	t.Run("single credential update", func(t *testing.T) {
		svc, _, parent, shadow, runtime := newOpenAICredentialFamilyInvalidationFixture(t)

		_, err := svc.UpdateAccount(context.Background(), parent.ID, &UpdateAccountInput{
			Credentials: map[string]any{"access_token": "rotated-token"},
		})

		require.NoError(t, err)
		require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
	})

	t.Run("bulk credential update", func(t *testing.T) {
		svc, _, parent, shadow, runtime := newOpenAICredentialFamilyInvalidationFixture(t)

		_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs:  []int64{parent.ID},
			Credentials: map[string]any{"access_token": "rotated-token"},
		})

		require.NoError(t, err)
		require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
	})

	t.Run("proxy fallback revert", func(t *testing.T) {
		svc, _, parent, shadow, runtime := newOpenAICredentialFamilyInvalidationFixture(t)

		err := svc.RevertAccountProxyFallback(context.Background(), parent.ID)

		require.NoError(t, err)
		require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
	})

	t.Run("cascade deletion", func(t *testing.T) {
		svc, _, parent, shadow, runtime := newOpenAICredentialFamilyInvalidationFixture(t)

		err := svc.DeleteAccount(context.Background(), parent.ID)

		require.NoError(t, err)
		require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
	})
}

type panicShadowLookupRepo struct {
	*sparkShadowRepoStub
}

func (r *panicShadowLookupRepo) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	panic("optional shadow lookup is unavailable")
}

func TestAdminAccountCredentialFamilyInvalidationDoesNotBreakUpdateWhenShadowLookupPanics(t *testing.T) {
	ctx := context.Background()
	base := newSparkShadowRepoStub()
	repo := &panicShadowLookupRepo{sparkShadowRepoStub: base}
	parent := &Account{
		Name:        "parent",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "parent-token"},
	}
	require.NoError(t, repo.Create(ctx, parent))

	runtime := &runtimeBlockerWithOpenAIWSInvalidator{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: runtime}
	_, err := svc.UpdateAccount(ctx, parent.ID, &UpdateAccountInput{
		Credentials: map[string]any{"access_token": "rotated-token"},
	})

	require.NoError(t, err)
	require.Equal(t, []int64{parent.ID}, runtime.accountIDs)
}

type shadowProxyUpdateFailRepo struct {
	*sparkShadowRepoStub
}

func (r *shadowProxyUpdateFailRepo) Update(ctx context.Context, account *Account) error {
	if account != nil && account.IsCredentialShadow() {
		return errors.New("shadow proxy update failed")
	}
	return r.sparkShadowRepoStub.Update(ctx, account)
}

func TestUpdateAccountInvalidatesCredentialFamilyWhenShadowProxyPropagationFails(t *testing.T) {
	ctx := context.Background()
	base := newSparkShadowRepoStub()
	repo := &shadowProxyUpdateFailRepo{sparkShadowRepoStub: base}
	parentProxyID := int64(11)
	parent := &Account{
		Name:        "parent",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		ProxyID:     &parentProxyID,
		Credentials: map[string]any{"access_token": "parent-token"},
	}
	require.NoError(t, repo.Create(ctx, parent))
	seed := &adminServiceImpl{accountRepo: repo}
	shadow, err := seed.CreateShadow(ctx, parent.ID, ShadowOptions{Name: "shadow"})
	require.NoError(t, err)

	runtime := &runtimeBlockerWithOpenAIWSInvalidator{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: runtime}
	nextProxyID := int64(12)
	_, err = svc.UpdateAccount(ctx, parent.ID, &UpdateAccountInput{ProxyID: &nextProxyID})

	require.Error(t, err)
	require.Equal(t, []int64{parent.ID, shadow.ID}, runtime.accountIDs)
}
