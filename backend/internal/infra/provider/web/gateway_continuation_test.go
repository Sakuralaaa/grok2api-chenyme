package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

// A continuation (load_existing) replays the parent turn after
// conversation.attached. Those frames must not be returned as the new answer.
func TestGatewayContinuationSkipsReplayedParentTurn(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		var initial map[string]any
		if connection.ReadJSON(&initial) != nil {
			return
		}
		initialID := initial["event"].(map[string]any)["event_id"].(string)
		send := func(event map[string]any) {
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": event})
		}
		send(map[string]any{"type": "session.created", "client_event_id": initialID})
		send(map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}})
		// Replay of the parent turn.
		send(map[string]any{"type": "response.chunk", "response_id": "resp_old", "chunk": map[string]any{"text": map[string]any{"text": "OLD-ANSWER", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}})
		send(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp_old", "status": "completed"}})
		// User-message echo (untagged).
		send(map[string]any{"type": "response.chunk", "response_id": "", "chunk": map[string]any{"text": map[string]any{"text": "USER-ECHO"}}})
		var create map[string]any
		if connection.ReadJSON(&create) != nil {
			return
		}
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_new"}})
		send(map[string]any{"type": "response.chunk", "response_id": "resp_new", "chunk": map[string]any{"text": map[string]any{"text": "NEW-ANSWER", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}})
		send(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp_new", "status": "completed"}})
		time.Sleep(time.Second)
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/mgw/"
	connection, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	previous := &inferencedomain.WebResponseState{ConversationID: "conv_1", UpstreamParentResponseID: "resp_old"}
	if err := runGatewayStream(ctx, connection, &out, "fast", "q", nil, previous, true); err != nil {
		t.Fatalf("runGatewayStream: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "OLD-ANSWER") || strings.Contains(got, "USER-ECHO") || strings.Contains(got, "resp_old") {
		t.Fatalf("replayed parent frames leaked: %s", got)
	}
	if !strings.Contains(got, "NEW-ANSWER") || !strings.Contains(got, "resp_new") {
		t.Fatalf("new turn missing: %s", got)
	}
}
