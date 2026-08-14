package service

import (
	"net/url"
	"strconv"
	"testing"
)

func openAITestAccountWithProxy(account *Account) *Account {
	if account == nil {
		return nil
	}
	proxyID := int64(8000 + account.ID)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{
		ID:       proxyID,
		Protocol: "http",
		Host:     "127.0.0.1",
		Port:     1080,
	}
	return account
}

func openAITestAccountWithProxyForURL(account *Account, rawURL string) *Account {
	openAITestAccountWithProxy(account)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || parsed.Port() == "" {
		return account
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return account
	}
	account.Proxy.Protocol = parsed.Scheme
	account.Proxy.Host = parsed.Hostname()
	account.Proxy.Port = port
	return account
}

func bindOpenAITestProxyToServer(t testing.TB, account *Account, rawURL string) {
	t.Helper()
	if account == nil {
		t.Fatal("OpenAI test account is nil")
	}
	openAITestAccountWithProxyForURL(account, rawURL)
	if account.Proxy == nil || account.Proxy.Port <= 0 {
		t.Fatalf("parse OpenAI test proxy URL: %s", rawURL)
	}
}
