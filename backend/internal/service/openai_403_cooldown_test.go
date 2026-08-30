//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAI403TempRecorder struct {
	*rateLimitAccountRepoStub
	until time.Time
}

func (r *openAI403TempRecorder) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.until = until
	return r.rateLimitAccountRepoStub.SetTempUnschedulable(ctx, id, until, reason)
}

type openAI403WindowRecorder struct {
	*countingOpenAI403CounterCache
	windows []int
}

func (r *openAI403WindowRecorder) IncrementOpenAI403Count(ctx context.Context, accountID int64, window int) (int64, error) {
	r.windows = append(r.windows, window)
	return r.countingOpenAI403CounterCache.IncrementOpenAI403Count(ctx, accountID, window)
}

func setOpenAI403TestSettings(t *testing.T, h *openAI403TestHarness, settings OpenAI403CooldownSettings) {
	t.Helper()
	repo := newMockSettingRepo()
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	repo.data[SettingKeyOpenAI403CooldownSettings] = string(data)
	h.svc.SetSettingService(NewSettingService(repo, &config.Config{}))
}

func TestGetOpenAI403CooldownSettings_DefaultsWhenNotSet(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})

	settings, err := svc.GetOpenAI403CooldownSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, openAI403CooldownMinutesDefault, settings.CooldownMinutes)
	require.Equal(t, openAI403DisableThreshold, settings.DisableThreshold)
	require.Equal(t, openAI403CounterWindowMinutes, settings.WindowMinutes)
}

func TestGetOpenAI403CooldownSettings_NilRepositoryUsesDefaults(t *testing.T) {
	svc := NewSettingService(nil, &config.Config{})

	settings, err := svc.GetOpenAI403CooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultOpenAI403CooldownSettings(), settings)
}

func TestGetOpenAI403CooldownSettings_ClampsOutOfRange(t *testing.T) {
	repo := newMockSettingRepo()
	repo.data[SettingKeyOpenAI403CooldownSettings] = `{"enabled":true,"cooldown_minutes":99999,"disable_threshold":0,"window_minutes":-5}`
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetOpenAI403CooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, maxOpenAI403CooldownMinutes, settings.CooldownMinutes)
	require.Equal(t, 1, settings.DisableThreshold)
	require.Equal(t, 1, settings.WindowMinutes)
}

func TestSetOpenAI403CooldownSettings_RejectsOutOfRangeWhenEnabled(t *testing.T) {
	tests := []struct {
		name     string
		settings OpenAI403CooldownSettings
		field    string
	}{
		{name: "cooldown_minutes", settings: OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 0, DisableThreshold: 3, WindowMinutes: 180}, field: "cooldown_minutes"},
		{name: "disable_threshold", settings: OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 101, WindowMinutes: 180}, field: "disable_threshold"},
		{name: "window_minutes", settings: OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 3, WindowMinutes: 1441}, field: "window_minutes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := NewSettingService(newMockSettingRepo(), &config.Config{})
			err := svc.SetOpenAI403CooldownSettings(context.Background(), &test.settings)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.field)
		})
	}
}

func TestSetOpenAI403CooldownSettings_NormalizesWhenDisabled(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})
	settings := &OpenAI403CooldownSettings{Enabled: false, CooldownMinutes: 0, DisableThreshold: 101, WindowMinutes: -1}

	require.NoError(t, svc.SetOpenAI403CooldownSettings(context.Background(), settings))
	stored, err := svc.GetOpenAI403CooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	require.Equal(t, openAI403CooldownMinutesDefault, stored.CooldownMinutes)
	require.Equal(t, openAI403DisableThreshold, stored.DisableThreshold)
	require.Equal(t, openAI403CounterWindowMinutes, stored.WindowMinutes)
}

func TestHandleOpenAI403_UsesConfiguredCooldownMinutes(t *testing.T) {
	h := newOpenAI403TestHarness(t, 601, 1)
	setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 2, DisableThreshold: 3, WindowMinutes: 180})
	recorder := &openAI403TempRecorder{rateLimitAccountRepoStub: h.repo}
	h.svc.accountRepo = recorder
	before := time.Now()

	require.True(t, h.handle(`{"error":{"message":"temporary edge rejection"}}`))
	require.WithinDuration(t, before.Add(2*time.Minute), recorder.until, 5*time.Second)
	require.Less(t, recorder.until.Sub(before), 5*time.Minute)
}

