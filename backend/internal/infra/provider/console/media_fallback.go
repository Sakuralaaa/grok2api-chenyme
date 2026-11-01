package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Console 免费账号的 /v1/images/generations 与 /v1/images/edits 会按请求随机返回
// 402 personal-team-blocked:spending-limit（2026-11 实测：同一账号、同一请求约 20%~30%
// 成功，失败不扣额度）。因此先在同一账号上重试标准图片接口，以保留 resolution、
// quality、aspect_ratio 等参数；仍失败时退回 Playground 同款链路：/v1/responses +
// image_generation 工具（由对话模型调用生图工具，稳定可用，但只能用提示词控制比例与张数）。
const (
	consoleImageSpendingLimitAttempts = 6
	consoleImageToolModel             = "grok-4.20-0309-non-reasoning"
	consoleImageToolBodyLimit         = 48 << 20
)

var consoleImageRetryDelays = [...]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 750 * time.Millisecond, time.Second}

type consoleImageFallback struct {
	prompt      string
	aspectRatio string
	count       int
	inputImages []string
}

// forwardConsoleImage 包装 forwardConsoleMedia：随机 402 时重试，最终退回 image_generation 工具。
func (a *Adapter) forwardConsoleImage(ctx context.Context, credential account.Credential, path string, payload any, format string, count int, fallback consoleImageFallback) (*provider.Response, error) {
	var last *provider.Response
	for attempt := 0; attempt < consoleImageSpendingLimitAttempts; attempt++ {
		result, err := a.forwardConsoleMedia(ctx, credential, path, payload, format, count)
		if err != nil {
			return nil, err
		}
		if !isConsoleImageSpendingLimitResponse(result) {
			return result, nil
		}
		last = result
		if attempt+1 < consoleImageSpendingLimitAttempts {
			if err := sleepConsoleImageRetry(ctx, attempt); err != nil {
				return nil, err
			}
		}
	}
	a.logger().Info("console_image_tool_fallback", "account_id", credential.ID, "path", path, "attempts", consoleImageSpendingLimitAttempts)
	toolResult, err := a.generateConsoleImageViaTool(ctx, credential, format, fallback)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		a.logger().Warn("console_image_tool_fallback_failed", "account_id", credential.ID, "error", err)
		return last, nil
	}
	return toolResult, nil
}

func sleepConsoleImageRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(consoleImageRetryDelays[min(attempt, len(consoleImageRetryDelays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isConsoleImageSpendingLimitResponse 识别图片接口的随机 402；body 已被 forwardConsoleMedia
// 读入内存，这里读取后再放回，保证调用方仍能拿到原始错误体。
func isConsoleImageSpendingLimitResponse(result *provider.Response) bool {
	if result == nil || result.StatusCode != http.StatusPaymentRequired {
		return false
	}
	var data []byte
	if result.Diagnostic != nil && len(result.Diagnostic.Body) > 0 {
		data = result.Diagnostic.Body
	} else if result.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(result.Body, 64<<10))
		_ = result.Body.Close()
		if err != nil {
			return false
		}
		result.Body = io.NopCloser(bytes.NewReader(raw))
		data = raw
	}
	return isConsoleSpendingLimitBody(data)
}

func isConsoleSpendingLimitBody(data []byte) bool {
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &payload) == nil && strings.TrimSpace(payload.Code) != "" {
		return strings.EqualFold(strings.TrimSpace(payload.Code), "personal-team-blocked:spending-limit")
	}
	return bytes.Contains(data, []byte("personal-team-blocked:spending-limit"))
}

func buildConsoleImageToolPrompt(fallback consoleImageFallback) string {
	var builder strings.Builder
	builder.WriteString("Use the image generation tool")
	if len(fallback.inputImages) > 0 {
		builder.WriteString(" to edit the attached image")
		if len(fallback.inputImages) > 1 {
			builder.WriteString("s")
		}
	}
	builder.WriteString(". Pass the user's prompt to the tool as faithfully as possible and do not reply with text only.")
	if fallback.count > 1 {
		fmt.Fprintf(&builder, " Generate %d different images (%d separate generations).", fallback.count, fallback.count)
	}
	if ratio := strings.TrimSpace(fallback.aspectRatio); ratio != "" && ratio != "auto" {
		fmt.Fprintf(&builder, " Use aspect ratio %s.", ratio)
	}
	builder.WriteString("\nPrompt: ")
	builder.WriteString(fallback.prompt)
	return builder.String()
}

func buildConsoleImageToolPayload(fallback consoleImageFallback) map[string]any {
	text := buildConsoleImageToolPrompt(fallback)
	var input any = text
	if len(fallback.inputImages) > 0 {
		content := []map[string]any{{"type": "input_text", "text": text}}
		for _, image := range fallback.inputImages {
			content = append(content, map[string]any{"type": "input_image", "image_url": image})
		}
		input = []map[string]any{{"role": "user", "content": content}}
	}
	return map[string]any{
		"model":       consoleImageToolModel,
		"input":       input,
		"stream":      false,
		"store":       false,
		"tools":       []map[string]any{{"type": "image_generation"}},
		"tool_choice": "required",
	}
}

// parseConsoleImageToolOutput 从 /v1/responses 的 output 中取出 image_generation_call.result（base64 JPEG）。
func parseConsoleImageToolOutput(data []byte) ([]string, error) {
	var envelope struct {
		Output []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Result string `json:"result"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("解析 Console 生图工具响应: %w", err)
	}
	images := make([]string, 0, 1)
	for _, item := range envelope.Output {
		if item.Type != "image_generation_call" || strings.TrimSpace(item.Result) == "" {
			continue
		}
		images = append(images, strings.TrimSpace(item.Result))
	}
	if len(images) == 0 {
		return nil, errors.New("Console 生图工具未返回图片")
	}
	return images, nil
}

func (a *Adapter) generateConsoleImageViaTool(ctx context.Context, credential account.Credential, format string, fallback consoleImageFallback) (*provider.Response, error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(buildConsoleImageToolPayload(fallback))
	if err != nil {
		return nil, err
	}
	cfg := a.config()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	lease, err := a.egress.AcquireCredential(requestCtx, egressdomain.ScopeConsole, credential)
	if err != nil {
		return nil, err
	}
	response, err := a.doDPoPRequest(requestCtx, credential, token, lease, http.MethodPost, consoleV1Endpoint(cfg.BaseURL, "/responses"), body, "application/json")
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, consoleImageToolBodyLimit+1))
	_ = response.Body.Close()
	a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, readErr)
	lease.Release()
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > consoleImageToolBodyLimit {
		return nil, errors.New("Console 生图工具响应过大")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result := jsonRawProviderResponse(response.StatusCode, data)
		result.Diagnostic = &provider.DiagnosticResponse{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header.Clone(), Body: data}
		return result, nil
	}
	images, err := parseConsoleImageToolOutput(data)
	if err != nil {
		return nil, err
	}
	if fallback.count > 0 && len(images) > fallback.count {
		images = images[:fallback.count]
	}
	items := make([]map[string]any, 0, len(images))
	for _, encoded := range images {
		item := map[string]any{"revised_prompt": fallback.prompt}
		if format == "url" {
			raw, decodeErr := base64.StdEncoding.DecodeString(encoded)
			if decodeErr != nil {
				return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, fmt.Errorf("解码 Console 生图工具图片: %w", decodeErr))
			}
			if a.assets == nil {
				return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("图片媒体存储未配置"))
			}
			asset, saveErr := a.saveConsoleImage(ctx, raw)
			if saveErr != nil {
				return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, saveErr)
			}
			item["url"] = a.assets.PublicImageURL(asset.ID)
			item["mime_type"] = asset.MIMEType
		} else {
			item["b64_json"] = encoded
			item["mime_type"] = "image/jpeg"
		}
		items = append(items, item)
	}
	result := jsonProviderResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": items})
	result.QuotaUnits = max(1, len(items))
	result.Header.Set("X-Grok2API-Console-Image-Path", "image_generation_tool")
	return result, nil
}

func jsonRawProviderResponse(status int, data []byte) *provider.Response {
	result := jsonProviderResponse(status, nil)
	result.Body = io.NopCloser(bytes.NewReader(data))
	result.Header.Set("Content-Length", fmt.Sprint(len(data)))
	return result
}
