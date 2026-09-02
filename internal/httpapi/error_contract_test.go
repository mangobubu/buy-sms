package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"buysms/internal/application"

	"github.com/gin-gonic/gin"
)

func TestRespondErrorPurchaseContract(t *testing.T) {
	tests := []struct {
		name, code, message string
		kind                error
		status              int
	}{
		{"幂等参数不一致", "idempotency_mismatch", "该购买编号已用于其他条件，页面将生成新的购买请求", application.ErrConflict, http.StatusConflict},
		{"购买处理中", "purchase_in_progress", "购买请求仍在处理中，系统尚未收到最终结果；请等待最多2分钟，再使用当前请求重试确认；为避免重复扣费，仅暂停该请求的重复提交", application.ErrConflict, http.StatusConflict},
		{"结果待确认", "purchase_result_unknown", "购买结果尚未确认：系统未能确定供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交", application.ErrConflict, http.StatusConflict},
		{"价格超限", "price_exceeded", "供应商实际价格超过所选价格，购买已取消", application.ErrConflict, http.StatusConflict},
		{"暂无号码", "no_numbers", "所选条件当前暂无可用号码，请稍后重试或调整条件", application.ErrConflict, http.StatusConflict},
		{"余额不足", "insufficient_balance", "供应商账户余额不足，请联系管理员充值", application.ErrProvider, http.StatusBadGateway},
		{"选择无效", "invalid_selection", "供应商不支持所选国家或服务，请重新选择", application.ErrBadRequest, http.StatusBadRequest},
		{"请求限流", "provider_rate_limited", "供应商请求过于频繁，请稍后重试", application.ErrProvider, http.StatusTooManyRequests},
		{"供应商停用", "provider_disabled", "所选供应商已停用，请选择其他供应商", application.ErrConflict, http.StatusConflict},
		{"供应商错误", "provider_error", "购买结果尚未确认：供应商请求超时、连接中断或响应异常，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交", application.ErrProvider, http.StatusBadGateway},
		{"供应商前置查询错误", "provider_preflight_error", "购买前获取供应商资源失败，号码购买尚未提交；可以重新提交当前平台与购买条件", application.ErrProvider, http.StatusBadGateway},
		{"配置错误", "configuration", "供应商配置不完整，请联系管理员", application.ErrProvider, http.StatusBadGateway},
		{"准备失败", "purchase_setup_failed", "购买准备失败，请稍后重试", nil, http.StatusInternalServerError},
		{"保存失败", "database_error", "购买结果尚未确认：供应商已返回号码，但本地订单保存结果未确认，系统无法判断订单是否已记录或号码是否已取消；为避免重复扣费，仅暂停当前平台与购买条件的重复提交", nil, http.StatusInternalServerError},
		{"未知失败", "purchase_failed", "购买请求已失败，请刷新页面后重试", application.ErrConflict, http.StatusConflict},
	}
	gin.SetMode(gin.TestMode)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/orders", nil)
			var err error = &application.PurchaseError{Code: tt.code, Message: tt.message, Kind: tt.kind, Cause: errors.New("detail")}
			if tt.code == "configuration" {
				err = fmt.Errorf("包装: %w", err)
			}

			respondError(context, err)

			if recorder.Code != tt.status {
				t.Fatalf("HTTP 状态=%d，期望=%d；正文=%s", recorder.Code, tt.status, recorder.Body.String())
			}
			expected := `{"code":"` + tt.code + `","message":"` + tt.message + `"}`
			if recorder.Body.String() != expected {
				t.Fatalf("响应=%s，期望=%s", recorder.Body.String(), expected)
			}
		})
	}
}

func TestFailIncludesStableDefaultCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	fail(context, http.StatusBadRequest, "参数错误")

	if recorder.Body.String() != `{"code":"bad_request","message":"参数错误"}` {
		t.Fatalf("响应=%s", recorder.Body.String())
	}
}
