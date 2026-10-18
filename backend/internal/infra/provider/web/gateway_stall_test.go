package web

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// stallServer accepts a turn, then emits only search tool frames (or nothing)
// with heartbeats, never any assistant text.
func stallServer(t *testing.T, search bool, answerAfter time.Duration) *fhttptest.Server {
	t.Helper()
	return fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
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
		send := func(event map[string]any) error {
			return connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": event})
		}
		_ = send(map[string]any{"type": "session.created", "client_event_id": initialID})
		_ = send(map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}})
		var create map[string]any
		if connection.ReadJSON(&create) != nil {
			return
		}
		_ = send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}})
		start := time.Now()
		for time.Since(start) < 3*time.Second {
			if answerAfter > 0 && time.Since(start) >= answerAfter {
				_ = send(map[string]any{"type": "response.chunk", "response_id": "resp_1", "chunk": map[string]any{"text": map[string]any{"text": "ANSWER", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}})
				_ = send(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp_1", "status": "completed"}})
				time.Sleep(200 * time.Millisecond)
				return
			}
			if search {
				if send(map[string]any{"type": "response.tool_call", "response_id": "resp_1"}) != nil {
					return
				}
			} else if send(map[string]any{"type": "pong"}) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
}

func runStall(t *testing.T, server *fhttptest.Server, budget time.Duration) error {
	t.Helper()
	original := gatewayFirstOutputBudget
	gatewayFirstOutputBudget = func(string) time.Duration { return budget }
	t.Cleanup(func() { gatewayFirstOutputBudget = original })
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/mgw/"
	connection, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	return runGatewayStream(ctx, connection, &out, "fast", "q", nil, nil, true)
}

func TestGatewaySearchOnlyStallFailsAsDegraded(t *testing.T) {
	server := stallServer(t, true, 0)
	defer server.Close()
	started := time.Now()
	err := runStall(t, server, 500*time.Millisecond)
	if !neterrorpkg.IsUpstreamSearchDegraded(err) {
		t.Fatalf("err = %v, want search degraded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stall detection took %s", elapsed)
	}
}

func TestGatewaySilentStallFailsAsEmpty(t *testing.T) {
	server := stallServer(t, false, 0)
	defer server.Close()
	err := runStall(t, server, 500*time.Millisecond)
	if !errors.Is(err, neterrorpkg.ErrUpstreamResponseEmpty) || neterrorpkg.IsUpstreamSearchDegraded(err) {
		t.Fatalf("err = %v, want empty (not search degraded)", err)
	}
}

func TestGatewayAnswerBeforeBudgetSucceeds(t *testing.T) {
	server := stallServer(t, true, 300*time.Millisecond)
	defer server.Close()
	if err := runStall(t, server, time.Second); err != nil {
		t.Fatalf("healthy turn failed: %v", err)
	}
}
