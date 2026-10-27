package account

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	webMinimumAge               = 20
	webMaximumAge               = 40
	webAccountStateWriteTimeout = 5 * time.Second
)

var (
	webFirstNames = []string{
		"James", "John", "Robert", "Michael", "William", "David", "Richard", "Joseph",
		"Thomas", "Charles", "Mary", "Patricia", "Jennifer", "Linda", "Elizabeth",
		"Ethan", "Noah", "Liam", "Mason", "Lucas", "Owen", "Caleb", "Ryan",
	}
	webLastNames = []string{
		"Smith", "Johnson", "Williams", "Brown", "Jones", "Miller", "Davis", "Wilson",
		"Brooks", "Turner", "Foster", "Hayes", "Bennett", "Morgan", "Reed", "Cole",
	}
)

// webDisplayNameAdapter 是可选能力：读取/设置上游显示名（全名）。
// 用可选接口而不是扩展 provider 接口，避免所有实现与测试桩都要跟着改。
type webDisplayNameAdapter interface {
	DisplayName(ctx context.Context, credential accountdomain.Credential) (string, string, error)
	SetDisplayName(ctx context.Context, credential accountdomain.Credential, givenName, familyName string) error
}

// AcceptWebTerms 接受指定 Grok Web SSO 账号的上游服务协议。
func (s *Service) AcceptWebTerms(ctx context.Context, id uint64) error {
	return s.runSingleWebAccountScript(ctx, id, WebAccountScriptOptions{AcceptTerms: true})
}

// SetWebBirthDate 为指定 Grok Web SSO 账号生成并设置 20–40 岁的随机生日。
func (s *Service) SetWebBirthDate(ctx context.Context, id uint64) error {
	return s.runSingleWebAccountScript(ctx, id, WebAccountScriptOptions{SetBirthDate: true})
}

// randomWebBirthDate 在所有可形成 20–40 周岁年龄的自然日中均匀取值。
func randomWebBirthDate(now time.Time, source io.Reader) (time.Time, error) {
	if source == nil {
		return time.Time{}, fmt.Errorf("随机源不能为空")
	}
	earliest, latest := webBirthDateRange(now)
	dayCount := int64(latest.Sub(earliest)/(24*time.Hour)) + 1
	if dayCount <= 0 {
		return time.Time{}, fmt.Errorf("随机生日范围无效")
	}
	offset, err := cryptorand.Int(source, big.NewInt(dayCount))
	if err != nil {
		return time.Time{}, err
	}
	return earliest.AddDate(0, 0, int(offset.Int64())), nil
}

func webBirthDateRange(now time.Time) (time.Time, time.Time) {
	// 生日是纯日期：以调用方所在时区取“今天”，再用 UTC 承载该日历值，避免 DST 干扰天数计算。
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// 20 岁生日当天仍属于范围；41 岁生日当天已超出范围，因此最早日期需后移一天。
	earliest := webDateYearsAgo(today, webMaximumAge+1).AddDate(0, 0, 1)
	latest := webDateYearsAgo(today, webMinimumAge)
	return earliest, latest
}

// webDateYearsAgo 将闰日夹紧到目标年份当月的最后一天，避免 AddDate 的跨月归一化。
func webDateYearsAgo(today time.Time, years int) time.Time {
	year := today.Year() - years
	day := today.Day()
	lastDay := time.Date(year, today.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, today.Month(), day, 0, 0, 0, 0, time.UTC)
}

// EnableWebNSFW 为指定 Grok Web SSO 账号设置随机生日后开启成人内容显示偏好。
func (s *Service) EnableWebNSFW(ctx context.Context, id uint64) error {
	return s.runSingleWebAccountScript(ctx, id, WebAccountScriptOptions{EnableNSFW: true})
}

