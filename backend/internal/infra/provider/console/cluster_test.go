package console

import (
	"context"
	"net/http"
	"testing"
)

func TestConsoleClusterForRequest(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"/v1/responses":          consoleClusterUSEast1,
		"/v1/images/generations": consoleClusterUSWest2,
		"/v1/images/edits":       consoleClusterUSWest2,
		"/v1/videos/generations": "",
	}
	for path, want := range cases {
		if got := consoleClusterForRequest(ctx, path); got != want {
			t.Fatalf("%s: got %q want %q", path, got, want)
		}
	}
	if got := consoleClusterForRequest(withConsoleCluster(ctx, consoleClusterUSWest2), "/v1/responses"); got != consoleClusterUSWest2 {
		t.Fatalf("override ignored: %q", got)
	}
}

func TestConsoleImageUnavailableResponseIsTransient(t *testing.T) {
	result := consoleImageUnavailableResponse(nil)
	if result.StatusCode != http.StatusServiceUnavailable || result.Header.Get(ConsoleImageTransientHeader) != "1" {
		t.Fatalf("unexpected response: %d %v", result.StatusCode, result.Header)
	}
	if len(consoleImageToolPlans) < 2 || consoleImageToolPlans[0].model != consoleImageToolModel {
		t.Fatalf("unexpected tool plans: %+v", consoleImageToolPlans)
	}
}
