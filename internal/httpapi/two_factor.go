package httpapi

import (
	"errors"
	"net/http"

	"buysms/internal/application"
	"buysms/internal/auth"
	"github.com/gin-gonic/gin"
)

func (h *Handler) loginTwoFactor(c *gin.Context) {
	setNoStore(c)
	var in auth.TwoFactorLoginInput
	if !bind(c, &in) {
		return
	}
	if header := c.GetHeader("X-Admin-Path"); header != "" {
		if in.AdminPath != "" && in.AdminPath != header {
			bad(c, "后台入口校验失败")
			return
		}
		in.AdminPath = header
	}
	in.IP, in.UserAgent = c.ClientIP(), c.Request.UserAgent()
	result, err := h.auth.LoginTwoFactor(c.Request.Context(), in)
	if err != nil {
		if !respondTwoFactorError(c, err) {
			serverError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) twoFactorSetup(c *gin.Context) {
	var in application.TwoFactorSetupInput
	if !bind(c, &in) {
		return
	}
	setup, err := h.app.TwoFactorSetup(c.Request.Context(), in, currentUser(c))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, setup)
}

func (h *Handler) userTwoFactorDetails(c *gin.Context) {
	details, err := h.app.UserTwoFactorDetails(c.Request.Context(), c.Param("id"), currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, details)
}

func respondTwoFactorError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, auth.ErrTwoFactorInvalid):
		failWithCode(c, http.StatusBadRequest, "two_factor_invalid", err.Error())
	case errors.Is(err, auth.ErrTwoFactorExpired):
		failWithCode(c, http.StatusUnauthorized, "two_factor_expired", err.Error())
	case errors.Is(err, auth.ErrTwoFactorSetupExpired):
		failWithCode(c, http.StatusBadRequest, "two_factor_setup_expired", err.Error())
	case errors.Is(err, auth.ErrTwoFactorSetupInvalid):
		failWithCode(c, http.StatusBadRequest, "two_factor_setup_invalid", err.Error())
	case errors.Is(err, auth.ErrRateLimited):
		failWithCode(c, http.StatusTooManyRequests, "rate_limited", err.Error())
	default:
		return false
	}
	return true
}
