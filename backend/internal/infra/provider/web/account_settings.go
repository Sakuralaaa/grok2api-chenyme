package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
)

const (
	officialAccountsBaseURL       = "https://accounts.x.ai"
	webAccountSettingTimeout      = 15 * time.Second
	webAccountSettingBodyLimit    = 64 << 10
	webAccountSettingErrorPreview = 240
	webBirthDateLockedMarker      = "[WKE=account:birth-date-change-limit-reached]"

	// Grok Web 的资料设置不是 REST，而是 Next.js Server Action：
	//   POST https://grok.com/  + Next-Action 头 + text/plain body
	// 下面三个 action id 是 2026-10 抓包实测的构建常量，上游重新发版后可能变化；
	// 失效时的表现是响应体里出现 "error" 字段（不会返回非 2xx）。
	webServerActionState        = "00fad60341437f6ebf62636b07e6bb0eeeaef04182"
	webServerActionSetName      = "7f8e273c2dfe9af53c782c9e38214d7ce55d938601"
	webServerActionSetBirthDate = "7f3c642562b678da31a68ea7746e0b6e58b8fdf190"
	webNextRouterStateTree      = `["",{"children":["__PAGE__",{},null,null,0]},null,null,16]`
	webOfficialBaseURL          = "https://grok.com"
	webDisplayNameMaxLength     = 129
)

var (
	acceptTermsBody = []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x10, 0x01}
	enableNSFWBody  = []byte{
		0x00, 0x00, 0x00, 0x00, 0x20,
		0x0a, 0x02, 0x10, 0x01,
		0x12, 0x1a, 0x0a, 0x18,
		'a', 'l', 'w', 'a', 'y', 's', '_', 's', 'h', 'o', 'w', '_', 'n', 's', 'f', 'w', '_', 'c', 'o', 'n', 't', 'e', 'n', 't',
	}
)

// AcceptTerms 接受当前 Grok Web SSO 账号的上游服务协议。
func (a *Adapter) AcceptTerms(ctx context.Context, credential account.Credential) error {
	accountBaseURL := strings.TrimRight(a.accountsBaseURL, "/")
	if accountBaseURL == "" {
		accountBaseURL = officialAccountsBaseURL
	}
	webBaseURL := strings.TrimRight(a.config().BaseURL, "/")
	if webBaseURL == "" {
		return fmt.Errorf("Grok Web BaseURL 不能为空")
	}
	productBody, err := json.Marshal(struct {
		TOSVersion int `json:"tosVersion"`
	}{TOSVersion: account.CurrentWebTermsVersion})
	if err != nil {
		return err
	}
	return a.runWebAccountSettings(ctx, credential,
		webAccountSettingRequest{
			endpoint:    accountBaseURL + "/auth_mgmt.AuthManagement/SetTosAcceptedVersion",
			body:        acceptTermsBody,
			contentType: "application/grpc-web+proto",
			origin:      accountBaseURL,
			referer:     accountBaseURL + "/accept-tos",
			grpcWeb:     true,
			connectES:   true,
			withoutCF:   true,
		},
		webAccountSettingRequest{
			endpoint:    webBaseURL + "/rest/auth/set-tos-accepted",
			body:        productBody,
			contentType: "application/json",
			origin:      webBaseURL,
			referer:     webBaseURL + "/",
			statsig:     true,
			clientHints: true,
		},
	)
}

// DisplayName 读取账号当前的上游显示名（givenName/familyName）。
// 走 Server Action 的「读资料」动作，body 为空数组。
func (a *Adapter) DisplayName(ctx context.Context, credential account.Credential) (string, string, error) {
	baseURL := a.webBaseURL()
	var body []byte
	err := a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:     baseURL + "/",
		body:         []byte("[]"),
		contentType:  "text/plain;charset=UTF-8",
		origin:       baseURL,
		referer:      baseURL + "/",
		serverAction: webServerActionState,
		captureBody:  &body,
	})
	if err != nil {
		return "", "", err
	}
	given, family := webServerActionUserName(body)
	return given, family, nil
}

