package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type rateLimitResponse struct {
	RPM                int     `json:"rpm"`
	TPM                int     `json:"tpm"`
	MaxConcurrency     int     `json:"max-concurrency"`
	WarnThreshold      float64 `json:"warn-threshold"`
	ExponentialBackoff bool    `json:"exponential-backoff"`
	LarkWebhook        string  `json:"lark-webhook"`
	LarkPrefix         string  `json:"lark-prefix"`
	LarkEvents         string  `json:"lark-events"`
}

type rateLimitUpdateRequest struct {
	RPM                *int     `json:"rpm"`
	TPM                *int     `json:"tpm"`
	MaxConcurrency     *int     `json:"max-concurrency"`
	WarnThreshold      *float64 `json:"warn-threshold"`
	ExponentialBackoff *bool    `json:"exponential-backoff"`
	LarkWebhook        *string  `json:"lark-webhook"`
	LarkPrefix         *string  `json:"lark-prefix"`
	LarkEvents         *string  `json:"lark-events"`
}

func (h *Handler) GetRateLimit(c *gin.Context) {
	rl := h.cfg.RateLimit
	c.JSON(http.StatusOK, gin.H{
		"rate-limit": rateLimitResponse{
			RPM:                rl.RPM,
			TPM:                rl.TPM,
			MaxConcurrency:     rl.MaxConcurrency,
			WarnThreshold:      rl.WarnThreshold,
			ExponentialBackoff: rl.ExponentialBackoff,
			LarkWebhook:        rl.LarkWebhook,
			LarkPrefix:         rl.LarkPrefix,
			LarkEvents:         rl.LarkEvents,
		},
	})
}

func (h *Handler) PutRateLimit(c *gin.Context) {
	var body rateLimitUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	rl := &h.cfg.RateLimit
	if body.RPM != nil {
		v := *body.RPM
		if v < 0 {
			v = 0
		}
		rl.RPM = v
	}
	if body.TPM != nil {
		v := *body.TPM
		if v < 0 {
			v = 0
		}
		rl.TPM = v
	}
	if body.MaxConcurrency != nil {
		v := *body.MaxConcurrency
		if v < 0 {
			v = 0
		}
		rl.MaxConcurrency = v
	}
	if body.WarnThreshold != nil {
		v := *body.WarnThreshold
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		rl.WarnThreshold = v
	}
	if body.ExponentialBackoff != nil {
		rl.ExponentialBackoff = *body.ExponentialBackoff
	}
	if body.LarkWebhook != nil {
		rl.LarkWebhook = *body.LarkWebhook
	}
	if body.LarkPrefix != nil {
		rl.LarkPrefix = *body.LarkPrefix
	}
	if body.LarkEvents != nil {
		rl.LarkEvents = *body.LarkEvents
	}

	h.persist(c)
}

func (h *Handler) PatchRateLimit(c *gin.Context) {
	h.PutRateLimit(c)
}

func (h *Handler) DeleteRateLimit(c *gin.Context) {
	h.cfg.RateLimit = config.RateLimitConfig{}
	h.persist(c)
}

// TestLarkWebhook sends a test notification to the configured Lark webhook via the backend.
func (h *Handler) TestLarkWebhook(c *gin.Context) {
	rl := h.cfg.RateLimit
	webhook := strings.TrimSpace(rl.LarkWebhook)
	if webhook == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "lark-webhook is not configured"})
		return
	}

	prefix := strings.TrimSpace(rl.LarkPrefix)
	title := "✅ CLIProxyAPI 通知测试"
	if prefix != "" {
		title = fmt.Sprintf("[%s] %s", prefix, title)
	}

	payload, _ := json.Marshal(map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": title},
				"template": "green",
			},
			"elements": []map[string]any{
				{"tag": "markdown", "content": "**限流通知已配置成功**\n当触发 RPM/TPM/并发 限流时，你会在这里收到告警。"},
				{"tag": "note", "elements": []map[string]string{
					{"tag": "plain_text", "content": time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
				}},
			},
		},
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to send: %v", err)})
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("webhook returned status %d", resp.StatusCode)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
