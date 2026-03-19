package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

type RateLimitConfig struct {
	RPM                int
	TPM                int
	MaxConcurrency     int
	WarnThreshold      float64
	ExponentialBackoff bool
	LarkWebhook        string
	LarkPrefix         string
	LarkEvents         string
}

type rateLimitEntry struct {
	mu             sync.Mutex
	requestTimes   []time.Time
	tokenCounts    []tokenRecord
	consecutiveHit int32
	inflight       int32
}

type tokenRecord struct {
	ts     time.Time
	tokens int
}

type RateLimiter struct {
	cfg        atomic.Pointer[RateLimitConfig]
	entries    sync.Map
	usageStats *usage.RequestStatistics
	larkMu     sync.Mutex
	larkLast   map[string]time.Time
	peakRPM    atomic.Int32
	peakConc   atomic.Int32
}

func NewRateLimiter(cfg *RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		usageStats: usage.GetRequestStatistics(),
		larkLast:   make(map[string]time.Time),
	}
	rl.cfg.Store(cfg)
	go rl.cleanupLoop()
	go rl.dailySummaryLoop()
	return rl
}

func (rl *RateLimiter) UpdateConfig(cfg *RateLimitConfig) {
	rl.cfg.Store(cfg)
}

func (rl *RateLimiter) getEntry(key string) *rateLimitEntry {
	if val, ok := rl.entries.Load(key); ok {
		return val.(*rateLimitEntry)
	}
	entry := &rateLimitEntry{}
	actual, _ := rl.entries.LoadOrStore(key, entry)
	return actual.(*rateLimitEntry)
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-2 * time.Minute)
		rl.entries.Range(func(key, value any) bool {
			entry := value.(*rateLimitEntry)
			entry.mu.Lock()
			entry.requestTimes = pruneTimestamps(entry.requestTimes, cutoff)
			entry.tokenCounts = pruneTokenRecords(entry.tokenCounts, cutoff)
			empty := len(entry.requestTimes) == 0 && len(entry.tokenCounts) == 0 && atomic.LoadInt32(&entry.inflight) == 0
			entry.mu.Unlock()
			if empty {
				rl.entries.Delete(key)
			}
			return true
		})
	}
}

func (rl *RateLimiter) dailySummaryLoop() {
	now := time.Now()
	nextRun := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	time.Sleep(time.Until(nextRun))

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	rl.sendDailySummary()
	for range ticker.C {
		rl.sendDailySummary()
	}
}

func (rl *RateLimiter) sendDailySummary() {
	cfg := rl.cfg.Load()
	if cfg == nil || strings.TrimSpace(cfg.LarkWebhook) == "" {
		return
	}
	if !rl.larkEnabled("daily") && !rl.larkEnabled("exceeded") {
		return
	}

	snap := rl.usageStats.Snapshot()
	peakRPM := rl.peakRPM.Swap(0)
	peakConc := rl.peakConc.Swap(0)

	successRate := float64(0)
	if snap.TotalRequests > 0 {
		successRate = float64(snap.SuccessCount) / float64(snap.TotalRequests) * 100
	}

	// Calculate recommended next-tier values
	recRPM, recTPM, recConc := rl.recommendNextTier(cfg, peakRPM, peakConc, snap)

	prefix := strings.TrimSpace(cfg.LarkPrefix)
	title := "📊 Daily Rate Limit Summary"
	if prefix != "" {
		title = fmt.Sprintf("[%s] %s", prefix, title)
	}

	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")

	body := fmt.Sprintf(
		"**Date:** %s\n"+
			"**Total Requests:** %d\n"+
			"**Success:** %d (%.1f%%)\n"+
			"**Failed:** %d\n"+
			"**Rate Limited:** %d\n"+
			"**Total Tokens:** %d\n"+
			"---\n"+
			"**Peak RPM:** %d / %d\n"+
			"**Peak Concurrency:** %d / %d\n"+
			"---\n"+
			"**🎯 Recommended Next Tier:**\n"+
			"RPM: %d → **%d**\n"+
			"TPM: %d → **%d**\n"+
			"MaxConcurrency: %d → **%d**\n"+
			"---\n"+
			"_If no rate-limited requests occurred and peak usage < 60%% of limits, consider raising limits gradually._",
		yesterday,
		snap.TotalRequests, snap.SuccessCount, successRate,
		snap.FailureCount, snap.RateLimitedCount, snap.TotalTokens,
		peakRPM, cfg.RPM, peakConc, cfg.MaxConcurrency,
		cfg.RPM, recRPM, cfg.TPM, recTPM, cfg.MaxConcurrency, recConc,
	)

	webhook := strings.TrimSpace(cfg.LarkWebhook)
	go func() {
		payload, _ := json.Marshal(map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"header": map[string]any{
					"title":    map[string]string{"tag": "plain_text", "content": title},
					"template": "blue",
				},
				"elements": []map[string]any{
					{"tag": "markdown", "content": body},
					{"tag": "note", "elements": []map[string]string{
						{"tag": "plain_text", "content": time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
					}},
				},
			},
		})

		resp, err := http.Post(webhook, "application/json", bytes.NewReader(payload))
		if err != nil {
			log.Warnf("[RateLimit] failed to send daily summary: %v", err)
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}()

	log.Info("[RateLimit] daily summary sent to Lark")
}

