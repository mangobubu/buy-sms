package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSMSBowerFinishRequiresMatchingAcknowledgement(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   string
		response string
		wantCode string
	}{
		{name: "完成确认", status: "6", response: "ACCESS_ACTIVATION"},
		{name: "取消确认", status: "8", response: "ACCESS_CANCEL"},
		{name: "确认忽略空白大小写", status: "8", response: " access_cancel\n"},
		{name: "完成不能接受取消回执", status: "6", response: "ACCESS_CANCEL", wantCode: "INVALID_RESPONSE"},
		{name: "取消不能接受完成回执", status: "8", response: "ACCESS_ACTIVATION", wantCode: "INVALID_RESPONSE"},
		{name: "完成不能接受续码回执", status: "6", response: "ACCESS_RETRY_GET", wantCode: "INVALID_RESPONSE"},
		{name: "取消不能接受泛化成功", status: "8", response: "ACCESS_READY", wantCode: "INVALID_RESPONSE"},
		{name: "完成不能接受无效后缀", status: "6", response: "ACCESS_ACTIVATION:unknown", wantCode: "INVALID_RESPONSE"},
		{name: "平台拒绝保留错误码", status: "8", response: "BAD_STATUS", wantCode: "BAD_STATUS"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				query := request.URL.Query()
				if request.Method != http.MethodGet || query.Get("action") != "setStatus" ||
					query.Get("id") != "finish-order" || query.Get("status") != tt.status || query.Get("api_key") != testAPIKey {
					t.Error("结束订单请求参数不正确")
					http.Error(writer, "BAD_STATUS", http.StatusBadRequest)
					return
				}
				_, _ = writer.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)
			client := NewSMSBower(server.URL)
			var err error
			if tt.status == "6" {
				err = client.Complete(context.Background(), testAPIKey, "finish-order")
			} else {
				err = client.Cancel(context.Background(), testAPIKey, "finish-order")
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("合法确认失败: %v", err)
				}
				return
			}
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Code != tt.wantCode {
				t.Fatalf("错误回执被当成成功或错误码不正确: got=%v want=%s", err, tt.wantCode)
			}
		})
	}
}
