package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
	"github.com/xiangzhang-coding/go-single/internal/user/service"
)

// writeError keeps user-owned business boundaries local while sharing the
// transport-level timeout and fallback contract.
func writeError(c *gin.Context, err error) {
	httpresponse.WriteError(c, err,
		httpresponse.Rule{Status: http.StatusBadRequest, Errors: []error{
			service.ErrInvalidUsername, service.ErrInvalidPassword, service.ErrInvalidProfile, service.ErrInvalidAddress,
		}},
		httpresponse.Rule{Status: http.StatusUnauthorized, Errors: []error{service.ErrInvalidCredentials}, Message: "invalid username or password"},
		httpresponse.Rule{Status: http.StatusForbidden, Errors: []error{service.ErrAddressForbidden}},
		httpresponse.Rule{Status: http.StatusNotFound, Errors: []error{service.ErrUserNotFound, service.ErrAddressNotFound}},
		httpresponse.Rule{Status: http.StatusConflict, Errors: []error{service.ErrUsernameTaken}},
	)
}