// SetDisplayName 设置上游显示名（全名）。
// 实测：该动作返回的是「裸 user 对象」（1:{...}），没有 user 包装，
// 且成功后按 givenName 回读校验最可靠。
func (a *Adapter) SetDisplayName(ctx context.Context, credential account.Credential, givenName, familyName string) error {
	givenName = strings.TrimSpace(givenName)
	familyName = strings.TrimSpace(familyName)
	if givenName == "" {
		return fmt.Errorf("显示名的 givenName 不能为空")
	}
	if len(givenName)+len(familyName)+1 > webDisplayNameMaxLength {
		return fmt.Errorf("显示名超过上游长度上限")
	}
	payload, err := json.Marshal([]map[string]string{{
		"givenName":  givenName,
		"familyName": familyName,
	}})
	if err != nil {
		return err
	}
	baseURL := a.webBaseURL()
	var body []byte
	err = a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:     baseURL + "/",
		body:         payload,
		contentType:  "text/plain;charset=UTF-8",
		origin:       baseURL,
		referer:      baseURL + "/",
		serverAction: webServerActionSetName,
		captureBody:  &body,
	})
	if err != nil {
		return err
	}
	gotGiven, _ := webServerActionUserName(body)
	if gotGiven != givenName {
		return fmt.Errorf("Grok Web 显示名未生效（上游回显 givenName=%q）", gotGiven)
	}
	return nil
}

func (a *Adapter) webBaseURL() string {
	baseURL := strings.TrimRight(a.config().BaseURL, "/")
	if baseURL == "" {
		return webOfficialBaseURL
	}
	return baseURL
}

// webServerActionUserName 从 Server Action 的 RSC 响应里挖出 givenName/familyName。
// 两种形态都要认：1:{"user":{...}} 与 1:{...裸 user 字段...}
func webServerActionUserName(body []byte) (string, string) {
	type nameFields struct {
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
		UserID     string `json:"userId"`
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		idx := bytes.IndexByte(line, '{')
		if idx < 0 {
			continue
		}
		var envelope map[string]json.RawMessage
		if json.NewDecoder(bytes.NewReader(line[idx:])).Decode(&envelope) != nil {
			continue
		}
		if userRaw, ok := envelope["user"]; ok {
			var fields nameFields
			if json.Unmarshal(userRaw, &fields) == nil {
				return fields.GivenName, fields.FamilyName
			}
			continue
		}
		var fields nameFields
		if json.Unmarshal(line[idx:], &fields) == nil &&
			(fields.UserID != "" || fields.GivenName != "" || fields.FamilyName != "") {
			return fields.GivenName, fields.FamilyName
		}
	}
	return "", ""
}

// webServerActionError 从 RSC 响应里取出 "error" 字段；没有就返回空。
// Server Action 的失败是 200 + {"error": "..."}，不会返回非 2xx。
func webServerActionError(body []byte) string {
	idx := bytes.Index(body, []byte(`"error"`))
	if idx < 0 {
		return ""
	}
	start := bytes.LastIndexByte(body[:idx], '{')
	if start < 0 {
		return accountSettingPreview(body)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(bytes.NewReader(body[start:])).Decode(&payload) != nil {
		return accountSettingPreview(body)
	}
	if strings.TrimSpace(payload.Error) == "" {
		return ""
	}
	return payload.Error
}

// SetBirthDate 设置 Grok Web 账号生日。上游接收 RFC3339 字符串；日期由应用层生成。
func (a *Adapter) SetBirthDate(ctx context.Context, credential account.Credential, birthDate time.Time) error {
	cfg := a.config()
	data, err := json.Marshal(map[string]string{
		"birthDate": birthDate.UTC().Format("2006-01-02") + "T16:00:00.000Z",
	})
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	err = a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:    baseURL + "/rest/auth/set-birth-date",
		body:        data,
		contentType: "application/json",
		origin:      baseURL,
		referer:     baseURL + "/",
		statsig:     true,
	})
	var upstreamErr *webAccountSettingError
	if errors.As(err, &upstreamErr) && upstreamErr.birthDateAlreadySet {
		return provider.ErrBirthDateAlreadySet
	}
	return err
}

// EnableNSFW 将上游内容偏好设置为 always_show_nsfw_content。
// 生日只负责年龄资料，NSFW 仍需独立调用本接口。
func (a *Adapter) EnableNSFW(ctx context.Context, credential account.Credential) error {
	cfg := a.config()
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return a.runWebAccountSetting(ctx, credential, webAccountSettingRequest{
		endpoint:    baseURL + "/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		body:        enableNSFWBody,
		contentType: "application/grpc-web+proto",
		origin:      baseURL,
		referer:     baseURL + "/",
		grpcWeb:     true,
		statsig:     true,
	})
}

type webAccountSettingRequest struct {
	endpoint    string
	body        []byte
	contentType string
	origin      string
	referer     string
	grpcWeb     bool
	connectES   bool
	statsig     bool
	clientHints bool
	withoutCF   bool
	// serverAction 非空时按 Next.js Server Action 发送（POST grok.com/ + Next-Action 头）。
	serverAction string
	// captureBody 非空时把 2xx 响应体回填出来（Server Action 的返回值在这里）。
	captureBody *[]byte
}

