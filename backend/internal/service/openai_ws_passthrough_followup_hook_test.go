package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPassthroughIngressReacquiresFollowupTurnHookAfterAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)

	var hooksMu sync.Mutex
	var order []string
	hooks := &OpenAIWSIngressHooks{
		BeforeRequest: func(turn int, _ []byte, _ string) error {
			hooksMu.Lock()
			order = append(order, fmt.Sprintf("before_request:%d", turn))
			hooksMu.Unlock()
			return nil
		},
		BeforePassthroughTurn: func(turn int) error {
			hooksMu.Lock()
			order = append(order, fmt.Sprintf("before_passthrough:%d", turn))
			hooksMu.Unlock()
			return nil
		},
		AfterTurn: func(turn int, _ *OpenAIForwardResult, turnErr error) {
			hooksMu.Lock()
			if turnErr == nil {
				order = append(order, fmt.Sprintf("after:%d", turn))
			} else {
				order = append(order, fmt.Sprintf("after_error:%d", turn))
			}
			hooksMu.Unlock()
		},
	}

	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, firstMessage, err := ReadOpenAIWSClientMessage(controlCtx, conn, 3*time.Second, coderws.StatusPolicyViolation, "missing first response.create message")
		if err != nil {
			serverErr <- err
			return
		}
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		req := r.Clone(controlCtx)
		req.Header = req.Header.Clone()
		ginCtx.Request = req
		serverErr <- newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream).ProxyResponsesWebSocketFromClient(controlCtx, ginCtx, conn, passthroughLifecycleAccount(), "sk-test", firstMessage, hooks)
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, firstEvent, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(firstEvent, "type").String())
	require.Equal(t, "response.create", gjson.GetBytes(<-upstream.writes, "type").String())

	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)
	secondRequest := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "response.create", gjson.GetBytes(secondRequest, "type").String())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":2}}}`)

	readCtx, cancelRead = context.WithTimeout(context.Background(), 3*time.Second)
	_, secondEvent, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(secondEvent, "type").String())
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, _ = clientConn.Read(idleCtx)
	cancelIdle()
	select {
	case <-serverErr:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough ingress did not exit")
	}

	hooksMu.Lock()
	gotOrder := append([]string(nil), order...)
	hooksMu.Unlock()
	require.Contains(t, gotOrder, "before_request:2")
	require.Contains(t, gotOrder, "before_passthrough:2")
	require.Contains(t, gotOrder, "after:1")
	require.Contains(t, gotOrder, "after:2")
	beforeRequestIndex, beforePassthroughIndex := -1, -1
	for i, entry := range gotOrder {
		if entry == "before_request:2" {
			beforeRequestIndex = i
		}
		if entry == "before_passthrough:2" {
			beforePassthroughIndex = i
		}
	}
	require.Greater(t, beforePassthroughIndex, beforeRequestIndex)
}
