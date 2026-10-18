package gateway

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// generationTiming 只记录阶段耗时和有限枚举，不保存请求体、凭据或会话键。
type generationTiming struct {
	mu             sync.Mutex
	started        time.Time
	route          string
	provider       accountdomain.Provider
	selectionWait  time.Duration
	credentialWait time.Duration
	upstreamWait   time.Duration
	firstHeaders   time.Duration
	firstBody      time.Duration
	attempts       int
	finished       bool
	// Request-scoped diagnostics (identifiers and enums only).
	requestID     string
	accountID     uint64
	accountsTried []uint64
	errorCode     string
	searchStrikes int
	searchRate    float64
	searchSamples int
	streaming     bool
}

// setRequest records the request id and streaming flag for diagnostics.
func (t *generationTiming) setRequest(requestID string, streaming bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.requestID, t.streaming = requestID, streaming
	t.mu.Unlock()
}

// markAccount records an account the request was sent to. The last one is the
// account that served (or finally failed) the request.
func (t *generationTiming) markAccount(accountID uint64) {
	if t == nil || accountID == 0 {
		return
	}
	t.mu.Lock()
	t.accountID = accountID
	if n := len(t.accountsTried); (n == 0 || t.accountsTried[n-1] != accountID) && n < 16 {
		t.accountsTried = append(t.accountsTried, accountID)
	}
	t.mu.Unlock()
}

// setOutcomeDetail records the terminal error code and the serving account's
// recent search-health snapshot.
func (t *generationTiming) setOutcomeDetail(errorCode string, strikes int, rate float64, samples int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.errorCode, t.searchStrikes, t.searchRate, t.searchSamples = errorCode, strikes, rate, samples
	t.mu.Unlock()
}

func newGenerationTiming(route string, provider accountdomain.Provider) *generationTiming {
	return &generationTiming{started: time.Now(), route: route, provider: provider}
}

func (t *generationTiming) markSelection(duration time.Duration) {
	t.mu.Lock()
	t.selectionWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markCredential(duration time.Duration) {
	t.mu.Lock()
	t.credentialWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markUpstream(duration time.Duration) {
	t.mu.Lock()
	t.attempts++
	t.upstreamWait += duration
	if t.firstHeaders == 0 {
		t.firstHeaders = time.Since(t.started)
	}
	t.mu.Unlock()
}

func (t *generationTiming) markFirstBody() {
	t.mu.Lock()
	if t.firstBody == 0 {
		t.firstBody = time.Since(t.started)
	}
	t.mu.Unlock()
}

func (t *generationTiming) finish(logger *slog.Logger, outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	total := time.Since(t.started)
	retries := max(0, t.attempts-1)
	fields := []any{
		"route", t.route, "provider", t.provider, "outcome", outcome, "total_ms", total.Milliseconds(),
		"selection_wait_ms", t.selectionWait.Milliseconds(), "credential_wait_ms", t.credentialWait.Milliseconds(),
		"upstream_wait_ms", t.upstreamWait.Milliseconds(), "first_headers_ms", t.firstHeaders.Milliseconds(),
		"first_body_ms", t.firstBody.Milliseconds(), "attempts", t.attempts, "retries", retries,
	}
	diagnostic := t.requestID != "" || t.accountID != 0
	if diagnostic {
		fields = append(fields, "request_id", t.requestID, "streaming", t.streaming, "account_id", t.accountID, "accounts_tried", t.accountsTried)
		if t.errorCode != "" {
			fields = append(fields, "error_code", t.errorCode)
		}
		if t.searchSamples > 0 || t.searchStrikes > 0 {
			fields = append(fields, "search_strikes", t.searchStrikes, "search_success_rate", t.searchRate, "search_samples", t.searchSamples)
		}
	}
	t.mu.Unlock()
	labels := perfmetrics.Labels{Subsystem: "gateway", Provider: string(t.provider), Outcome: outcome}
	perfmetrics.Default.ObserveDuration("request_duration_us", labels, total)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "selection"), t.selectionWait)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "credential"), t.credentialWait)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "upstream"), t.upstreamWait)
	perfmetrics.Default.Add("attempt_count", labels, int64(t.attempts))
	if logger == nil {
		logger = slog.Default()
	}
	if diagnostic {
		// One structured line per request so operators can trace routing,
		// retries and the outcome without enabling debug logging.
		logger.Info("request_diagnostics", fields...)
		return
	}
	logger.Debug("generation_timing", fields...)
}

func withTimingStage(labels perfmetrics.Labels, stage string) perfmetrics.Labels {
	labels.Stage = stage
	return labels
}

type firstByteReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

type firstTokenTimer struct {
	started time.Time
	once    sync.Once
	encoded atomic.Int64
}

func newFirstTokenTimer(started time.Time) *firstTokenTimer {
	return &firstTokenTimer{started: started}
}

func (t *firstTokenTimer) mark() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		elapsedMS := max(int64(0), time.Since(t.started).Milliseconds())
		t.encoded.Store(elapsedMS + 1)
	})
}

func (t *firstTokenTimer) milliseconds() *int64 {
	if t == nil {
		return nil
	}
	encoded := t.encoded.Load()
	if encoded == 0 {
		return nil
	}
	value := encoded - 1
	return &value
}

func (r *firstByteReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, err
}
