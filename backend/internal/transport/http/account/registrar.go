package account

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	infraregistrar "github.com/chenyme/grok2api/backend/internal/infra/registrar"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

// WithRegistrar 注入注册机 sidecar 客户端；nil 表示未部署。
func (h *Handler) WithRegistrar(client *infraregistrar.Client) *Handler {
	h.registrar = client
	return h
}

// RegisterRegistrar 注册管理端的「自动补号」入口。
func (h *Handler) RegisterRegistrar(router *gin.RouterGroup) {
	router.GET("/accounts/registrar/status", h.registrarStatus)
	router.POST("/accounts/registrar/jobs", h.registrarStartJob)
	router.GET("/accounts/registrar/jobs/:jobId", h.registrarJob)
	router.POST("/accounts/registrar/jobs/:jobId/cancel", h.registrarCancelJob)
}

// RegisterRegistrarInternal 注册 sidecar 回传凭据的内部入口。
func (h *Handler) RegisterRegistrarInternal(router *gin.RouterGroup) {
	router.POST("/accounts", h.registrarImportAccount)
}

func (h *Handler) registrarStatus(c *gin.Context) {
	available := false
	if h.registrar.Enabled() {
		if ok, err := h.registrar.Health(c.Request.Context()); err == nil && ok {
			available = true
		}
	}
	response.Success(c, http.StatusOK, gin.H{
		"available": available,
		"enabled":   h.registrar.Enabled(),
		"maxCount":  h.registrar.MaxCount(),
		"domains":   h.registrar.Domains(),
	})
}

type registrarJobRequest struct {
	Count   int      `json:"count"`
	Domains []string `json:"domains"`
}

func (h *Handler) registrarStartJob(c *gin.Context) {
	var request registrarJobRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "补号请求无效")
		return
	}
	if !h.registrar.Enabled() {
		response.Error(c, http.StatusServiceUnavailable, "registrarUnavailable", "注册机 sidecar 未启用")
		return
	}
	if request.Count < 1 || request.Count > h.registrar.MaxCount() {
		response.Error(c, http.StatusBadRequest, "invalidRequest",
			fmt.Sprintf("补号数量必须在 1 到 %d 之间", h.registrar.MaxCount()))
		return
	}
	job, err := h.registrar.StartJob(c.Request.Context(), request.Count, sanitizeRegistrarDomains(request.Domains))
	if err != nil {
		writeRegistrarError(c, err)
		return
	}
	response.Success(c, http.StatusAccepted, job)
}

func (h *Handler) registrarJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "任务标识无效")
		return
	}
	job, err := h.registrar.Job(c.Request.Context(), jobID)
	if err != nil {
		writeRegistrarError(c, err)
		return
	}
	response.Success(c, http.StatusOK, job)
}

func (h *Handler) registrarCancelJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "任务标识无效")
		return
	}
	job, err := h.registrar.Cancel(c.Request.Context(), jobID)
	if err != nil {
		writeRegistrarError(c, err)
		return
	}
	response.Success(c, http.StatusOK, job)
}

func writeRegistrarError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, infraregistrar.ErrDisabled):
		response.Error(c, http.StatusServiceUnavailable, "registrarUnavailable", "注册机 sidecar 未启用")
	case errors.Is(err, infraregistrar.ErrUnauthorized):
		response.Error(c, http.StatusBadGateway, "registrarUnauthorized", "注册机 sidecar 内部认证失败")
	case errors.Is(err, infraregistrar.ErrJobNotFound):
		response.Error(c, http.StatusNotFound, "registrarJobNotFound", "补号任务不存在")
	default:
		response.Error(c, http.StatusBadGateway, "registrarRequestFailed", err.Error())
	}
}

func sanitizeRegistrarDomains(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

type registrarImportRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	SSO      string `json:"sso"`
}

// registrarImportAccount 接收 sidecar 注册产出的凭据并走既有 Web 账号导入链路。
// 响应里只回统计信息，绝不回显任何凭据。
func (h *Handler) registrarImportAccount(c *gin.Context) {
	var request registrarImportRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "凭据格式无效")
		return
	}
	email := strings.TrimSpace(request.Email)
	sso := strings.TrimSpace(request.SSO)
	if email == "" || sso == "" {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "缺少邮箱或 SSO")
		return
	}
	document := []byte(fmt.Sprintf("%s----%s----%s\n", email, strings.TrimSpace(request.Password), sso))
	result, err := h.service.ImportWebCredentialDocumentsWithProgress(c.Request.Context(), [][]byte{document}, nil, nil)
	if err != nil {
		// 不回传错误细节，避免把凭据带进响应或审计日志。
		_ = c.Error(err)
		response.Success(c, http.StatusOK, gin.H{
			"imported": false,
			"reason":   "import failed",
			"created":  0,
			"updated":  0,
			"skipped":  0,
		})
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"imported": result.Created > 0 || result.Updated > 0,
		"created":  result.Created,
		"updated":  result.Updated,
		"skipped":  result.Skipped,
	})
}
