package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// SetTLSFingerprintProfileService wires the account-profile resolver into the
// OpenAI gateway.  Keeping this setter avoids changing the public constructor
// (which is used by many unit fixtures) while still using the same resolver as
// the account test/usage paths.
func (s *OpenAIGatewayService) SetTLSFingerprintProfileService(svc *TLSFingerprintProfileService) {
	if s != nil {
		s.tlsFPProfileService = svc
	}
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	var explicitProfile *tlsfingerprint.Profile
	if s.tlsFPProfileService != nil {
		explicitProfile = s.tlsFPProfileService.ResolveTLSProfile(account)
	}
	// Keep the ordinary Do method for API-key/custom-compatible accounts.  Apart
	// from avoiding needless profile plumbing, this preserves the observable
	// transport contract for fakes and plugins; only an actual Codex profile
	// needs the DoWithTLS path.
	profile := resolveOpenAICodexTLSProfile(explicitProfile, account)
	if profile == nil {
		return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, profile)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		var explicitProfile *tlsfingerprint.Profile
		if s.tlsFPProfileService != nil {
			explicitProfile = s.tlsFPProfileService.ResolveTLSProfile(account)
		}
		profile := resolveOpenAICodexTLSProfile(explicitProfile, account)
		if profile == nil {
			return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
		}
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			profile,
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
