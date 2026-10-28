package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func init() {
	// Most package tests count exact upstream requests; the search probe is
	// enabled explicitly by the tests below.
	consoleSearchProbeEnabled = false
}

func enableConsoleSearchProbe(t *testing.T) {
	t.Helper()
	consoleSearchProbeEnabled = true
	t.Cleanup(func() { consoleSearchProbeEnabled = false })
}

func newSearchProbeServer(t *testing.T, gated bool, probes, chats *atomic.Int32, lastChat *atomic.Value) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			serveTestDPoPToken(t, writer, request)
			return
		}
		data, _ := io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(data), consoleSearchProbeFunction) {
			probes.Add(1)
			writer.WriteHeader(http.StatusBadRequest)
			if gated {
				_, _ = io.WriteString(writer, `{"code":"invalid-argument","error":"Invalid request content: A tool_choice was set on the request but no tools were specified."}`)
			} else {
				_, _ = io.WriteString(writer, `{"code":"invalid-argument","error":"Invalid request content: Required function grok2api_search_probe is not present in the provided tools."}`)
			}
			return
		}
		chats.Add(1)
		lastChat.Store(string(data))
		_, _ = io.WriteString(writer, `{"id":"resp_ok","object":"response","status":"completed","output":[]}`)
	}))
}

func TestConsoleSearchProbeShieldsGatedAccount(t *testing.T) {
	enableConsoleSearchProbe(t)
	var probes, chats atomic.Int32
	var lastChat atomic.Value
	server := newSearchProbeServer(t, true, &probes, &chats, &lastChat)
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	for i := 0; i < 2; i++ {
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.7",
			Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.7","input":"hello"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusTooManyRequests || response.RateLimit == nil || response.RateLimit.Model != "grok-4.7" || response.RateLimit.TeamID == "" {
			t.Fatalf("attempt %d: status=%d rateLimit=%#v", i, response.StatusCode, response.RateLimit)
		}
	}
	if probes.Load() != 1 || chats.Load() != 0 {
		t.Fatalf("probes=%d chats=%d, want cached single probe and no chat", probes.Load(), chats.Load())
	}
}

func TestConsoleSearchProbeAllowsCapableAccount(t *testing.T) {
	enableConsoleSearchProbe(t)
	var probes, chats atomic.Int32
	var lastChat atomic.Value
	server := newSearchProbeServer(t, false, &probes, &chats, &lastChat)
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	for i := 0; i < 2; i++ {
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.7",
			Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.7","input":"hello"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status=%d", i, response.StatusCode)
		}
	}
	if probes.Load() != 1 || chats.Load() != 2 {
		t.Fatalf("probes=%d chats=%d", probes.Load(), chats.Load())
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(lastChat.Load().(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if !consoleBodyHasSearchTools([]byte(lastChat.Load().(string))) {
		t.Fatalf("chat body lost search tools: %v", payload["tools"])
	}
}

func TestConsoleSearchToolsDroppedResponseIsShielded(t *testing.T) {
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			serveTestDPoPToken(t, writer, request)
			return
		}
		chats.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"code":"invalid-argument","error":"Invalid request content: A tool_choice was set on the request but no tools were specified."}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.7",
		Operation: conversation.OperationChat, NormalizeBody: true, Body: []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.RateLimit == nil {
		t.Fatalf("status=%d rateLimit=%#v", response.StatusCode, response.RateLimit)
	}
	if capable, ok := adapter.searchCaps.get(credential.ID, "grok-4.7", time.Now()); !ok || capable {
		t.Fatalf("cache capable=%v ok=%v", capable, ok)
	}
}

func TestConsoleBodyHasSearchToolsHonoursToolChoiceNone(t *testing.T) {
	if consoleBodyHasSearchTools([]byte(`{"tools":[{"type":"web_search"}],"tool_choice":"none"}`)) {
		t.Fatal("tool_choice none must skip the search capability check")
	}
	if !consoleBodyHasSearchTools([]byte(`{"tools":[{"type":"function","name":"f"},{"type":"x_search"}],"tool_choice":"auto"}`)) {
		t.Fatal("x_search must be detected")
	}
}
