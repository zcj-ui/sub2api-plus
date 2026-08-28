//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImagesOAuthUnavailableCooldownSettings_DefaultAndStoredValue(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, openAIImagesOAuthUnavailableDefaultCooldownMinutes, settings.CooldownMinutes)

	repo.data[SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings] = `{"cooldown_minutes":7}`
	settings, err = svc.GetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 7, settings.CooldownMinutes)
}

func TestOpenAIImagesOAuthUnavailableCooldownSettings_InvalidValuesFallBackToDefault(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	for _, raw := range []string{
		`{"cooldown_minutes":0}`,
		`{"cooldown_minutes":-1}`,
		`{"cooldown_minutes":121}`,
		`{"cooldown_minutes":9223372036854775808}`,
		"not-json",
	} {
		repo.data[SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings] = raw
		settings, err := svc.GetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background())
		require.NoError(t, err)
		require.Equal(t, openAIImagesOAuthUnavailableDefaultCooldownMinutes, settings.CooldownMinutes, "raw=%q", raw)
	}
}

func TestOpenAIImagesOAuthUnavailableCooldownSettings_SetEnforcesStrictRange(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	for _, minutes := range []int{1, 30, openAIImagesOAuthUnavailableMaxCooldownMinutes} {
		require.NoError(t, svc.SetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background(), &OpenAIImagesOAuthUnavailableCooldownSettings{
			CooldownMinutes: minutes,
		}))
	}
	for _, minutes := range []int{0, -1, openAIImagesOAuthUnavailableMaxCooldownMinutes + 1} {
		err := svc.SetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background(), &OpenAIImagesOAuthUnavailableCooldownSettings{
			CooldownMinutes: minutes,
		})
		require.ErrorContains(t, err, "cooldown_minutes must be between 1-120")
	}
	require.Error(t, svc.SetOpenAIImagesOAuthUnavailableCooldownSettings(context.Background(), nil))

	var persisted OpenAIImagesOAuthUnavailableCooldownSettings
	require.NoError(t, json.Unmarshal([]byte(repo.data[SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings]), &persisted))
	require.Equal(t, openAIImagesOAuthUnavailableMaxCooldownMinutes, persisted.CooldownMinutes)
}

func TestOpenAIGatewayService_CoolOpenAIImagesOAuthToolUsesConfiguredCooldown(t *testing.T) {
	accountRepo := &modelNotFoundAccountRepoStub{}
	settingRepo := newMockSettingRepo()
	settingRepo.data[SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings] = `{"cooldown_minutes":7}`
	svc := &OpenAIGatewayService{
		accountRepo:    accountRepo,
		settingService: NewSettingService(settingRepo, &config.Config{}),
	}

	before := time.Now()
	svc.coolOpenAIImagesOAuthTool(context.Background(), &Account{ID: 206, Platform: PlatformOpenAI, Type: AccountTypeOAuth})

	require.Len(t, accountRepo.modelRateLimitCalls, 1)
	require.Equal(t, openAIImageGenerationRateLimitKey, accountRepo.modelRateLimitCalls[0].scope)
	require.Equal(t, openAIImagesOAuthUnavailableReason, accountRepo.modelRateLimitCalls[0].reason)
	require.WithinDuration(t, before.Add(7*time.Minute), accountRepo.modelRateLimitCalls[0].resetAt, time.Second)
}

func TestOpenAIGatewayService_CoolOpenAIImagesOAuthToolReadFailureUsesDefault(t *testing.T) {
	accountRepo := &modelNotFoundAccountRepoStub{}
	settingRepo := newMockSettingRepo()
	settingRepo.getValueErr = errors.New("settings store unavailable")
	svc := &OpenAIGatewayService{
		accountRepo:    accountRepo,
		settingService: NewSettingService(settingRepo, &config.Config{}),
	}

	before := time.Now()
	svc.coolOpenAIImagesOAuthTool(context.Background(), &Account{ID: 207, Platform: PlatformOpenAI, Type: AccountTypeOAuth})

	require.Len(t, accountRepo.modelRateLimitCalls, 1)
	require.WithinDuration(t, before.Add(openAIImagesOAuthUnavailableDefaultCooldown), accountRepo.modelRateLimitCalls[0].resetAt, time.Second)
}

// blockingImageCooldownSettingRepo intentionally ignores context cancellation
// to model a broken custom repository.  The gateway helper must still return
// after its short read budget and apply the default cooldown.
type blockingImageCooldownSettingRepo struct {
	mockSettingRepo
	started chan struct{}
	release chan struct{}
}

func (r *blockingImageCooldownSettingRepo) GetValue(context.Context, string) (string, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.release
	return "", errors.New("settings read interrupted")
}

func TestOpenAIGatewayService_CoolOpenAIImagesOAuthToolDoesNotWaitForStuckSettingsRepo(t *testing.T) {
	accountRepo := &modelNotFoundAccountRepoStub{}
	settingRepo := &blockingImageCooldownSettingRepo{
		mockSettingRepo: *newMockSettingRepo(),
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	svc := &OpenAIGatewayService{
		accountRepo:    accountRepo,
		settingService: NewSettingService(settingRepo, &config.Config{}),
	}

	startedAt := time.Now()
	svc.coolOpenAIImagesOAuthTool(context.Background(), &Account{ID: 208, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	elapsed := time.Since(startedAt)
	close(settingRepo.release)

	require.Less(t, elapsed, 2*time.Second)
	require.Len(t, accountRepo.modelRateLimitCalls, 1)
	require.WithinDuration(t, startedAt.Add(openAIImagesOAuthUnavailableDefaultCooldown), accountRepo.modelRateLimitCalls[0].resetAt, time.Second)
}
