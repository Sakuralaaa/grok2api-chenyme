package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Live 2026-10-29: some Console teams have the server-side search tools gated
// off for grok-4.5 and newer (grok-4.3 still works on the same team). Upstream
// then silently drops web_search/x_search: with client function tools the model
// answers from memory, and without them the request fails with
// "A tool_choice was set on the request but no tools were specified.".
//
// The gateway turns on search by default, so such an account is shielded for
// that model and the request rotates to another account. Capability is learned
// with a free probe: a tool_choice naming a function that is not declared makes
// a capable team answer "Required function ... is not present in the provided
// tools" and a gated one answer "no tools were specified". Both are 400s that
// are never billed.

const (
	consoleSearchProbeFunction   = "grok2api_search_probe"
	consoleSearchCapabilityTTL   = 6 * time.Hour
	consoleSearchUnavailableTTL  = 6 * time.Hour
	consoleSearchUnavailableCode = "console_search_tools_unavailable"
	// RateLimitScopeSearchTools marks the synthetic team+model shield used when
	// the account's team cannot use the built-in search tools for a model.
	consoleRateLimitScopeSearchTools = "search_tools"
)

// consoleSearchProbeEnabled lets the package tests that count upstream
// requests keep their exact request sequence; production always probes.
var consoleSearchProbeEnabled = true

type consoleSearchCapability struct {
	capable   bool
	expiresAt time.Time
}

type consoleSearchCapabilityCache struct {
	mu      sync.Mutex
	entries map[string]consoleSearchCapability
}

func newConsoleSearchCapabilityCache() *consoleSearchCapabilityCache {
	return &consoleSearchCapabilityCache{entries: make(map[string]consoleSearchCapability)}
}

func consoleSearchCapabilityKey(accountID uint64, model string) string {
	return strconv.FormatUint(accountID, 10) + "|" + strings.ToLower(strings.TrimSpace(model))
}

func (c *consoleSearchCapabilityCache) get(accountID uint64, model string, now time.Time) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := consoleSearchCapabilityKey(accountID, model)
	entry, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		return false, false
	}
	return entry.capable, true
}

func (c *consoleSearchCapabilityCache) set(accountID uint64, model string, capable bool, now time.Time) {
	if c == nil {
		return
	}
	ttl := consoleSearchCapabilityTTL
	if !capable {
		ttl = consoleSearchUnavailableTTL
	}
	c.mu.Lock()
	c.entries[consoleSearchCapabilityKey(accountID, model)] = consoleSearchCapability{capable: capable, expiresAt: now.Add(ttl)}
	c.mu.Unlock()
}

// consoleBodyHasSearchTools reports whether the outbound Responses body asks for
// web_search or x_search with a tool_choice that lets the model use them.
func consoleBodyHasSearchTools(body []byte) bool {
	var payload struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if choice, ok := payload.ToolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(choice), "none") {
		return false
	}
	for _, tool := range payload.Tools {
		if typeName, _ := tool["type"].(string); typeName == "web_search" || typeName == "x_search" {
			return true
		}
	}
	return false
}

// isConsoleSearchToolsDroppedBody matches the upstream rejection a gated team
// returns when every declared tool was a server-side search tool.
func isConsoleSearchToolsDroppedBody(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "tool_choice was set on the request but no tools were specified")
}

// consoleSearchCapable returns false only when the account's team is known to
// have the search tools gated off for the model. Unknown outcomes (network
// errors, other statuses) return true so a probe problem never blocks traffic.
func (a *Adapter) consoleSearchCapable(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, upstreamModel string) bool {
	if a.searchCaps == nil || !consoleSearchProbeEnabled {
		return true
	}
	now := time.Now()
	if capable, ok := a.searchCaps.get(credential.ID, upstreamModel, now); ok {
		return capable
	}
	probe, _ := json.Marshal(map[string]any{
		"model":             upstreamModel,
		"input":             "ping",
		"tools":             []any{map[string]any{"type": "web_search"}},
		"tool_choice":       map[string]any{"type": "function", "name": consoleSearchProbeFunction},
		"max_output_tokens": 16,
		"store":             false,
	})
	response, err := a.doDPoPRequest(ctx, credential, ssoToken, lease, http.MethodPost, consoleEndpoint(a.config().BaseURL), probe, "*/*")
	if err != nil {
		return true
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	text := strings.ToLower(string(data))
	switch {
	case response.StatusCode == http.StatusBadRequest && isConsoleSearchToolsDroppedBody(data):
		a.searchCaps.set(credential.ID, upstreamModel, false, now)
		a.logger().Warn("console_search_tools_unavailable", "account_id", credential.ID, "model", upstreamModel)
		return false
	case response.StatusCode == http.StatusBadRequest && strings.Contains(text, consoleSearchProbeFunction) && strings.Contains(text, "not present"):
		a.searchCaps.set(credential.ID, upstreamModel, true, now)
		return true
	case response.StatusCode >= 200 && response.StatusCode < 300:
		a.searchCaps.set(credential.ID, upstreamModel, true, now)
		return true
	default:
		return true
	}
}

// consoleSearchUnavailableResponse is a synthetic team+model 429 so the gateway
// rotates to another account without penalising this one for other models.
func consoleSearchUnavailableResponse(credential account.Credential, upstreamModel string) *provider.Response {
	retryAfter := consoleSearchUnavailableTTL
	response := jsonProviderResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"type":    "rate_limit_error",
		"code":    consoleSearchUnavailableCode,
		"message": "该 Console 账号的 " + upstreamModel + " 暂不支持 web_search/x_search，已切换其他账号",
	}})
	response.Header.Set("Retry-After", strconv.FormatInt(int64(retryAfter/time.Second), 10))
	response.RateLimit = &provider.RateLimitMetadata{
		Scope:      consoleRateLimitScopeSearchTools,
		TeamID:     "console-search-tools:" + strconv.FormatUint(credential.ID, 10),
		Model:      upstreamModel,
		RetryAfter: retryAfter,
	}
	return response
}
