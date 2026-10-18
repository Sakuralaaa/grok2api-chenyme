package web

import (
	"testing"
)

func TestDeriveLinkAnnotationsFromMarkdownLinks(t *testing.T) {
	parsed := &parsedChat{ServerTools: 1}
	appendSearchSource(parsed, "https://example.com/a", "Example A", "web")
	parsed.browsedURLs = []string{"https://news.test/story"}
	parsed.Text.WriteString("天气见 [中国天气](https://example.com/a)，另见 [报道](https://news.test/story) 和 [无关](https://other.test/x)，图 ![p](https://example.com/a)。")
	deriveLinkAnnotations(parsed)
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %d, want 2: %#v", len(parsed.Annotations), parsed.Annotations)
	}
	first := parsed.Annotations[0]
	if first["url"] != "https://example.com/a" || first["title"] != "1" || first["source_title"] != "Example A" {
		t.Fatalf("first annotation = %#v", first)
	}
	runes := []rune(parsed.Text.String())
	start, end := first["start_index"].(int), first["end_index"].(int)
	if got := string(runes[start:end]); got != "[中国天气](https://example.com/a)" {
		t.Fatalf("first span = %q", got)
	}
	second := parsed.Annotations[1]
	start, end = second["start_index"].(int), second["end_index"].(int)
	if got := string(runes[start:end]); got != "[报道](https://news.test/story)" || second["title"] != "2" {
		t.Fatalf("second span = %q ann=%#v", got, second)
	}
}

func TestDeriveLinkAnnotationsSkipsWithoutSearch(t *testing.T) {
	parsed := &parsedChat{}
	parsed.Text.WriteString("[x](https://example.com/a)")
	deriveLinkAnnotations(parsed)
	if len(parsed.Annotations) != 0 {
		t.Fatalf("want no annotations without a search turn")
	}
}
