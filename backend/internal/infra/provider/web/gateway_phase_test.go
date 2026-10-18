package web

import (
	"strconv"
	"strings"
	"testing"
)

// frame helpers for the mgw gateway NDJSON protocol.
func respStart(phase int) string {
	return `{"event":{"type":"response.chunk","chunk":{"metadata":{"phase_index":` + strconv.Itoa(phase) + `},"phase_marker":{"kind":"KIND_RESPONSE_START","phase_index":` + strconv.Itoa(phase) + `}}}}`
}
func thinkStart(phase int) string {
	return `{"event":{"type":"response.chunk","chunk":{"phase_marker":{"kind":"KIND_THINKING_START","phase_index":` + strconv.Itoa(phase) + `}}}}`
}
func respText(t string) string {
	return `{"event":{"type":"response.chunk","chunk":{"text":{"text":"` + t + `","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`
}
func searchCard(q string) string {
	return `{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"c-` + q + `","web_search":{"args":{"query":"` + q + `"}}}}}}`
}
func doneFrame() string {
	return `{"event":{"type":"response.done","response":{"id":"r1","status":"completed"}}}`
}

func collect(t *testing.T, frames ...string) (reasoning, text string) {
	t.Helper()
	var r, x strings.Builder
	parsed, err := consumeUpstreamWithCitations(strings.NewReader(strings.Join(frames, "\n")), func(kind, delta string) error {
		switch kind {
		case "reasoning":
			r.WriteString(delta)
		case "text":
			x.WriteString(delta)
		}
		return nil
	}, true)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if parsed.Text.String() != x.String() {
		t.Fatalf("streamed text %q != parsed.Text %q", x.String(), parsed.Text.String())
	}
	return r.String(), x.String()
}

// Interim narration between search phases must land in reasoning, and only the
// final phase in answer text.
func TestGatewayInterimNarrationGoesToReasoning(t *testing.T) {
	reasoning, text := collect(t,
		thinkStart(1),
		respStart(2), respText("let me check the news."),
		searchCard("top news"),
		thinkStart(3),
		respStart(4),
		respText("The top story is X."),
		doneFrame(),
	)
	if !strings.Contains(reasoning, "let me check the news.") {
		t.Fatalf("narration missing from reasoning: %q", reasoning)
	}
	if strings.Contains(text, "let me check") {
		t.Fatalf("narration leaked into answer: %q", text)
	}
	if strings.TrimSpace(text) != "The top story is X." {
		t.Fatalf("answer text = %q", text)
	}
	if !strings.Contains(reasoning, "top news") {
		t.Fatalf("search query missing from reasoning: %q", reasoning)
	}
}

// A plain single-phase answer must still be delivered as answer text.
func TestGatewaySinglePhaseAnswerIsText(t *testing.T) {
	_, text := collect(t,
		thinkStart(1),
		respStart(2),
		respText("Tokyo is sunny today."),
		doneFrame(),
	)
	if strings.TrimSpace(text) != "Tokyo is sunny today." {
		t.Fatalf("answer text = %q", text)
	}
}
