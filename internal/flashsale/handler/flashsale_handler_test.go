package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

type purchaseOnlyService struct {
	service.Service
	purchase *model.PreDeduction
	result   *service.PurchaseResult
}

func (s purchaseOnlyService) GetPreDeduction(context.Context, int64, int64) (*model.PreDeduction, error) {
	return s.purchase, nil
}

func (s purchaseOnlyService) Seckill(context.Context, int64, int64, string) (*service.PurchaseResult, error) {
	return s.result, nil
}

func TestGetPurchaseExposesOnlyLifecycleContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	orderNo := "9001"
	svc := purchaseOnlyService{purchase: &model.PreDeduction{
		ID: 123, UserID: 42, ActivityID: 7, ClientRequestID: "private-request",
		OrderNo: &orderNo, SKUID: 8, Price: 9900, Quantity: 1, PurchaseSlot: 2,
		Status: model.PreDeductionStatusOrdered, PublishAttempts: 3, RollbackAttempts: 4,
		LastError: "private dependency detail", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
		OrderedAt: &now, RolledBackAt: &now,
	}}
	verifier := auth.NewJWT(auth.JWTConfig{Secret: "purchase-contract-test", TTL: time.Hour})
	token, err := verifier.Issue(42, "user")
	require.NoError(t, err)
	r := gin.New()
	New(svc, verifier).RegisterRoutes(r.Group("/api"), func(c *gin.Context) { c.Next() })
	req := httptest.NewRequest(http.MethodGet, "/api/flashsales/purchases/123", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.ElementsMatch(t, []string{
		"id", "status", "order_no", "created_at", "updated_at", "ordered_at", "rolled_back_at",
	}, keys(body))
	require.Equal(t, "123", body["id"])
	require.Equal(t, string(model.PreDeductionStatusOrdered), body["status"])
	require.Equal(t, orderNo, body["order_no"])
}

func TestPurchaseReturnsRolledBackLifecycleInsteadOfQueued(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := purchaseOnlyService{result: &service.PurchaseResult{
		PreDeductionID: 123,
		OrderNo:        "9001",
		Status:         model.PreDeductionStatusRolledBack,
	}}
	verifier := auth.NewJWT(auth.JWTConfig{Secret: "rolled-back-purchase-test", TTL: time.Hour})
	token, err := verifier.Issue(42, "user")
	require.NoError(t, err)
	r := gin.New()
	New(svc, verifier).RegisterRoutes(r.Group("/api"), func(c *gin.Context) { c.Next() })
	req := httptest.NewRequest(http.MethodPost, "/api/flashsales/7/purchase",
		strings.NewReader(`{"client_request_id":"same-request"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "123", body["pre_deduction_id"])
	require.Equal(t, "9001", body["order_no"])
	require.Equal(t, string(model.PreDeductionStatusRolledBack), body["pre_deduction_status"])
	require.Equal(t, string(model.PreDeductionStatusRolledBack), body["status"])
	require.Equal(t, "抢购已回退", body["message"])
}

func keys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}