func (rl *RateLimiter) recommendNextTier(cfg *RateLimitConfig, peakRPM, peakConc int32, snap usage.StatisticsSnapshot) (rpm, tpm, conc int) {
	rpm = cfg.RPM
	tpm = cfg.TPM
	conc = cfg.MaxConcurrency

	if snap.RateLimitedCount > 0 {
		// Got rate-limited: don't increase, maybe stay or decrease
		return
	}

	// Safe zone: peak < 60% of limit → suggest +25% increase
	// Moderate zone: peak 60-85% → suggest +10% increase
	// Hot zone: peak > 85% → keep current
	bump := func(current int, peak int32) int {
		if current <= 0 {
			return current
		}
		ratio := float64(peak) / float64(current)
		switch {
		case ratio < 0.6:
			return int(math.Ceil(float64(current) * 1.25))
		case ratio < 0.85:
			return int(math.Ceil(float64(current) * 1.10))
		default:
			return current
		}
	}

	rpm = bump(cfg.RPM, peakRPM)
	conc = bump(cfg.MaxConcurrency, peakConc)

	// TPM: estimate from total tokens
	if cfg.TPM > 0 && snap.TotalRequests > 0 {
		avgTokensPerMin := float64(snap.TotalTokens) / (24 * 60)
		tpmRatio := avgTokensPerMin / float64(cfg.TPM)
		switch {
		case tpmRatio < 0.6:
			tpm = int(math.Ceil(float64(cfg.TPM) * 1.25))
		case tpmRatio < 0.85:
			tpm = int(math.Ceil(float64(cfg.TPM) * 1.10))
		}
	}

	return
}

func (rl *RateLimiter) larkEnabled(event string) bool {
	cfg := rl.cfg.Load()
	if cfg == nil || strings.TrimSpace(cfg.LarkWebhook) == "" {
		return false
	}
	events := strings.TrimSpace(cfg.LarkEvents)
	if events == "" {
		events = "exceeded"
	}
	for _, e := range strings.Split(events, ",") {
		if strings.TrimSpace(e) == event {
			return true
		}
	}
	return false
}

func (rl *RateLimiter) sendLarkNotification(event, title, message string) {
	if !rl.larkEnabled(event) {
		return
	}
	cfg := rl.cfg.Load()
	if cfg == nil {
		return
	}
	webhook := strings.TrimSpace(cfg.LarkWebhook)
	if webhook == "" {
		return
	}

	rl.larkMu.Lock()
	if last, ok := rl.larkLast[event]; ok && time.Since(last) < 30*time.Second {
		rl.larkMu.Unlock()
		return
	}
	rl.larkLast[event] = time.Now()
	rl.larkMu.Unlock()

	prefix := strings.TrimSpace(cfg.LarkPrefix)
	fullTitle := title
	if prefix != "" {
		fullTitle = fmt.Sprintf("[%s] %s", prefix, title)
	}

	go func() {
		payload, _ := json.Marshal(map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"header": map[string]any{
					"title":    map[string]string{"tag": "plain_text", "content": fullTitle},
					"template": "red",
				},
				"elements": []map[string]any{
					{"tag": "markdown", "content": message},
					{"tag": "note", "elements": []map[string]string{
						{"tag": "plain_text", "content": time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
					}},
				},
			},
		})

		resp, err := http.Post(webhook, "application/json", bytes.NewReader(payload))
		if err != nil {
			log.Warnf("[RateLimit] failed to send Lark notification: %v", err)
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode != http.StatusOK {
			log.Warnf("[RateLimit] Lark webhook returned status %d", resp.StatusCode)
		}
	}()
}

