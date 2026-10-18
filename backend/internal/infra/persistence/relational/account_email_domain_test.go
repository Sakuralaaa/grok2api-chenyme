package relational

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCountEmailDomains(t *testing.T) {
	got := countEmailDomains([]string{"a@Gmail.com", " b@gmail.com ", "c@outlook.com", "", "noat", "d@"})
	want := []repository.EmailDomainCount{{Domain: "gmail.com", Count: 2}, {Domain: "outlook.com", Count: 1}, {Domain: repository.EmailDomainNone, Count: 3}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
