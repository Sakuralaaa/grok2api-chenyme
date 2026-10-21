package console

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Console Playground no longer has a keyless free quota: requests without an
// API key are rejected with "Free usage quota exceeded ... provision an API
// key". The official playground (console.x.ai/team/<id>/chat) resolves the
// personal team via gRPC-web AuthManagement/ListTeams, reuses or creates a
// team API key ("xAI Chat Playground", acls api-key:endpoint:* +
// api-key:model:*) and sends its ID as the x-apikey header next to DPoP.

const (
	consolePlaygroundKeyName = "xAI Chat Playground"
	consoleAPIKeyTTL         = 6 * time.Hour
	authMgmtService          = "auth_mgmt.AuthManagement"
)

var consolePlaygroundKeyACLs = []string{"api-key:endpoint:*", "api-key:model:*"}

type consoleAPIKeyEntry struct {
	teamID    string
	apiKeyID  string
	expiresAt time.Time
}

type consoleAPIKeyCache struct {
	mu      sync.Mutex
	entries map[string]consoleAPIKeyEntry
	locks   map[string]*sync.Mutex
}

func newConsoleAPIKeyCache() *consoleAPIKeyCache {
	return &consoleAPIKeyCache{entries: map[string]consoleAPIKeyEntry{}, locks: map[string]*sync.Mutex{}}
}

func consoleAPIKeyCacheKey(credential account.Credential, ssoToken string) string {
	return strconv.FormatUint(credential.ID, 10) + "|" + security.HashToken(ssoToken)
}

func (c *consoleAPIKeyCache) lock(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[key]
	if !ok {
		l = &sync.Mutex{}
		c.locks[key] = l
	}
	return l
}

func (c *consoleAPIKeyCache) get(key string) (consoleAPIKeyEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return consoleAPIKeyEntry{}, false
	}
	return entry, true
}

func (c *consoleAPIKeyCache) put(key string, entry consoleAPIKeyEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.expiresAt = time.Now().Add(consoleAPIKeyTTL)
	c.entries[key] = entry
}

func (c *consoleAPIKeyCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

var globalConsoleAPIKeys = newConsoleAPIKeyCache()

// consoleAPIKeyEnabled lets unit tests that fake only /v1 endpoints opt out.
var consoleAPIKeyEnabled = true

// consoleRequiresAPIKey reports whether a Console /v1 request must carry the
// playground x-apikey header (everything except DPoP minting and /usage).
func consoleRequiresAPIKey(path string) bool {
	if !consoleAPIKeyEnabled {
		return false
	}
	path = strings.TrimRight(path, "/")
	idx := strings.Index(path, "/v1/")
	if idx < 0 {
		return false
	}
	rest := path[idx+len("/v1"):]
	return !strings.HasPrefix(rest, "/dpop/") && rest != "/usage"
}

// isConsoleAPIKeyRejectedBody detects responses that mean the cached key ID is
// no longer valid (deleted, disabled, propagated away) so it can be refreshed.
func isConsoleAPIKeyRejectedBody(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "credits") || strings.Contains(lower, "spending") {
		return false
	}
	return strings.Contains(lower, "api key") || strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey")
}

func (a *Adapter) ensureConsoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease) (string, error) {
	cacheKey := consoleAPIKeyCacheKey(credential, ssoToken)
	if entry, ok := globalConsoleAPIKeys.get(cacheKey); ok {
		return entry.apiKeyID, nil
	}
	l := globalConsoleAPIKeys.lock(cacheKey)
	l.Lock()
	defer l.Unlock()
	if entry, ok := globalConsoleAPIKeys.get(cacheKey); ok {
		return entry.apiKeyID, nil
	}
	teamID, err := a.consoleTeamID(ctx, credential, ssoToken, lease)
	if err != nil {
		return "", err
	}
	keyID, err := a.findConsolePlaygroundKey(ctx, credential, ssoToken, lease, teamID)
	if err != nil {
		return "", err
	}
	if keyID == "" {
		keyID, err = a.createConsolePlaygroundKey(ctx, credential, ssoToken, lease, teamID)
		if err != nil {
			return "", err
		}
	}
	globalConsoleAPIKeys.put(cacheKey, consoleAPIKeyEntry{teamID: teamID, apiKeyID: keyID})
	return keyID, nil
}

func (a *Adapter) invalidateConsoleAPIKey(credential account.Credential, ssoToken string) {
	globalConsoleAPIKeys.invalidate(consoleAPIKeyCacheKey(credential, ssoToken))
}

func (a *Adapter) consoleTeamID(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease) (string, error) {
	data, err := a.consoleGRPC(ctx, credential, ssoToken, lease, authMgmtService, "ListTeams", nil)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, team := range pbRepeatedBytes(data, 1) {
		teamID := pbString(team, 1)
		if teamID == "" {
			continue
		}
		if fallback == "" {
			fallback = teamID
		}
		if strings.Contains(strings.ToLower(pbString(team, 6)), "personal team") {
			return teamID, nil
		}
	}
	if fallback == "" {
		return "", errors.New("Console 账号没有可用团队")
	}
	return fallback, nil
}

