package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWeeklyResetTime_LegacyMidnightAnchor_UsesStartsAt(t *testing.T) {
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, time.FixedZone("UTC+8", 8*3600))
	windowStart := startOfDay(startsAt)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 30), WeeklyWindowStart: ptrTime(windowStart)}

	got := sub.WeeklyResetTime()
	require.NotNil(t, got)
	assert.True(t, startsAt.Add(7*24*time.Hour).Equal(*got))
}

func TestWeeklyResetTime_RegularAnchor_UsesWindowStart(t *testing.T) {
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, time.FixedZone("UTC+8", 8*3600))
	windowStart := startsAt.Add(7 * 24 * time.Hour)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 30), WeeklyWindowStart: ptrTime(windowStart)}

	got := sub.WeeklyResetTime()
	require.NotNil(t, got)
	assert.True(t, windowStart.Add(7*24*time.Hour).Equal(*got))
}

func TestWeeklyResetTime_MatchesAutomaticWindowStart(t *testing.T) {
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, time.FixedZone("UTC+8", 8*3600))
	windowStart := startOfDay(startsAt)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 30), WeeklyWindowStart: ptrTime(windowStart)}
	resetAt := *sub.WeeklyResetTime()

	_, ok := sub.automaticWindowStartAt(sub.WeeklyWindowStart, 7*24*time.Hour, resetAt.Add(-time.Second))
	assert.False(t, ok)
	newStart, ok := sub.automaticWindowStartAt(sub.WeeklyWindowStart, 7*24*time.Hour, resetAt)
	assert.True(t, ok)
	assert.True(t, resetAt.Equal(newStart))
}

func TestMonthlyResetTime_LegacyMidnightAnchor_UsesStartsAt(t *testing.T) {
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, time.FixedZone("UTC+8", 8*3600))
	windowStart := startOfDay(startsAt)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 60), MonthlyWindowStart: ptrTime(windowStart)}

	got := sub.MonthlyResetTime()
	require.NotNil(t, got)
	assert.True(t, startsAt.Add(30*24*time.Hour).Equal(*got))
}

func TestDailyResetTime_UnaffectedByWindowResetAnchor(t *testing.T) {
	base := timezone.StartOfDay(time.Date(2026, 7, 31, 12, 0, 0, 0, timezone.Location()))
	startsAt := base.Add(13*time.Hour + 37*time.Minute + 6*time.Second)
	sub := &UserSubscription{StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 30), DailyWindowStart: ptrTime(base)}

	got := sub.DailyResetTime()
	require.NotNil(t, got)
	assert.True(t, got.Equal(base.AddDate(0, 0, 1)))
	assert.False(t, got.Equal(startsAt.Add(24*time.Hour)))
}
