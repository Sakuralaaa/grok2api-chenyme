package console

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestConsoleRequiresAPIKey(t *testing.T) {
	prev := consoleAPIKeyEnabled
	consoleAPIKeyEnabled = true
	defer func() { consoleAPIKeyEnabled = prev }()
	cases := map[string]bool{
		"/v1/responses": true, "/v1/images/generations": true, "/v1/videos/abc": true,
		"/v1/realtime": true, "/v1/usage": false, "/v1/dpop/token": false,
		"/auth_mgmt.AuthManagement/ListTeams": false,
	}
	for path, want := range cases {
		if got := consoleRequiresAPIKey(path); got != want {
			t.Fatalf("consoleRequiresAPIKey(%q)=%v want %v", path, got, want)
		}
	}
}

func TestConsoleProtobufHelpers(t *testing.T) {
	key := pbAppendString(nil, 4, consolePlaygroundKeyName)
	key = pbAppendString(key, 8, "key-1")
	key = pbAppendString(key, 16, "api-key:endpoint:*")
	key = pbAppendString(key, 16, "api-key:model:*")
	disabled := pbAppendString(nil, 8, "key-0")
	disabled = pbAppendBool(disabled, 15, true)
	list := pbAppendString(nil, 1, string(disabled))
	list = pbAppendString(list, 1, string(key))
	keys := pbRepeatedBytes(list, 1)
	if len(keys) != 2 || !pbBool(keys[0], 15) || pbString(keys[1], 8) != "key-1" {
		t.Fatalf("decode keys = %#v", keys)
	}
	if !consoleKeyACLsCoverPlayground(pbRepeatedStrings(keys[1], 16)) {
		t.Fatal("expected playground acls")
	}
}

func TestConsoleAPIKeyRejectedBody(t *testing.T) {
	if !isConsoleAPIKeyRejectedBody(401, []byte(`{"error":"Incorrect API key provided"}`)) {
		t.Fatal("expected api key rejection")
	}
	if isConsoleAPIKeyRejectedBody(403, []byte(`{"error":"Your team has used all available credits ... API requests"}`)) {
		t.Fatal("credits exhaustion must not refresh key")
	}
}

func TestMain(m *testing.M) {
	consoleAPIKeyEnabled = false
	m.Run()
}

func grpcWebFrame(message []byte) []byte {
	frame := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	frame = append(frame, message...)
	trailer := []byte("grpc-status:0\r\n")
	tf := make([]byte, 5)
	tf[0] = 0x80
	binary.BigEndian.PutUint32(tf[1:], uint32(len(trailer)))
	return append(append(frame, tf...), trailer...)
}

func TestConsoleResponsesCreatesPlaygroundAPIKey(t *testing.T) {
	consoleAPIKeyEnabled = true
	defer func() { consoleAPIKeyEnabled = false }()
	var created atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/dpop/token":
			serveTestDPoPToken(t, writer, request)
		case "/auth_mgmt.AuthManagement/ListTeams":
			verifyTestDPoPProof(t, request)
			team := pbAppendString(nil, 1, "team-1")
			team = pbAppendString(team, 6, "Your personal team to get started with the xAI API.")
			writer.Header().Set("Content-Type", "application/grpc-web+proto")
			_, _ = writer.Write(grpcWebFrame(pbAppendString(nil, 1, string(team))))
		case "/auth_mgmt.AuthManagement/ListApiKeys":
			writer.Header().Set("Content-Type", "application/grpc-web+proto")
			_, _ = writer.Write(grpcWebFrame(nil))
		case "/auth_mgmt.AuthManagement/CreateApiKey":
			created.Add(1)
			body, _ := io.ReadAll(request.Body)
			if len(body) < 5 || pbString(body[5:], 2) != "team-1" || pbString(body[5:], 1) != consolePlaygroundKeyName {
				t.Errorf("create body = %x", body)
			}
			writer.Header().Set("Content-Type", "application/grpc-web+proto")
			_, _ = writer.Write(grpcWebFrame(pbAppendString(nil, 8, "key-123")))
		case "/v1/usage":
			if request.Header.Get("x-apikey") != "" {
				t.Errorf("usage must not carry x-apikey")
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":0,"used":0,"remaining":0},{"kind":"image","limit":0,"used":0,"remaining":0},{"kind":"video","limit":0,"used":0,"remaining":0}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	ssoToken, err := adapter.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := adapter.egress.AcquireCredential(context.Background(), egressdomain.ScopeConsole, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for range 2 {
		keyID, err := adapter.ensureConsoleAPIKey(context.Background(), credential, ssoToken, lease)
		if err != nil || keyID != "key-123" {
			t.Fatalf("ensureConsoleAPIKey = %q, %v", keyID, err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("CreateApiKey calls = %d", created.Load())
	}
	snapshot, err := adapter.SyncQuota(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range snapshot.Windows {
		if window.Source != account.QuotaSourceEstimated || window.Remaining <= 0 {
			t.Fatalf("limit-0 window must not be exhausted upstream: %#v", window)
		}
	}
}
