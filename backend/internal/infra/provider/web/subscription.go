package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// subscriptionTierRank mirrors grok.com getSubscriptionLevel: among ACTIVE
// subscriptions the highest-ranked tier is the account's plan.
var subscriptionTierRank = map[string]int{
	"SUBSCRIPTION_TIER_INVALID":         0,
	"SUBSCRIPTION_TIER_X_BASIC":         1,
	"SUBSCRIPTION_TIER_X_PREMIUM":       2,
	"SUBSCRIPTION_TIER_X_PREMIUM_PLUS":  3,
	"SUBSCRIPTION_TIER_SUPER_GROK_LITE": 4,
	"SUBSCRIPTION_TIER_GROK_PRO":        5, // SuperGrok
	"SUBSCRIPTION_TIER_SUPER_GROK_PLUS": 6,
	"SUBSCRIPTION_TIER_SUPER_GROK_PRO":  7, // SuperGrok Heavy (grok.com hasMaxTierSubscription)
}

type webSubscriptionsResponse struct {
	Subscriptions []struct {
		Tier   string `json:"tier"`
		Status string `json:"status"`
	} `json:"subscriptions"`
}

// webTierFromSubscriptions maps the upstream plan to a routing tier. ok=false
// means the plan is real but its model entitlements are not known (SuperGrok
// Lite, X Premium); callers then fall back to the quota-shape heuristic instead
// of guessing.
func webTierFromSubscriptions(body []byte) (account.WebTier, bool, error) {
	var value webSubscriptionsResponse
	if err := json.Unmarshal(body, &value); err != nil {
		return account.WebTierAuto, false, fmt.Errorf("解析 Grok Web 订阅: %w", err)
	}
	best, bestRank := "", -1
	for _, item := range value.Subscriptions {
		if item.Status != "SUBSCRIPTION_STATUS_ACTIVE" {
			continue
		}
		rank, known := subscriptionTierRank[item.Tier]
		if !known {
			// A tier newer than this build: do not downgrade or promote on it.
			return account.WebTierAuto, false, nil
		}
		if rank > bestRank {
			best, bestRank = item.Tier, rank
		}
	}
	switch best {
	case "", "SUBSCRIPTION_TIER_INVALID", "SUBSCRIPTION_TIER_X_BASIC":
		return account.WebTierBasic, true, nil
	case "SUBSCRIPTION_TIER_SUPER_GROK_PRO":
		return account.WebTierHeavy, true, nil
	case "SUBSCRIPTION_TIER_GROK_PRO", "SUBSCRIPTION_TIER_SUPER_GROK_PLUS", "SUBSCRIPTION_TIER_X_PREMIUM_PLUS":
		return account.WebTierSuper, true, nil
	default:
		return account.WebTierAuto, false, nil
	}
}

// syncSubscriptionTier reads the account's real plan from grok.com
// /rest/subscriptions. Any failure is returned so the caller can fall back to
// the quota-shape heuristic; it never blocks a quota refresh on its own.
func (a *Adapter) syncSubscriptionTier(ctx context.Context, credential account.Credential) (account.WebTier, bool, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.WebTierAuto, false, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return account.WebTierAuto, false, err
	}
	defer lease.Release()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.QuotaTimeoutSeconds)*time.Second)
	defer cancel()
	endpoint := cfg.BaseURL + "/rest/subscriptions"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return account.WebTierAuto, false, err
	}
	request.Header = buildHeaders(token, lease, "application/json")
	applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/")
	a.applySignedStatsig(requestCtx, request, token, lease)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return account.WebTierAuto, false, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if err != nil {
		return account.WebTierAuto, false, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		return account.WebTierAuto, false, provider.ErrUnauthorized
	}
	if response.StatusCode == http.StatusForbidden && provider.IsDefinitiveAccountBlockBody(body) {
		return account.WebTierAuto, false, fmt.Errorf("%w: account blocked", provider.ErrUnauthorized)
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return account.WebTierAuto, false, fmt.Errorf("Grok Web 订阅接口返回 %d", response.StatusCode)
	}
	return webTierFromSubscriptions(body)
}
