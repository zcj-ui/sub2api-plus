//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updatingProxyRepoStub struct {
	*proxyRepoStub
	proxy             *Proxy
	updateCalls       int
	accountSummaries  []ProxyAccountSummary
	accountSummaryErr error
}

func (s *updatingProxyRepoStub) GetByID(context.Context, int64) (*Proxy, error) {
	copy := *s.proxy
	return &copy, nil
}

func (s *updatingProxyRepoStub) Update(_ context.Context, proxy *Proxy) error {
	s.updateCalls++
	copy := *proxy
	s.proxy = &copy
	return nil
}

func (s *updatingProxyRepoStub) ListAccountSummariesByProxyID(_ context.Context, _ int64) ([]ProxyAccountSummary, error) {
	if s.accountSummaryErr != nil {
		return nil, s.accountSummaryErr
	}
	return append([]ProxyAccountSummary(nil), s.accountSummaries...), nil
}

type openAIWSInvalidatorSpy struct {
	accountIDs []int64
}

func (s *openAIWSInvalidatorSpy) InvalidateOpenAIWSConnections(accountID int64) {
	s.accountIDs = append(s.accountIDs, accountID)
}

type runtimeBlockerWithOpenAIWSInvalidator struct {
	openAIWSInvalidatorSpy
}

func (runtimeBlockerWithOpenAIWSInvalidator) BlockAccountScheduling(*Account, time.Time, string) {}

func (runtimeBlockerWithOpenAIWSInvalidator) ClearAccountSchedulingBlock(int64) {}

func TestProxyServiceUpdateInvalidatesDeduplicatedAccountConnections(t *testing.T) {
	repo := &updatingProxyRepoStub{
		proxyRepoStub: &proxyRepoStub{},
		proxy:         &Proxy{ID: 9, Protocol: "http", Host: "old.example", Port: 8080, Status: StatusActive},
		accountSummaries: []ProxyAccountSummary{
			{ID: 41, Platform: PlatformOpenAI},
			{ID: 41, Platform: PlatformOpenAI},
			{ID: 0, Platform: PlatformOpenAI},
			{ID: 52, Platform: PlatformOpenAI},
			{ID: 63, Platform: PlatformAnthropic},
		},
	}
	invalidator := &openAIWSInvalidatorSpy{}
	svc := NewProxyService(repo, invalidator)
	host := "new.example"

	_, err := svc.Update(context.Background(), 9, UpdateProxyRequest{Host: &host})

	require.NoError(t, err)
	require.Equal(t, []int64{41, 52}, invalidator.accountIDs)
}

func TestProxyServiceDeleteInvalidatesConnectionsAfterSuccessfulDelete(t *testing.T) {
	repo := &updatingProxyRepoStub{
		proxyRepoStub: &proxyRepoStub{},
		proxy:         &Proxy{ID: 9, Protocol: "http", Host: "proxy.example", Port: 8080, Status: StatusActive},
		accountSummaries: []ProxyAccountSummary{
			{ID: 61, Platform: PlatformOpenAI},
			{ID: 61, Platform: PlatformOpenAI},
		},
	}
	invalidator := &openAIWSInvalidatorSpy{}
	svc := NewProxyService(repo, invalidator)

	err := svc.Delete(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, []int64{9}, repo.deletedIDs)
	require.Equal(t, []int64{61}, invalidator.accountIDs)
}

func TestAdminProxyMutationsInvalidateOpenAIWSConnections(t *testing.T) {
	repo := &updatingProxyRepoStub{
		proxyRepoStub: &proxyRepoStub{},
		proxy:         &Proxy{ID: 9, Protocol: "http", Host: "proxy.example", Port: 8080, Status: StatusActive},
		accountSummaries: []ProxyAccountSummary{
			{ID: 73, Platform: PlatformOpenAI},
		},
	}
	runtime := &runtimeBlockerWithOpenAIWSInvalidator{}
	svc := &adminServiceImpl{proxyRepo: repo, runtimeBlocker: runtime}

	_, err := svc.UpdateProxy(context.Background(), 9, &UpdateProxyInput{
		Host:           "new.example",
		FallbackMode:   FallbackModeNone,
		ExpiryWarnDays: 7,
	})
	require.NoError(t, err)
	require.Equal(t, []int64{73}, runtime.accountIDs)

	runtime.accountIDs = nil
	err = svc.DeleteProxy(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, []int64{73}, runtime.accountIDs)

	runtime.accountIDs = nil
	result, err := svc.BatchDeleteProxies(context.Background(), []int64{10, 11})
	require.NoError(t, err)
	require.Equal(t, []int64{10, 11}, result.DeletedIDs)
	require.Equal(t, []int64{73, 73}, runtime.accountIDs)
}

func TestBothProxyUpdateServicesUseRepositoryUpdateBoundary(t *testing.T) {
	t.Run("ProxyService", func(t *testing.T) {
		repo := &updatingProxyRepoStub{
			proxyRepoStub: &proxyRepoStub{},
			proxy:         &Proxy{ID: 9, Protocol: "http", Host: "old.example", Port: 8080, Status: StatusActive},
		}
		svc := NewProxyService(repo)
		host := "new.example"

		_, err := svc.Update(context.Background(), 9, UpdateProxyRequest{Host: &host})

		require.NoError(t, err)
		require.Equal(t, 1, repo.updateCalls)
		require.Equal(t, host, repo.proxy.Host)
	})

	t.Run("adminService", func(t *testing.T) {
		repo := &updatingProxyRepoStub{
			proxyRepoStub: &proxyRepoStub{},
			proxy: &Proxy{
				ID:             9,
				Protocol:       "http",
				Host:           "old.example",
				Port:           8080,
				Status:         StatusActive,
				FallbackMode:   FallbackModeNone,
				ExpiryWarnDays: 7,
			},
		}
		svc := &adminServiceImpl{proxyRepo: repo}

		_, err := svc.UpdateProxy(context.Background(), 9, &UpdateProxyInput{
			Host:           "new.example",
			FallbackMode:   FallbackModeNone,
			ExpiryWarnDays: 7,
		})

		require.NoError(t, err)
		require.Equal(t, 1, repo.updateCalls)
		require.Equal(t, "new.example", repo.proxy.Host)
	})
}

func TestAdminProxyServiceRejectsInvalidFallbackMode(t *testing.T) {
	svc := &adminServiceImpl{}

	_, err := svc.CreateProxy(context.Background(), &CreateProxyInput{FallbackMode: "random"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback_mode")

	_, err = svc.UpdateProxy(context.Background(), 9, &UpdateProxyInput{FallbackMode: "random"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fallback_mode")
}

func TestNormalizeProxyFallbackModeAcceptsDocumentedModes(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "", want: FallbackModeNone},
		{input: " none ", want: FallbackModeNone},
		{input: FallbackModeProxy, want: FallbackModeProxy},
		{input: FallbackModeDirect, want: FallbackModeDirect},
	} {
		got, err := normalizeProxyFallbackMode(test.input)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
}
