package web

import (
	"regexp"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

// grok.com often answers a search turn with ordinary markdown links instead
// of render_citation chunks (notably Build/Expert final phases). Those links
// are the citations, but clients that read structured url_citation
// annotations (OpenAI SDKs, Cherry Studio's source panel) see none. When a
// search turn produced no positional annotations, derive them from markdown
// links in the final answer whose target is a URL the turn actually searched,
// browsed, or was handed back as a source.
var markdownLinkPattern = regexp.MustCompile(`\[([^\[\]\n]{1,300})\]\((https?://[^\s()]+(?:\([^\s()]*\)[^\s()]*)*)\)`)

// deriveLinkAnnotations appends url_citation annotations for markdown links in
// the answer. It does nothing when annotations already exist, when the turn
// ran no hosted search, or when inline citations are disabled (that path
// builds source-list annotations in finalizeXAIAnnotations instead).
func deriveLinkAnnotations(parsed *parsedChat) {
	if parsed == nil || parsed.DisableInlineCitations || len(parsed.Annotations) > 0 {
		return
	}
	if parsed.ServerTools == 0 && len(parsed.SearchSources) == 0 && len(parsed.HostedSearchCalls) == 0 {
		return
	}
	known := searchedURLSet(parsed)
	if len(known) == 0 {
		return
	}
	text := parsed.Text.String()
	matches := markdownLinkPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return
	}
	if parsed.citationIndex == nil {
		parsed.citationIndex = make(map[string]int)
	}
	runeCursor, byteCursor := 0, 0
	for _, match := range matches {
		if len(parsed.Annotations) >= maxTrackedAnnotations {
			return
		}
		// Skip images: ![alt](url).
		if match[0] > 0 && text[match[0]-1] == '!' {
			continue
		}
		normalized, ok := searchresult.NormalizeURL(text[match[4]:match[5]])
		if !ok {
			continue
		}
		if _, searched := known[normalized]; !searched {
			continue
		}
		runeCursor += utf8.RuneCountInString(text[byteCursor:match[0]])
		byteCursor = match[0]
		start := runeCursor
		end := start + utf8.RuneCountInString(text[match[0]:match[1]])
		index, exists := parsed.citationIndex[normalized]
		if !exists {
			if len(parsed.citationIndex) >= maxTrackedCitationSources {
				continue
			}
			index = len(parsed.citationIndex) + 1
			parsed.citationIndex[normalized] = index
		}
		annotation := citationAnnotation(parsed, normalized, index)
		annotation["start_index"] = start
		annotation["end_index"] = end
		parsed.Annotations = append(parsed.Annotations, annotation)
	}
}

func searchedURLSet(parsed *parsedChat) map[string]struct{} {
	known := make(map[string]struct{})
	add := func(raw any) {
		value, _ := raw.(string)
		if normalized, ok := searchresult.NormalizeURL(value); ok {
			known[normalized] = struct{}{}
		}
	}
	for _, source := range parsed.SearchSources {
		add(source["url"])
	}
	for _, call := range parsed.HostedSearchCalls {
		for _, source := range call.Sources {
			add(source["url"])
		}
	}
	for _, value := range parsed.browsedURLs {
		add(value)
	}
	return known
}
