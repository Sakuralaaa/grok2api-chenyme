package gateway

import (
	"context"
	"net/http"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// consoleImageTransientHeader 与 console.ConsoleImageTransientHeader 保持一致：
// Console 适配器在标准图片接口与全部生图工具降级都失败（非额度原因）时设置。
const consoleImageTransientHeader = "X-Grok2API-Console-Image-Transient"

// consoleImageTransientCooldown 只短暂隔离该账号，避免同一请求和紧随其后的请求再次命中。
const consoleImageTransientCooldown = 2 * time.Minute

// handleConsoleImageAccountFailure 记录 Console 生图在本账号上的最终失败，并报告是否应换号重试。
// Console 适配器内部已经完成了随机 402 重试与 image_generation 工具降级，所以到这里的：
//   - 402：工具链路也被 spending-limit 拒绝，账号周额度真实耗尽 → 暂停账号；
//   - 429：限流或未开通 Key → 软冷却；
//   - 503 + 暂时不可用标记：所有链路都没出图 → 软冷却。
//
// 三种情况都换下一个账号继续。其它状态（含 400 内容审核）原样返回给客户端。
func (s *Service) handleConsoleImageAccountFailure(ctx context.Context, requestID string, credential accountdomain.Credential, response *provider.Response) bool {
	if credential.Provider != accountdomain.ProviderConsole || response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusPaymentRequired:
		if err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{}); err != nil {
			s.logger.Warn("console_image_spending_limit_pause_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		}
		s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
		s.logger.Info("console_image_account_rotate", "request_id", requestID, "account_id", credential.ID, "reason", "spending_limit")
		return true
	case http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
		if err := s.selector.markSoftFailure(ctx, credential, response.StatusCode, retryAfter); err != nil {
			s.logger.Warn("console_image_soft_cooldown_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		}
		s.logger.Info("console_image_account_rotate", "request_id", requestID, "account_id", credential.ID, "reason", "rate_limited")
		return true
	case http.StatusServiceUnavailable:
		if response.Header.Get(consoleImageTransientHeader) == "" {
			return false
		}
		if err := s.selector.markSoftFailure(ctx, credential, response.StatusCode, consoleImageTransientCooldown); err != nil {
			s.logger.Warn("console_image_soft_cooldown_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		}
		s.logger.Info("console_image_account_rotate", "request_id", requestID, "account_id", credential.ID, "reason", "image_unavailable")
		return true
	}
	return false
}