func (a *Adapter) findConsolePlaygroundKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, teamID string) (string, error) {
	request := pbAppendString(nil, 2, teamID)
	request = pbAppendBool(request, 7, true)
	data, err := a.consoleGRPC(ctx, credential, ssoToken, lease, authMgmtService, "ListApiKeys", request)
	if err != nil {
		return "", err
	}
	var anyMatch string
	for _, key := range pbRepeatedBytes(data, 1) {
		if pbBool(key, 15) {
			continue
		}
		keyID := pbString(key, 8)
		if keyID == "" || !consoleKeyACLsCoverPlayground(pbRepeatedStrings(key, 16)) {
			continue
		}
		if pbString(key, 4) == consolePlaygroundKeyName {
			return keyID, nil
		}
		if anyMatch == "" {
			anyMatch = keyID
		}
	}
	return anyMatch, nil
}

func consoleKeyACLsCoverPlayground(acls []string) bool {
	endpoint, model := false, false
	for _, acl := range acls {
		switch acl {
		case "api-key:endpoint:*":
			endpoint = true
		case "api-key:model:*":
			model = true
		}
	}
	return endpoint && model
}

func (a *Adapter) createConsolePlaygroundKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, teamID string) (string, error) {
	request := pbAppendString(nil, 1, consolePlaygroundKeyName)
	request = pbAppendString(request, 2, teamID)
	for _, acl := range consolePlaygroundKeyACLs {
		request = pbAppendString(request, 3, acl)
	}
	data, err := a.consoleGRPC(ctx, credential, ssoToken, lease, authMgmtService, "CreateApiKey", request)
	if err != nil {
		return "", err
	}
	keyID := pbString(data, 8)
	if keyID == "" {
		return "", errors.New("Console 创建 Playground API Key 未返回 ID")
	}
	return keyID, nil
}

func consoleOrigin(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "https://console.x.ai"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// consoleGRPC performs a unary gRPC-web (binary protobuf) call authenticated
// with the account's DPoP session, returning the raw response message bytes.
func (a *Adapter) consoleGRPC(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, service, method string, message []byte) ([]byte, error) {
	frame := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	frame = append(frame, message...)
	endpoint := consoleOrigin(a.config().BaseURL) + "/" + service + "/" + method
	response, err := a.doDPoPRequestWithContentType(ctx, credential, ssoToken, lease, http.MethodPost, endpoint, frame, "application/grpc-web+proto", "application/grpc-web+proto")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Console %s HTTP %d", method, response.StatusCode)
	}
	if status := response.Header.Get("grpc-status"); status != "" && status != "0" {
		msg, _ := url.QueryUnescape(response.Header.Get("grpc-message"))
		return nil, fmt.Errorf("Console %s grpc-status %s: %s", method, status, msg)
	}
	var out []byte
	for len(body) >= 5 {
		flag := body[0]
		length := int(binary.BigEndian.Uint32(body[1:5]))
		if 5+length > len(body) {
			break
		}
		chunk := body[5 : 5+length]
		body = body[5+length:]
		if flag&0x80 != 0 {
			trailer := string(chunk)
			if idx := strings.Index(trailer, "grpc-status:"); idx >= 0 {
				status := strings.TrimSpace(strings.SplitN(trailer[idx+len("grpc-status:"):], "\r\n", 2)[0])
				if status != "0" {
					return nil, fmt.Errorf("Console %s grpc-status %s: %s", method, status, strings.TrimSpace(trailer))
				}
			}
			continue
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// --- minimal protobuf wire helpers (strings, bools, nested messages) ---

func pbAppendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func pbAppendString(buf []byte, field int, value string) []byte {
	buf = pbAppendVarint(buf, uint64(field<<3|2))
	buf = pbAppendVarint(buf, uint64(len(value)))
	return append(buf, value...)
}

func pbAppendBool(buf []byte, field int, value bool) []byte {
	if !value {
		return buf
	}
	buf = pbAppendVarint(buf, uint64(field<<3))
	return append(buf, 1)
}

func pbReadVarint(buf []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		v |= uint64(buf[i]&0x7f) << (7 * i)
		if buf[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// pbFields walks a message and calls fn for each field (bytes for wire type 2,
// varint value otherwise). Malformed input stops the walk.
func pbFields(buf []byte, fn func(field int, wire int, raw []byte, varint uint64)) {
	for len(buf) > 0 {
		tag, n := pbReadVarint(buf)
		if n == 0 {
			return
		}
		buf = buf[n:]
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			v, m := pbReadVarint(buf)
			if m == 0 {
				return
			}
			fn(field, wire, nil, v)
			buf = buf[m:]
		case 1:
			if len(buf) < 8 {
				return
			}
			buf = buf[8:]
		case 2:
			l, m := pbReadVarint(buf)
			if m == 0 || uint64(len(buf)-m) < l {
				return
			}
			fn(field, wire, buf[m:m+int(l)], 0)
			buf = buf[m+int(l):]
		case 5:
			if len(buf) < 4 {
				return
			}
			buf = buf[4:]
		default:
			return
		}
	}
}

func pbRepeatedBytes(buf []byte, field int) [][]byte {
	var out [][]byte
	pbFields(buf, func(f, wire int, raw []byte, _ uint64) {
		if f == field && wire == 2 {
			out = append(out, bytes.Clone(raw))
		}
	})
	return out
}

func pbRepeatedStrings(buf []byte, field int) []string {
	var out []string
	for _, raw := range pbRepeatedBytes(buf, field) {
		out = append(out, string(raw))
	}
	return out
}

func pbString(buf []byte, field int) string {
	values := pbRepeatedStrings(buf, field)
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func pbBool(buf []byte, field int) bool {
	var out bool
	pbFields(buf, func(f, wire int, _ []byte, v uint64) {
		if f == field && wire == 0 {
			out = v != 0
		}
	})
	return out
}
