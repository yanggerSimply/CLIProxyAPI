package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type rateLimitResponse struct {
	RPM                int     `json:"rpm"`
	TPM                int     `json:"tpm"`
	WarnThreshold      float64 `json:"warn-threshold"`
	ExponentialBackoff bool    `json:"exponential-backoff"`
}

type rateLimitUpdateRequest struct {
	RPM                *int     `json:"rpm"`
	TPM                *int     `json:"tpm"`
	WarnThreshold      *float64 `json:"warn-threshold"`
	ExponentialBackoff *bool    `json:"exponential-backoff"`
}

// GetRateLimit returns the current rate-limit configuration.
func (h *Handler) GetRateLimit(c *gin.Context) {
	rl := h.cfg.RateLimit
	c.JSON(http.StatusOK, gin.H{
		"rate-limit": rateLimitResponse{
			RPM:                rl.RPM,
			TPM:                rl.TPM,
			WarnThreshold:      rl.WarnThreshold,
			ExponentialBackoff: rl.ExponentialBackoff,
		},
	})
}

// PutRateLimit replaces the entire rate-limit configuration.
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

	h.persist(c)
}

// PatchRateLimit is an alias for PutRateLimit (partial update).
func (h *Handler) PatchRateLimit(c *gin.Context) {
	h.PutRateLimit(c)
}

// DeleteRateLimit resets rate-limit configuration to defaults (disabled).
func (h *Handler) DeleteRateLimit(c *gin.Context) {
	h.cfg.RateLimit = config.RateLimitConfig{}
	h.persist(c)
}
