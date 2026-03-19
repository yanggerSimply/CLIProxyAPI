package middleware

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

type RateLimitConfig struct {
	// Max requests per minute per source (0 = unlimited)
	RPM int
	// Max tokens per minute per source (0 = unlimited)
	TPM int
	// Warning threshold as a fraction (0.0-1.0). Emit a warning log when usage exceeds this fraction of the limit.
	WarnThreshold float64
	// Enable exponential backoff: rejected clients receive Retry-After headers with increasing delays.
	ExponentialBackoff bool
}

type rateLimitEntry struct {
	mu             sync.Mutex
	requestTimes   []time.Time
	tokenCounts    []tokenRecord
	consecutiveHit int32
}

type tokenRecord struct {
	ts     time.Time
	tokens int
}

type RateLimiter struct {
	cfg        atomic.Pointer[RateLimitConfig]
	entries    sync.Map // string -> *rateLimitEntry
	usageStats *usage.RequestStatistics
}

func NewRateLimiter(cfg *RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		usageStats: usage.GetRequestStatistics(),
	}
	rl.cfg.Store(cfg)
	go rl.cleanupLoop()
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
			empty := len(entry.requestTimes) == 0 && len(entry.tokenCounts) == 0
			entry.mu.Unlock()
			if empty {
				rl.entries.Delete(key)
			}
			return true
		})
	}
}

// Middleware returns a Gin middleware that enforces RPM limits before the request.
// TPM is tracked after the response completes via RecordTokens.
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := rl.cfg.Load()
		if cfg == nil || (cfg.RPM <= 0 && cfg.TPM <= 0) {
			c.Next()
			return
		}

		source := rateLimitSource(c)
		entry := rl.getEntry(source)
		now := time.Now()
		windowStart := now.Add(-time.Minute)

		entry.mu.Lock()

		// --- RPM check ---
		if cfg.RPM > 0 {
			entry.requestTimes = pruneTimestamps(entry.requestTimes, windowStart)
			currentRPM := len(entry.requestTimes)

			warnThreshold := cfg.WarnThreshold
			if warnThreshold <= 0 {
				warnThreshold = 0.8
			}
			warnAt := int(math.Ceil(float64(cfg.RPM) * warnThreshold))
			if currentRPM >= warnAt && currentRPM < cfg.RPM {
				log.Warnf("[RateLimit] source=%s RPM approaching limit: %d/%d", source, currentRPM, cfg.RPM)
			}

			if currentRPM >= cfg.RPM {
				hits := atomic.AddInt32(&entry.consecutiveHit, 1)
				entry.mu.Unlock()
				retryAfter := computeRetryAfter(cfg.ExponentialBackoff, int(hits))
				log.Warnf("[RateLimit] source=%s RPM limit exceeded: %d/%d, retry-after=%ds", source, currentRPM, cfg.RPM, retryAfter)
				if rl.usageStats != nil {
					rl.usageStats.RecordRateLimited(source, fmt.Sprintf("rpm_exceeded:%d/%d", currentRPM, cfg.RPM))
				}
				rejectRequest(c, cfg.RPM, 0, retryAfter)
				return
			}

			entry.requestTimes = append(entry.requestTimes, now)
		}

		// --- TPM warning (pre-request, based on existing window) ---
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
			}

			if currentTPM >= cfg.TPM {
				hits := atomic.AddInt32(&entry.consecutiveHit, 1)
				entry.mu.Unlock()
				retryAfter := computeRetryAfter(cfg.ExponentialBackoff, int(hits))
				log.Warnf("[RateLimit] source=%s TPM limit exceeded: %d/%d, retry-after=%ds", source, currentTPM, cfg.TPM, retryAfter)
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

// RecordTokens records token usage after a successful response.
// Call this from the response logging middleware or handler completion.
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

// TokenTrackingMiddleware returns a Gin middleware that estimates and records token usage
// from the response body size when exact token counts aren't available.
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

		// Rough estimation: ~4 chars per token for English, response body bytes / 4
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
	delay := 1 << min(consecutiveHits, 8) // 2, 4, 8, 16 ... 256s max
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