func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := rl.cfg.Load()
		if cfg == nil || (cfg.RPM <= 0 && cfg.TPM <= 0 && cfg.MaxConcurrency <= 0) {
			c.Next()
			return
		}

		// Skip rate limiting for management/control-panel endpoints
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/v0/management") || path == "/management.html" {
			c.Next()
			return
		}

		source := rateLimitSource(c)
		entry := rl.getEntry(source)
		now := time.Now()
		windowStart := now.Add(-time.Minute)

		// --- Concurrency check (lock-free) ---
		if cfg.MaxConcurrency > 0 {
			current := atomic.AddInt32(&entry.inflight, 1)
			for {
				old := rl.peakConc.Load()
				if current <= old || rl.peakConc.CompareAndSwap(old, current) {
					break
				}
			}
			if int(current) > cfg.MaxConcurrency {
				atomic.AddInt32(&entry.inflight, -1)
				hits := atomic.AddInt32(&entry.consecutiveHit, 1)
				retryAfter := computeRetryAfter(cfg.ExponentialBackoff, int(hits))
				log.Warnf("[RateLimit] source=%s concurrency limit exceeded: %d/%d, retry-after=%ds", source, current, cfg.MaxConcurrency, retryAfter)
				rl.sendLarkNotification("exceeded", "⚠️ Concurrency Limit Exceeded", fmt.Sprintf("**Source:** %s\n**Concurrent:** %d/%d\n**Retry-After:** %ds", source, current, cfg.MaxConcurrency, retryAfter))
				if rl.usageStats != nil {
					rl.usageStats.RecordRateLimited(source, fmt.Sprintf("concurrency_exceeded:%d/%d", current, cfg.MaxConcurrency))
				}
				rejectRequest(c, 0, 0, retryAfter)
				return
			}
			defer atomic.AddInt32(&entry.inflight, -1)
		}

		entry.mu.Lock()

		// --- RPM check ---
		if cfg.RPM > 0 {
			entry.requestTimes = pruneTimestamps(entry.requestTimes, windowStart)
			currentRPM := len(entry.requestTimes)
			if rpm32 := int32(currentRPM); rpm32 > 0 {
				for {
					old := rl.peakRPM.Load()
					if rpm32 <= old || rl.peakRPM.CompareAndSwap(old, rpm32) {
						break
					}
				}
			}

			warnThreshold := cfg.WarnThreshold
			if warnThreshold <= 0 {
				warnThreshold = 0.8
			}
			warnAt := int(math.Ceil(float64(cfg.RPM) * warnThreshold))
			if currentRPM >= warnAt && currentRPM < cfg.RPM {
				log.Warnf("[RateLimit] source=%s RPM approaching limit: %d/%d", source, currentRPM, cfg.RPM)
				rl.sendLarkNotification("warning", "⚡ RPM Approaching Limit", fmt.Sprintf("**Source:** %s\n**RPM:** %d/%d", source, currentRPM, cfg.RPM))
			}

			if currentRPM >= cfg.RPM {
				hits := atomic.AddInt32(&entry.consecutiveHit, 1)
				entry.mu.Unlock()
				retryAfter := computeRetryAfter(cfg.ExponentialBackoff, int(hits))
				log.Warnf("[RateLimit] source=%s RPM limit exceeded: %d/%d, retry-after=%ds", source, currentRPM, cfg.RPM, retryAfter)
				rl.sendLarkNotification("exceeded", "⚠️ RPM Limit Exceeded", fmt.Sprintf("**Source:** %s\n**RPM:** %d/%d\n**Retry-After:** %ds", source, currentRPM, cfg.RPM, retryAfter))
				if rl.usageStats != nil {
					rl.usageStats.RecordRateLimited(source, fmt.Sprintf("rpm_exceeded:%d/%d", currentRPM, cfg.RPM))
				}
				rejectRequest(c, cfg.RPM, 0, retryAfter)
				return
			}

			entry.requestTimes = append(entry.requestTimes, now)
		}

		// --- TPM warning ---
		if cfg.TPM > 0 {
			entry.tokenCounts = pruneTokenRecords(entry.tokenCounts, windowStart)
			currentTPM := sumTokens(entry.tokenCounts)
			warnThreshold := cfg.WarnThreshold
			if warnThreshold <= 0 {
				warnThreshold = 0.8
			}
			warnAt := int(math.Ceil(float64(cfg.TPM) * warnThreshold))
			if currentTPM >= warnAt && currentTPM < cfg.TPM {
				log.Warnf("[RateLimit] source=%s TPM approaching limit: %d/%d", source, currentTPM, cfg.TPM)
				rl.sendLarkNotification("warning", "⚡ TPM Approaching Limit", fmt.Sprintf("**Source:** %s\n**TPM:** %d/%d", source, currentTPM, cfg.TPM))
			}

			if currentTPM >= cfg.TPM {
				hits := atomic.AddInt32(&entry.consecutiveHit, 1)
				entry.mu.Unlock()
				retryAfter := computeRetryAfter(cfg.ExponentialBackoff, int(hits))
				log.Warnf("[RateLimit] source=%s TPM limit exceeded: %d/%d, retry-after=%ds", source, currentTPM, cfg.TPM, retryAfter)
				rl.sendLarkNotification("exceeded", "⚠️ TPM Limit Exceeded", fmt.Sprintf("**Source:** %s\n**TPM:** %d/%d\n**Retry-After:** %ds", source, currentTPM, cfg.TPM, retryAfter))
				if rl.usageStats != nil {
					rl.usageStats.RecordRateLimited(source, fmt.Sprintf("tpm_exceeded:%d/%d", currentTPM, cfg.TPM))
				}
				rejectRequest(c, 0, cfg.TPM, retryAfter)
				return
			}
		}

		atomic.StoreInt32(&entry.consecutiveHit, 0)
		entry.mu.Unlock()

		c.Next()
	}
}

