package web

import "unicode/utf8"

// logWebTurnSummary writes one structured line per completed Grok Web chat
// turn: what the upstream did (searches, browses, phases) and what the client
// received (answer/reasoning size, citations). It never logs content, URLs,
// prompts or credentials.
func (a *Adapter) logWebTurnSummary(accountID uint64, model, operation string, streaming bool, parsed *parsedChat) {
	if a == nil || parsed == nil {
		return
	}
	a.log().Info("web_turn_summary",
		"account_id", accountID,
		"model", model,
		"operation", operation,
		"streaming", streaming,
		"server_tools", parsed.ServerTools,
		"web_searches", parsed.WebSearchTools,
		"x_searches", parsed.XSearchTools,
		"browsed_pages", len(parsed.browsedURLs),
		"search_sources", len(parsed.SearchSources),
		"citations", len(parsed.Annotations),
		"answer_chars", utf8.RuneCountInString(parsed.Text.String()),
		"reasoning_chars", utf8.RuneCountInString(parsed.Reasoning.String()),
		"tool_calls", len(parsed.ToolCalls),
		"images", len(parsed.Images),
	)
}
