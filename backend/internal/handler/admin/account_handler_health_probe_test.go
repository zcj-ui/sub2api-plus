package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type healthProbeHandlerRepo struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts map[int64]*service.Account
	updates  map[int64]map[string]any
}

func (r *healthProbeHandlerRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return nil, service.ErrAccountNotFound
	}
	copy := *account
	return &copy, nil
}

func (r *healthProbeHandlerRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = make(map[int64]map[string]any)
	}
	r.updates[id] = updates
	return nil
}

type healthProbeHandlerQuota struct {
	mu    sync.Mutex
	calls map[int64]int
}

func (q *healthProbeHandlerQuota) QueryUsageForHealth(ctx context.Context, accountID int64) (*service.OpenAIQuotaUsage, error) {
	q.mu.Lock()
	q.calls[accountID]++
	q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID == 11 {
		time.Sleep(20 * time.Millisecond)
		return &service.OpenAIQuotaUsage{
			Credits:               &service.OpenAICodexCredits{HasCredits: true, Balance: "125.50"},
			RateLimitResetCredits: &service.OpenAIRateLimitResetCredits{AvailableCount: 2},
			FetchedAt:             123456,
		}, nil
	}
	return nil, errors.New("reset-credit endpoint returned 401")
}

func setupAccountHealthProbeRouter(repo service.AccountRepository, quota *healthProbeHandlerQuota) *gin.Engine {
	gin.SetMode(gin.TestMode)
	testService := service.NewAccountTestService(repo, nil, nil, nil, nil, nil, nil, nil)
	testService.SetOpenAIQuotaService(quota)
	handler := NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, testService, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/accounts/batch-health-probe", handler.BatchHealthProbe)
	router.POST("/api/v1/admin/accounts/batch-inventory", handler.BatchInventory)
	return router
}

func TestAccountHandlerBatchHealthProbeKeepsOrderAndAggregatesResults(t *testing.T) {
	repo := &healthProbeHandlerRepo{accounts: map[int64]*service.Account{
		11: {ID: 11, Name: "healthy", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		12: {ID: 12, Name: "dead", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		13: {ID: 13, Name: "claude", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
	}}
	quota := &healthProbeHandlerQuota{calls: make(map[int64]int)}
	router := setupAccountHealthProbeRouter(repo, quota)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-health-probe", bytes.NewBufferString(`{"account_ids":[11,12,13]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data BatchHealthProbeResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 1, envelope.Data.Healthy)
	require.Equal(t, 1, envelope.Data.Failed)
	require.Equal(t, 1, envelope.Data.Skipped)
	require.Len(t, envelope.Data.Results, 3)
	require.Equal(t, []int64{11, 12, 13}, []int64{
		envelope.Data.Results[0].AccountID,
		envelope.Data.Results[1].AccountID,
		envelope.Data.Results[2].AccountID,
	})
	require.True(t, envelope.Data.Results[0].Healthy)
	require.True(t, envelope.Data.Results[1].Dead)
	require.Contains(t, envelope.Data.Results[1].Reason, "reset-credit endpoint returned 401")
	require.False(t, envelope.Data.Results[2].Dead)
	require.Equal(t, 1, quota.calls[11])
	require.Equal(t, 2, quota.calls[12])
	require.Contains(t, repo.updates, int64(11))
	require.Contains(t, repo.updates, int64(12))
	require.NotContains(t, repo.updates, int64(13))
}

func TestAccountHandlerBatchHealthProbeValidatesAccountIDs(t *testing.T) {
	router := setupAccountHealthProbeRouter(&healthProbeHandlerRepo{}, &healthProbeHandlerQuota{})
	tooManyIDs := make([]int64, 201)
	for index := range tooManyIDs {
		tooManyIDs[index] = int64(index + 1)
	}
	tooManyBody, err := json.Marshal(BatchHealthProbeRequest{AccountIDs: tooManyIDs})
	require.NoError(t, err)

	for _, endpoint := range []string{
		"/api/v1/admin/accounts/batch-health-probe",
		"/api/v1/admin/accounts/batch-inventory",
	} {
		for _, body := range []string{
			`{"account_ids":[]}`,
			`{"account_ids":[0]}`,
			`{"account_ids":[-1]}`,
			`{"account_ids":[11,11]}`,
			string(tooManyBody),
		} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", endpoint, body)
		}
	}
}

func TestAccountHandlerBatchInventoryKeepsOrderReturnsQuotaAndAggregates(t *testing.T) {
	repo := &healthProbeHandlerRepo{accounts: map[int64]*service.Account{
		11: {ID: 11, Name: "oauth quota", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		12: {ID: 12, Name: "dead oauth", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		13: {ID: 13, Name: "claude", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
	}}
	quota := &healthProbeHandlerQuota{calls: make(map[int64]int)}
	router := setupAccountHealthProbeRouter(repo, quota)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-inventory", bytes.NewBufferString(`{"account_ids":[12,13,11]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data BatchInventoryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 1, envelope.Data.Healthy)
	require.Equal(t, 1, envelope.Data.Failed)
	require.Equal(t, 1, envelope.Data.Skipped)
	require.Equal(t, 1, envelope.Data.QuotaFetched)
	require.Len(t, envelope.Data.Results, 3)
	require.Equal(t, []int64{12, 13, 11}, []int64{
		envelope.Data.Results[0].AccountID,
		envelope.Data.Results[1].AccountID,
		envelope.Data.Results[2].AccountID,
	})
	require.True(t, envelope.Data.Results[0].Dead)
	require.Nil(t, envelope.Data.Results[0].Quota)
	require.False(t, envelope.Data.Results[1].Dead)
	require.Nil(t, envelope.Data.Results[1].Quota)
	require.True(t, envelope.Data.Results[2].Healthy)
	require.NotNil(t, envelope.Data.Results[2].Quota)
	require.Equal(t, "125.50", envelope.Data.Results[2].Quota.Credits.Balance)
	require.Equal(t, 2, envelope.Data.Results[2].Quota.RateLimitResetCredits.AvailableCount)
	require.Equal(t, 1, quota.calls[11])
	require.Equal(t, 2, quota.calls[12])
}

func TestAccountHandlerBatchInventoryHonorsCanceledContext(t *testing.T) {
	repo := &healthProbeHandlerRepo{accounts: map[int64]*service.Account{
		11: {ID: 11, Name: "oauth quota", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
	}}
	router := setupAccountHealthProbeRouter(repo, &healthProbeHandlerQuota{calls: make(map[int64]int)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-inventory", bytes.NewBufferString(`{"account_ids":[11]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data BatchInventoryResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Data.Healthy)
	require.Equal(t, 0, envelope.Data.Failed)
	require.Equal(t, 1, envelope.Data.Skipped)
	require.Equal(t, int64(11), envelope.Data.Results[0].AccountID)
	require.Contains(t, envelope.Data.Results[0].Reason, "context canceled")
}
