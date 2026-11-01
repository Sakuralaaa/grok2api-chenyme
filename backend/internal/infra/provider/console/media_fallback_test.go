package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestIsConsoleImageSpendingLimitResponse(t *testing.T) {
	body := []byte(`{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`)
	result := &provider.Response{StatusCode: http.StatusPaymentRequired, Body: io.NopCloser(bytes.NewReader(body))}
	if !isConsoleImageSpendingLimitResponse(result) {
		t.Fatal("expected spending-limit 402 to be detected")
	}
	rest, _ := io.ReadAll(result.Body)
	if !bytes.Equal(rest, body) {
		t.Fatalf("body must be restored, got %q", rest)
	}
	other := &provider.Response{StatusCode: http.StatusPaymentRequired, Body: io.NopCloser(strings.NewReader(`{"code":"other"}`))}
	if isConsoleImageSpendingLimitResponse(other) {
		t.Fatal("other 402 codes must not trigger retry")
	}
	if isConsoleImageSpendingLimitResponse(&provider.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(bytes.NewReader(body))}) {
		t.Fatal("only 402 triggers retry")
	}
}

func TestBuildConsoleImageToolPayload(t *testing.T) {
	payload := buildConsoleImageToolPayload(consoleImageFallback{prompt: "a cat", aspectRatio: "16:9", count: 2})
	text, _ := payload["input"].(string)
	for _, want := range []string{"image generation tool", "aspect ratio 16:9", "Generate 2 different images", "Prompt: a cat"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt %q missing %q", text, want)
		}
	}
	if payload["tool_choice"] != "required" {
		t.Fatal("tool_choice must be required")
	}
	edit := buildConsoleImageToolPayload(consoleImageFallback{prompt: "make it red", count: 1, inputImages: []string{"data:image/png;base64,AAAA"}})
	raw, _ := json.Marshal(edit["input"])
	if !strings.Contains(string(raw), `"input_image"`) || !strings.Contains(string(raw), "edit the attached image") {
		t.Fatalf("edit input wrong: %s", raw)
	}
}

func TestParseConsoleImageToolOutput(t *testing.T) {
	data := []byte(`{"output":[{"type":"reasoning"},{"type":"image_generation_call","status":"completed","result":"QUJD"},{"type":"message"}]}`)
	images, err := parseConsoleImageToolOutput(data)
	if err != nil || len(images) != 1 || images[0] != "QUJD" {
		t.Fatalf("unexpected %v %v", images, err)
	}
	if _, err := parseConsoleImageToolOutput([]byte(`{"output":[{"type":"message"}]}`)); err == nil {
		t.Fatal("expected error when no image")
	}
}