func TestHandleOpenAI403_UsesConfiguredThreshold(t *testing.T) {
	t.Run("at_threshold_disables", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 602, 2)
		setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 2, WindowMinutes: 180})
		require.True(t, h.handle(`{"error":{"message":"workspace forbidden"}}`))
		require.Equal(t, 1, h.repo.setErrorCalls)
		require.Equal(t, 0, h.repo.tempCalls)
	})

	t.Run("below_threshold_is_temporary", func(t *testing.T) {
		h := newOpenAI403TestHarness(t, 603, 1)
		setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 2, WindowMinutes: 180})
		require.True(t, h.handle(`{"error":{"message":"temporary edge rejection"}}`))
		require.Equal(t, 0, h.repo.setErrorCalls)
		require.Equal(t, 1, h.repo.tempCalls)
	})
}

func TestHandleOpenAI403_PassesConfiguredWindowToCounter(t *testing.T) {
	h := newOpenAI403TestHarness(t, 604, 1)
	setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 3, WindowMinutes: 30})
	counter := &openAI403WindowRecorder{countingOpenAI403CounterCache: h.counter}
	h.svc.SetOpenAI403CounterCache(counter)

	require.True(t, h.handle(`{"error":{"message":"temporary edge rejection"}}`))
	require.Equal(t, []int{30}, counter.windows)
}

func TestHandleOpenAI403_DisabledSkipsAccountPenalty(t *testing.T) {
	h := newOpenAI403TestHarness(t, 605, 1)
	setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: false, CooldownMinutes: 10, DisableThreshold: 3, WindowMinutes: 180})

	require.False(t, h.handle(`{"error":{"message":"temporary edge rejection"}}`))
	h.requireNoAccountPenalty(t)
}

func TestHandleOpenAI403_FallsBackToDefaultsWithoutSettingService(t *testing.T) {
	h := newOpenAI403TestHarness(t, 606, 1)
	recorder := &openAI403TempRecorder{rateLimitAccountRepoStub: h.repo}
	h.svc.accountRepo = recorder
	counter := &openAI403WindowRecorder{countingOpenAI403CounterCache: h.counter}
	h.svc.SetOpenAI403CounterCache(counter)
	before := time.Now()

	require.True(t, h.handle(`{"error":{"message":"temporary edge rejection"}}`))
	require.WithinDuration(t, before.Add(10*time.Minute), recorder.until, 5*time.Second)
	require.Equal(t, []int{180}, counter.windows)
}

func TestHandleOpenAI403_HTMLBodyStillSkipsPenaltyWhenEnabled(t *testing.T) {
	h := newOpenAI403TestHarness(t, 607, 1)
	setOpenAI403TestSettings(t, h, OpenAI403CooldownSettings{Enabled: true, CooldownMinutes: 10, DisableThreshold: 3, WindowMinutes: 180})

	require.False(t, h.handle(openAI403HTMLBody))
	h.requireNoAccountPenalty(t)
}

func TestHandleCNProvider403_IgnoresOpenAI403Setting(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{1}}
	settingRepo := newMockSettingRepo()
	data, err := json.Marshal(OpenAI403CooldownSettings{Enabled: false, CooldownMinutes: 1, DisableThreshold: 1, WindowMinutes: 1})
	require.NoError(t, err)
	settingRepo.data[SettingKeyOpenAI403CooldownSettings] = string(data)
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403CounterCache(counter)
	svc.SetSettingService(NewSettingService(settingRepo, &config.Config{}))
	account := &Account{ID: 608, Platform: PlatformZhipu, Type: AccountTypeAPIKey}

	require.True(t, svc.HandleUpstreamError(context.Background(), account, 403, nil, []byte(`{"error":{"message":"forbidden"}}`)))
	// The shared CN-provider policy remains active; the OpenAI-only switch must
	// not turn it into the disabled/no-penalty path.
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
}