func (rl *RateLimiter) RecordTokens(source string, tokens int) {
	if tokens <= 0 {
		return
	}
	cfg := rl.cfg.Load()
	if cfg == nil || cfg.TPM <= 0 {
		return
	}
	entry := rl.getEntry(source)
	now := time.Now()
	entry.mu.Lock()
	entry.tokenCounts = append(entry.tokenCounts, tokenRecord{ts: now, tokens: tokens})
	entry.mu.Unlock()
}

func (rl *RateLimiter) TokenTrackingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		cfg := rl.cfg.Load()
		if cfg == nil || cfg.TPM <= 0 {
			return
		}

		if c.Writer.Status() >= 400 {
			return
		}

		bytesWritten := c.Writer.Size()
		if bytesWritten <= 0 {
			return
		}

		estimatedTokens := bytesWritten / 4
		if estimatedTokens < 1 {
			estimatedTokens = 1
		}

		source := rateLimitSource(c)
		rl.RecordTokens(source, estimatedTokens)
	}
}

func rateLimitSource(c *gin.Context) string {
	if apiKey, exists := c.Get("apiKey"); exists {
		if key, ok := apiKey.(string); ok && key != "" {
			return key
		}
	}
	return c.ClientIP()
}

func computeRetryAfter(exponential bool, consecutiveHits int) int {
	if !exponential || consecutiveHits <= 1 {
		return 2
	}
	delay := 1 << min(consecutiveHits, 8)
	if delay > 256 {
		delay = 256
	}
	return delay
}

func rejectRequest(c *gin.Context, rpm, tpm, retryAfter int) {
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
	c.Header("X-RateLimit-Reset-Requests", fmt.Sprintf("%ds", retryAfter))

	msg := "Rate limit exceeded."
	if rpm > 0 {
		msg = fmt.Sprintf("Rate limit exceeded: %d requests per minute. Retry after %d seconds.", rpm, retryAfter)
	} else if tpm > 0 {
		msg = fmt.Sprintf("Rate limit exceeded: %d tokens per minute. Retry after %d seconds.", tpm, retryAfter)
	}

	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
		},
	})

	c.Data(http.StatusTooManyRequests, "application/json", body)
	c.Abort()
}

func pruneTimestamps(times []time.Time, cutoff time.Time) []time.Time {
	start := 0
	for start < len(times) && times[start].Before(cutoff) {
		start++
	}
	if start == 0 {
		return times
	}
	n := copy(times, times[start:])
	return times[:n]
}

func pruneTokenRecords(records []tokenRecord, cutoff time.Time) []tokenRecord {
	start := 0
	for start < len(records) && records[start].ts.Before(cutoff) {
		start++
	}
	if start == 0 {
		return records
	}
	n := copy(records, records[start:])
	return records[:n]
}

func sumTokens(records []tokenRecord) int {
	total := 0
	for _, r := range records {
		total += r.tokens
	}
	return total
}
