package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// degradedGatewayServer replays the grok.com degradation: a burst of tool
// frames, response.persisted, and then nothing but heartbeats.
func fakeGatewayServer(t *testing.T, afterTurn func(*websocket.Conn)) *fhttptest.Server {
	t.Helper()
	return fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		var initial map[string]any
		if err := connection.ReadJSON(&initial); err != nil {
			return
		}
		initialID := initial["event"].(map[string]any)["event_id"].(string)
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": initialID}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
		var item, create map[string]any
		if connection.ReadJSON(&item) != nil || connection.ReadJSON(&create) != nil {
			return
		}
		afterTurn(connection)
	}))
}

func writeDegradedFrames(connection *websocket.Conn) {
	for i := 0; i < 8; i++ {
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"tool_usage_card": map[string]any{"tool_usage_card_id": "c" + string(rune('0'+i)), "web_search": map[string]any{"args": map[string]any{"query": "q"}}}}}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"tool_result": map[string]any{"tool_usage_card_id": "c" + string(rune('0'+i)), "web_search": map[string]any{"webpages": []any{}}}}}})
	}
	_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.title.updated"}})
	_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.persisted", "status": "ok"}})
	// Upstream keeps the socket open (heartbeats only) forever.
	time.Sleep(10 * time.Second)
}

func newTestWebAdapter(t *testing.T, baseURL string) (*Adapter, account.Credential) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: baseURL, ChatTimeoutSeconds: 60}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	return adapter, account.Credential{ID: 1, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted}
}

func TestDegradedSearchOnlyTurnFailsFastForAccountRotation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		server := fakeGatewayServer(t, writeDegradedFrames)
		adapter, credential := newTestWebAdapter(t, server.URL)
		body, _ := json.Marshal(map[string]any{"model": "grok-chat-fast", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "今天上海天气"}}})
		started := time.Now()
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: credential, Method: http.MethodPost, Path: "/chat/completions", Body: body, Model: "grok-chat-fast", Operation: "chat", Streaming: stream,
		})
		if response != nil {
			response.Body.Close()
		}
		if err == nil || !neterrorpkg.IsUpstreamResponseEmpty(err) || !errors.Is(err, errGatewayDegraded) {
			t.Fatalf("stream=%v: want degraded empty error, got response=%v err=%v", stream, response, err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("stream=%v: degraded turn took %s, want fast failure", stream, elapsed)
		}
		server.Close()
	}
}

func TestPersistedAfterTextFinishesWithoutResponseDone(t *testing.T) {
	server := fakeGatewayServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "晴，22度", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.persisted", "status": "ok"}})
		time.Sleep(15 * time.Second)
	})
	defer server.Close()
	adapter, credential := newTestWebAdapter(t, server.URL)
	body, _ := json.Marshal(map[string]any{"model": "grok-chat-fast", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	started := time.Now()
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/chat/completions", Body: body, Model: "grok-chat-fast", Operation: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, _ := io.ReadAll(response.Body)
	if !bytes.Contains(result, []byte("晴，22度")) {
		t.Fatalf("body=%s", result)
	}
	if elapsed := time.Since(started); elapsed > gatewayPersistedGrace+3*time.Second {
		t.Fatalf("took %s", elapsed)
	}
}

func TestGoodTurnStillStreamsAfterToolFrames(t *testing.T) {
	server := fakeGatewayServer(t, func(connection *websocket.Conn) {
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "Thinking about weather", "channel": "CHANNEL_ASSISTANT_NOTETAKER_HEADER"}}}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"tool_usage_card": map[string]any{"tool_usage_card_id": "c1", "web_search": map[string]any{"args": map[string]any{"query": "q"}}}}}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "OK", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_1", "status": "completed"}}})
	})
	defer server.Close()
	adapter, credential := newTestWebAdapter(t, server.URL)
	body, _ := json.Marshal(map[string]any{"model": "grok-chat-fast", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/chat/completions", Body: body, Model: "grok-chat-fast", Operation: "chat", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(result, []byte(`"OK"`)) || !bytes.Contains(result, []byte("[DONE]")) {
		t.Fatalf("status=%d body=%s", response.StatusCode, result)
	}
}
