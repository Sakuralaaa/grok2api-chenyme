package registrar

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

const bootstrapVersion = 1

// StateDirEnv 让部署方覆写 sidecar 共享状态目录。
const StateDirEnv = "GROK2API_ACCOUNT_REGISTRAR_DIR"

const defaultStateDir = "/var/lib/grok2api-account-registrar"

type bootstrapFile struct {
	Version       int             `json:"version"`
	Enabled       bool            `json:"enabled"`
	InternalToken string          `json:"internal_token,omitempty"`
	Config        bootstrapConfig `json:"config"`
}

type bootstrapConfig struct {
	BaseURL           string   `json:"base_url"`
	AppDir            string   `json:"app_dir"`
	Domains           []string `json:"domains"`
	Proxy             string   `json:"proxy"`
	GeoIPIP           string   `json:"geoip_ip"`
	MaxCount          int      `json:"max_count"`
	JobTimeoutSeconds int      `json:"job_timeout_seconds"`
}

// StateDir 返回主程序与注册机 sidecar 共享的状态目录。
func StateDir() string {
	if value := strings.TrimSpace(os.Getenv(StateDirEnv)); value != "" {
		return value
	}
	return defaultStateDir
}

// Prepare 写出 sidecar 的 bootstrap 文件，并返回服务端认可的内部令牌。
// 令牌由 jwtSecret 做域分离派生，不经任何 API 暴露；未启用时返回空串。
func Prepare(path string, value config.AccountRegistrarConfig, jwtSecret string) (string, error) {
	if !value.Enabled {
		return "", nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("accountRegistrar 已启用，但未配置内部 bootstrap 文件路径")
	}
	token := ""
	if value.Enabled {
		token = deriveToken(jwtSecret)
	}
	payload := bootstrapFile{
		Version:       bootstrapVersion,
		Enabled:       value.Enabled,
		InternalToken: token,
		Config: bootstrapConfig{
			BaseURL:           strings.TrimSpace(value.CallbackURL),
			AppDir:            strings.TrimSpace(value.AppDir),
			Domains:           append([]string(nil), value.Domains...),
			Proxy:             strings.TrimSpace(value.Proxy),
			GeoIPIP:           strings.TrimSpace(value.GeoIPIP),
			MaxCount:          value.MaxCount,
			JobTimeoutSeconds: int(value.JobTimeout.Value().Seconds()),
		},
	}
	if err := writeAtomic(path, payload); err != nil {
		return "", fmt.Errorf("写入注册机 bootstrap: %w", err)
	}
	return token, nil
}

// deriveToken 与质量守护令牌用不同的域分离前缀，互不复用。
func deriveToken(jwtSecret string) string {
	mac := hmac.New(sha256.New, []byte(jwtSecret))
	mac.Write([]byte("grok2api/account-registrar/internal/v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func writeAtomic(path string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bootstrap-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
