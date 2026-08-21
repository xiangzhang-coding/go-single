package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

type processingOrderService struct{ service.Service }

func (processingOrderService) Create(context.Context, int64, service.CreateParams) (*service.CreateResult, error) {
	return &service.CreateResult{
		Order:      &model.OrderView{Order: model.Order{OrderNo: "12345"}},
		Idempotent: true,
		Processing: true,
	}, nil
}

func TestCreateProcessingResponseHasExplicitContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := auth.NewJWT(auth.JWTConfig{Secret: "order-handler-test", TTL: time.Hour})
	token, err := verifier.Issue(42, "user")
	require.NoError(t, err)

	router := gin.New()
	api := router.Group("/api")
	orderhandler.New(processingOrderService{}, verifier, processingOrderService{}).RegisterRoutes(api)

	req := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewBufferString(
		`{"client_request_id":"same-request","address_id":1,"items":[{"sku_id":1,"quantity":1}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	require.Equal(t, http.StatusAccepted, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, map[string]any{
		"state":    "processing",
		"order_no": "12345",
	}, body)
}
