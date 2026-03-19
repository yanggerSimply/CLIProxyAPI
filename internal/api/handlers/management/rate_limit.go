package management

import (
	"net/http"

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