func (s *Service) runWebAccountScript(ctx context.Context, id uint64, options WebAccountScriptOptions) error {
	options, err := normalizeWebAccountScriptOptions(options)
	if err != nil {
		return err
	}
	release, err := s.acquireWebAccountScriptLock(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	credential, adapter, err := s.webAccountSettings(ctx, id)
	if err != nil {
		return err
	}
	options = pendingWebAccountScriptOptions(credential, options)
	if !options.AcceptTerms && !options.SetBirthDate && !options.EnableNSFW && !options.SetName {
		return nil
	}
	if options.AcceptTerms {
		if err := s.runWebAccountSetting(ctx, credential, "接受服务协议", func() error {
			return adapter.AcceptTerms(ctx, credential)
		}); err != nil {
			return err
		}
		if err := s.recordWebTermsAccepted(ctx, credential.ID); err != nil {
			return err
		}
	}
	if options.SetName {
		if err := s.setWebDisplayName(ctx, credential, adapter); err != nil {
			return err
		}
	}
	if options.SetBirthDate {
		if err := s.setRandomWebBirthDate(ctx, credential, adapter); err != nil {
			return err
		}
	}
	if options.EnableNSFW {
		if err := s.runWebAccountSetting(ctx, credential, "开启 NSFW", func() error {
			return adapter.EnableNSFW(ctx, credential)
		}); err != nil {
			return err
		}
		if err := s.recordWebAccountState(ctx, credential.ID, "NSFW", s.accounts.MarkWebNSFWEnabled); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) setWebDisplayName(ctx context.Context, credential accountdomain.Credential, adapter provider.WebAccountSettingsAdapter) error {
	setter, ok := adapter.(webDisplayNameAdapter)
	if !ok {
		return fmt.Errorf("%w: 当前 Provider 不支持设置显示名", ErrUnsupported)
	}
	return s.runWebAccountSetting(ctx, credential, "设置显示名", func() error {
		given, _, err := setter.DisplayName(ctx, credential)
		if err != nil {
			return err
		}
		if strings.TrimSpace(given) != "" {
			// 上游已经有名字：不覆盖，天然幂等
			return nil
		}
		firstName, lastName, err := randomWebDisplayName(cryptorand.Reader)
		if err != nil {
			return err
		}
		return setter.SetDisplayName(ctx, credential, firstName, lastName)
	})
}

// randomWebDisplayName 随机生成一个常见英文全名。
func randomWebDisplayName(source io.Reader) (string, string, error) {
	first, err := pickWebNamePart(source, webFirstNames)
	if err != nil {
		return "", "", fmt.Errorf("生成显示名: %w", err)
	}
	last, err := pickWebNamePart(source, webLastNames)
	if err != nil {
		return "", "", fmt.Errorf("生成显示名: %w", err)
	}
	return first, last, nil
}

func pickWebNamePart(source io.Reader, values []string) (string, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("名字候选为空")
	}
	index, err := cryptorand.Int(source, big.NewInt(int64(len(values))))
	if err != nil {
		return "", err
	}
	return values[index.Int64()], nil
}

func pendingWebAccountScriptOptions(credential accountdomain.Credential, options WebAccountScriptOptions) WebAccountScriptOptions {
	if credential.WebTermsAcceptedAt != nil && credential.WebTermsAcceptedVersion >= accountdomain.CurrentWebTermsVersion {
		options.AcceptTerms = false
	}
	birthDateRecorded := credential.WebBirthDateSetAt != nil || credential.WebNSFWEnabledAt != nil
	if birthDateRecorded {
		options.SetBirthDate = false
	}
	if credential.WebNSFWEnabledAt != nil {
		options.EnableNSFW = false
	}
	if options.EnableNSFW && !birthDateRecorded {
		options.SetBirthDate = true
	}
	return options
}

func (s *Service) recordWebTermsAccepted(ctx context.Context, id uint64) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webAccountStateWriteTimeout)
	err := s.accounts.MarkWebTermsAccepted(writeCtx, id, accountdomain.CurrentWebTermsVersion, s.now().UTC())
	cancel()
	if err != nil {
		return fmt.Errorf("记录服务协议状态: %w", mapRepositoryError(err))
	}
	return nil
}

func (s *Service) setRandomWebBirthDate(ctx context.Context, credential accountdomain.Credential, adapter provider.WebAccountSettingsAdapter) error {
	birthDate, err := randomWebBirthDate(s.now().In(time.Local), cryptorand.Reader)
	if err != nil {
		return fmt.Errorf("生成随机生日: %w", err)
	}
	err = s.runWebAccountSetting(ctx, credential, "设置生日", func() error {
		return adapter.SetBirthDate(ctx, credential, birthDate)
	})
	if err != nil && !errors.Is(err, provider.ErrBirthDateAlreadySet) {
		return err
	}
	return s.recordWebAccountState(ctx, credential.ID, "生日", s.accounts.MarkWebBirthDateSet)
}

func (s *Service) recordWebAccountState(ctx context.Context, id uint64, state string, mark func(context.Context, uint64, time.Time) error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webAccountStateWriteTimeout)
	err := mark(writeCtx, id, s.now().UTC())
	cancel()
	if err != nil {
		return fmt.Errorf("记录%s状态: %w", state, mapRepositoryError(err))
	}
	return nil
}

func (s *Service) webAccountSettings(ctx context.Context, id uint64) (accountdomain.Credential, provider.WebAccountSettingsAdapter, error) {
	credential, err := s.accounts.Get(ctx, id)
	if err != nil {
		return accountdomain.Credential{}, nil, mapRepositoryError(err)
	}
	if credential.AuthType != accountdomain.AuthTypeSSO ||
		(credential.Provider != accountdomain.ProviderWeb &&
			credential.Provider != accountdomain.ProviderConsole) {
		return accountdomain.Credential{}, nil, fmt.Errorf("%w: 仅 Grok Web / Console 的 SSO 账号支持该操作", ErrUnsupported)
	}
	if s.providers == nil {
		return accountdomain.Credential{}, nil, fmt.Errorf("Grok Web Provider 未注册")
	}
	adapter, ok := s.providers.WebAccountSettings()
	if !ok {
		return accountdomain.Credential{}, nil, fmt.Errorf("%w: Grok Web Provider 不支持账号资料设置", ErrUnsupported)
	}
	return credential, adapter, nil
}

func (s *Service) runWebAccountSetting(ctx context.Context, credential accountdomain.Credential, operation string, run func() error) error {
	err := run()
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		err = errors.Join(err, s.markSSOCredentialRejected(ctx, credential, "Grok Web SSO credential rejected"))
	}
	return fmt.Errorf("%s: %w", operation, err)
}