func (a *Adapter) runWebAccountSetting(ctx context.Context, credential account.Credential, input webAccountSettingRequest) error {
	return a.runWebAccountSettings(ctx, credential, input)
}

func (a *Adapter) runWebAccountSettings(ctx context.Context, credential account.Credential, inputs ...webAccountSettingRequest) error {
	// Console 账号用的也是 accounts.x.ai 的 sso（与 Web 共用同一个令牌），
	// 所以同样能调 grok.com 的资料设置。Build 是 OAuth，不能。
	if credential.AuthType != account.AuthTypeSSO {
		return fmt.Errorf("仅 SSO 账号支持资料设置")
	}
	if credential.Provider != account.ProviderWeb && credential.Provider != account.ProviderConsole {
		return fmt.Errorf("仅 Grok Web / Console 的 SSO 账号支持资料设置")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return provider.ErrUnauthorized
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return err
	}
	defer lease.Release()
	for _, input := range inputs {
		if err := a.executeWebAccountSetting(ctx, token, lease, input); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) executeWebAccountSetting(ctx context.Context, token string, lease *infraegress.Lease, input webAccountSettingRequest) error {
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, webAccountSettingTimeout)
		request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, input.endpoint, bytes.NewReader(input.body))
		if requestErr != nil {
			cancel()
			return requestErr
		}
		request.Header = buildHeaders(token, lease, input.contentType)
		if input.withoutCF {
			request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, ""))
		}
		applyAppHeaders(request.Header, input.origin, input.referer)
		if input.clientHints {
			browserheaders.ApplyChromiumClientHints(request.Header, lease.UserAgent)
		}
		if input.grpcWeb {
			request.Header.Set("x-grpc-web", "1")
		}
		if input.connectES {
			request.Header.Set("x-user-agent", "connect-es/2.1.1")
		}
		if input.serverAction != "" {
			request.Header.Set("Next-Action", input.serverAction)
			request.Header.Set("Next-Router-State-Tree", url.QueryEscape(webNextRouterStateTree))
		}
		if input.statsig {
			a.applySignedStatsig(requestCtx, request, token, lease)
		}

		response, requestErr := lease.Do(request)
		if requestErr != nil {
			cancel()
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, requestErr)
			return requestErr
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, webAccountSettingBodyLimit+1))
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return readErr
		}
		if len(body) > webAccountSettingBodyLimit {
			return fmt.Errorf("Grok Web 账号设置响应超过安全上限")
		}
		if response.StatusCode == http.StatusForbidden && input.statsig && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, input.endpoint) {
			continue
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		if response.StatusCode == http.StatusUnauthorized {
			return provider.ErrUnauthorized
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return newWebAccountSettingError(response.StatusCode, body)
		}
		if input.serverAction != "" {
			// Server Action 的失败是 200 + {"error": "..."}，必须自己看响应体
			if message := webServerActionError(body); message != "" {
				return newWebAccountSettingError(response.StatusCode, []byte(message))
			}
			if input.captureBody != nil {
				*input.captureBody = append([]byte(nil), body...)
			}
		}
		if input.grpcWeb {
			if err := validateAccountSettingGRPCStatus(response, body); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("Grok Web Statsig 刷新后仍被拒绝")
}

func validateAccountSettingGRPCStatus(response *http.Response, body []byte) error {
	_, bodyStatus, err := parseGRPCWebFrames(body)
	if err != nil {
		return fmt.Errorf("解析 Grok Web 账号设置响应: %w", err)
	}
	for _, status := range []string{
		response.Header.Get("grpc-status"),
		response.Trailer.Get("grpc-status"),
		bodyStatus,
	} {
		status = strings.TrimSpace(status)
		if status != "" && status != "0" {
			return fmt.Errorf("Grok Web 账号设置 gRPC 状态 %s", status)
		}
	}
	return nil
}

type webAccountSettingError struct {
	status              int
	body                string
	birthDateAlreadySet bool
}

func newWebAccountSettingError(status int, body []byte) *webAccountSettingError {
	return &webAccountSettingError{
		status:              status,
		body:                accountSettingPreview(body),
		birthDateAlreadySet: status == http.StatusTooManyRequests && bytes.Contains(body, []byte(webBirthDateLockedMarker)),
	}
}

func (e *webAccountSettingError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("Grok Web 账号设置上游返回 %d", e.status)
	}
	return fmt.Sprintf("Grok Web 账号设置上游返回 %d: %s", e.status, e.body)
}

func (e *webAccountSettingError) HTTPStatusCode() int { return e.status }

func accountSettingPreview(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	if len(value) <= webAccountSettingErrorPreview {
		return value
	}
	return value[:webAccountSettingErrorPreview]
}

var _ provider.WebAccountSettingsAdapter = (*Adapter)(nil)
