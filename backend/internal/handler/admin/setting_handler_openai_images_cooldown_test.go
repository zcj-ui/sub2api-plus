//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// openAIImageCooldownHandlerRepo reuses the broad settings stub used by the
// admin settings tests while making the single-key Get/Set operations real.
type openAIImageCooldownHandlerRepo struct {
	settingHandlerRepoStub
	getErr error
	setErr error
}

func (r *openAIImageCooldownHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	if r.values == nil {
		return "", nil
	}
	return r.values[key], nil
}

func (r *openAIImageCooldownHandlerRepo) Set(_ context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func newOpenAIImageCooldownHandler(t *testing.T, repo *openAIImageCooldownHandlerRepo) *SettingHandler {
	t.Helper()
	return NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
}

func TestSettingHandler_OpenAIImagesOAuthUnavailableCooldown_GetDefaultsAndReadsStoredValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &openAIImageCooldownHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}}}
	h := newOpenAIImageCooldownHandler(t, repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown", nil)
	h.GetOpenAIImagesOAuthUnavailableCooldownSettings(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var envelope struct {
		Data dto.OpenAIImagesOAuthUnavailableCooldownSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 30, envelope.Data.CooldownMinutes)

	repo.values[service.SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings] = `{"cooldown_minutes":9}`
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown", nil)
	h.GetOpenAIImagesOAuthUnavailableCooldownSettings(c)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 9, envelope.Data.CooldownMinutes)
}

func TestSettingHandler_OpenAIImagesOAuthUnavailableCooldown_PutValidAndRejectsOutOfRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &openAIImageCooldownHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}}}
	h := newOpenAIImageCooldownHandler(t, repo)

	body, err := json.Marshal(map[string]int{"cooldown_minutes": 11})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateOpenAIImagesOAuthUnavailableCooldownSettings(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var envelope struct {
		Data dto.OpenAIImagesOAuthUnavailableCooldownSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 11, envelope.Data.CooldownMinutes)
	require.JSONEq(t, `{"cooldown_minutes":11}`, repo.values[service.SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings])

	for _, minutes := range []int{0, 121} {
		body, err = json.Marshal(map[string]int{"cooldown_minutes": minutes})
		require.NoError(t, err)
		rec = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateOpenAIImagesOAuthUnavailableCooldownSettings(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, "minutes=%d", minutes)
	}
	require.JSONEq(t, `{"cooldown_minutes":11}`, repo.values[service.SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings])
}

func TestSettingHandler_OpenAIImagesOAuthUnavailableCooldown_GetReportsRepositoryFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &openAIImageCooldownHandlerRepo{
		settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}},
		getErr:                 errors.New("database unavailable"),
	}
	h := newOpenAIImageCooldownHandler(t, repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/openai-images-oauth-unavailable-cooldown", nil)
	h.GetOpenAIImagesOAuthUnavailableCooldownSettings(c)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var envelope response.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.NotEqual(t, 0, envelope.Code)
}
