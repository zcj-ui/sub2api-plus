package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPassthroughPreludeFramesForwardBeforeFirstResponseCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_prelude","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	itemPayload := []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"staged before first turn"}]}}`)
	firstResponse := []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`)

	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		msgType, prelude, err := ReadOpenAIWSClientMessage(controlCtx, conn, 3*time.Second, coderws.StatusPolicyViolation, "missing conversation prelude")
		if err != nil {
			serverErr <- err
			return
		}
		if msgType != coderws.MessageText || gjson.GetBytes(prelude, "type").String() != "conversation.item.create" {
			serverErr <- errors.New("unexpected prelude frame")
			return
		}
		msgType, response, err := ReadOpenAIWSClientMessage(controlCtx, conn, 3*time.Second, coderws.StatusPolicyViolation, "missing first response.create")
		if err != nil {
			serverErr <- err
			return
		}
		if msgType != coderws.MessageBinary || gjson.GetBytes(response, "type").String() != "response.create" {
			serverErr <- errors.New("unexpected first response frame")
			return
		}
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		req := r.Clone(controlCtx)
		req.Header = req.Header.Clone()
		ginCtx.Request = req
		hooks := &OpenAIWSIngressHooks{
			InitialPassthroughFrames:   []OpenAIWSPassthroughInitialFrame{{MessageType: coderws.MessageText, Payload: prelude}},
			InitialResponseMessageType: msgType,
		}
		serverErr <- newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream).ProxyResponsesWebSocketFromClient(controlCtx, ginCtx, conn, passthroughLifecycleAccount(), "sk-test", response, hooks)
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, itemPayload))
	cancelWrite()
	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageBinary, firstResponse))
	cancelWrite()

	firstUpstream := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "conversation.item.create", gjson.GetBytes(firstUpstream, "type").String())
	require.Equal(t, coderws.MessageText, <-upstream.writeTypes)
	secondUpstream := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "response.create", gjson.GetBytes(secondUpstream, "type").String())
	require.Equal(t, coderws.MessageBinary, <-upstream.writeTypes)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, event, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())

	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough prelude relay did not exit")
	}
}
