package registrar

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

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// 分类错误，便于传输层映射成合适的状态码。
var (
	ErrDisabled     = errors.New("注册机 sidecar 未启用")
	ErrUnauthorized = errors.New("注册机 sidecar 内部认证失败")
	ErrJobNotFound  = errors.New("注册机任务不存在")
)

// JobItem 是单个账号的注册结果，不包含任何凭据。
type JobItem struct {
	Email  string `json:"email"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Job 是 sidecar 返回的任务快照。密码与 SSO 永远不会出现在这里。
type Job struct {
	JobID      string    `json:"jobId"`
	Status     string    `json:"status"`
	Count      int       `json:"count"`
	Created    int       `json:"created"`
	Imported   int       `json:"imported"`
	Failed     int       `json:"failed"`
	StartedAt  int64     `json:"startedAt"`
	FinishedAt int64     `json:"finishedAt"`
	LastError  string    `json:"lastError"`
	Items      []JobItem `json:"items"`
}

// Client 是主程序调用注册机 sidecar 的 HTTP 客户端。
type Client struct {
	baseURL  string
	token    string
	maxCount int
	domains  []string
	http     *http.Client
}

// NewClient 按配置构造客户端；配置未启用时客户端所有调用都会返回 ErrDisabled。
func NewClient(value config.AccountRegistrarConfig, token string) *Client {
	timeout := value.Timeout.Value()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		baseURL:  strings.TrimRight(strings.TrimSpace(value.BaseURL), "/"),
		token:    token,
		maxCount: value.MaxCount,
		domains:  append([]string(nil), value.Domains...),
		http:     &http.Client{Timeout: timeout},
	}
}

// Enabled 表示 sidecar 地址与内部令牌都已就绪。
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != "" && c.token != ""
}

// MaxCount 返回允许的单次补号上限。
func (c *Client) MaxCount() int {
	if c == nil || c.maxCount < 1 {
		return 1
	}
	return c.maxCount
}

// Domains 返回下发给管理端的可选收信域名。
func (c *Client) Domains() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.domains...)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("注册机 sidecar 请求失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return ErrJobNotFound
	case resp.StatusCode >= http.StatusBadRequest:
		return fmt.Errorf("注册机 sidecar 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("注册机 sidecar 响应无法解析: %w", err)
	}
	return json.Unmarshal(envelope.Data, out)
}

// Health 探测 sidecar 是否可用。
func (c *Client) Health(ctx context.Context) (bool, error) {
	var payload struct {
		OK          bool `json:"ok"`
		Enabled     bool `json:"enabled"`
		AppDirReady bool `json:"appDirReady"`
	}
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, &payload); err != nil {
		return false, err
	}
	return payload.OK, nil
}

// StartJob 触发一次补号任务。
func (c *Client) StartJob(ctx context.Context, count int, domains []string) (Job, error) {
	var job Job
	body := map[string]any{"count": count}
	if len(domains) > 0 {
		body["domains"] = domains
	}
	err := c.do(ctx, http.MethodPost, "/jobs", body, &job)
	return job, err
}

// Job 查询任务快照。
func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var job Job
	err := c.do(ctx, http.MethodGet, "/jobs/"+url.PathEscape(id), nil, &job)
	return job, err
}

// Cancel 请求取消任务。
func (c *Client) Cancel(ctx context.Context, id string) (Job, error) {
	var job Job
	err := c.do(ctx, http.MethodPost, "/jobs/"+url.PathEscape(id)+"/cancel", map[string]any{}, &job)
	return job, err
}
