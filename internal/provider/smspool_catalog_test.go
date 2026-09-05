package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"buysms/internal/domain"
)

func TestSMSPoolCatalogQueriesStockPerPoolAndBuildsPriceOptions(t *testing.T) {
	var mutex sync.Mutex
	stockCalls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost {
			t.Errorf("目录请求方法=%s", request.Method)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析目录请求失败: %v", err)
		}
		if request.Form.Get("key") != testAPIKey || request.Form.Get("country") != "1" || request.Form.Get("service") != "671" {
			t.Errorf("目录请求参数错误: country=%q service=%q", request.Form.Get("country"), request.Form.Get("service"))
		}
		switch request.URL.Path {
		case "/request/pricing":
			_, _ = writer.Write([]byte(`[
				{"country":1,"service":671,"pool":4,"price":"0.50"},
				{"country":1,"service":671,"pool":1,"price":"0.10"},
				{"country":1,"service":671,"pool":2,"price":"0.24"},
				{"country":1,"service":671,"pool":3,"price":"0.24"}
			]`))
		case "/sms/stock":
			pool := request.Form.Get("pool")
			mutex.Lock()
			stockCalls[pool]++
			mutex.Unlock()
			amounts := map[string]string{"1": "0", "2": "2", "3": `"3"`, "4": "9"}
			amount, found := amounts[pool]
			if !found {
				t.Errorf("库存请求未绑定真实报价池: %q", pool)
				http.Error(writer, `{"success":0}`, http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"success":1,"amount":` + amount + `}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	items, err := NewSMSPool(server.URL).Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "1", Service: "671"})
	if err != nil || len(items) != 1 {
		t.Fatalf("定向报价=%+v err=%v", items, err)
	}
	item := items[0]
	if item.Country != "1" || item.Code != "671" || item.Price == nil || *item.Price != 0.24 || item.Stock == nil || *item.Stock != 5 {
		t.Fatalf("应选最低有库存价格而非无库存低价: %+v", item)
	}
	wantOptions := []domain.CatalogPriceOption{{Price: 0.24, Available: 5}, {Price: 0.5, Available: 9}}
	if !reflect.DeepEqual(item.PriceOptions, wantOptions) {
		t.Fatalf("同价池库存应相加且价格升序: got=%+v want=%+v", item.PriceOptions, wantOptions)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if !reflect.DeepEqual(stockCalls, map[string]int{"1": 1, "2": 1, "3": 1, "4": 1}) {
		t.Fatalf("每个报价池应独立查询一次库存: %+v", stockCalls)
	}
}

func TestSMSPoolCatalogParsesStockAmounts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "整数库存", body: `{"success":1,"amount":12}`, want: 12},
		{name: "字符串库存", body: `{"success":1,"amount":"12"}`, want: 12},
		{name: "零库存", body: `{"success":1,"amount":0}`, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/request/pricing" {
					_, _ = writer.Write([]byte(`[{"country":1,"service":671,"pool":7,"price":"0.24"}]`))
					return
				}
				if request.URL.Path != "/sms/stock" {
					http.NotFound(writer, request)
					return
				}
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			items, err := NewSMSPool(server.URL).Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "1", Service: "671"})
			if err != nil || len(items) != 1 || items[0].Stock == nil || *items[0].Stock != test.want {
				t.Fatalf("库存解析错误: items=%+v err=%v", items, err)
			}
			if test.want == 0 && len(items[0].PriceOptions) != 0 {
				t.Fatalf("零库存不应生成可购买价格档: %+v", items[0].PriceOptions)
			}
		})
	}
}

func TestSMSPoolCatalogRejectsStockErrorsAndInvalidAmounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "上游HTTP失败", body: `{"success":0}`, status: http.StatusBadGateway},
		{name: "业务错误", body: `{"success":0,"type":"STOCK_ERROR","amount":12}`},
		{name: "无效JSON", body: `{"success":1`},
		{name: "缺少库存", body: `{"success":1}`},
		{name: "空库存", body: `{"success":1,"amount":null}`},
		{name: "负库存", body: `{"success":1,"amount":-1}`},
		{name: "小数库存", body: `{"success":1,"amount":1.5}`},
		{name: "文本库存", body: `{"success":1,"amount":"invalid"}`},
		{name: "布尔库存", body: `{"success":1,"amount":true}`},
		{name: "库存溢出", body: `{"success":1,"amount":18446744073709551616}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/request/pricing" {
					_, _ = writer.Write([]byte(`[{"country":1,"service":671,"pool":7,"price":"0.24"}]`))
					return
				}
				if request.URL.Path != "/sms/stock" {
					http.NotFound(writer, request)
					return
				}
				if test.status != 0 {
					writer.WriteHeader(test.status)
				}
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			items, err := NewSMSPool(server.URL).Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "1", Service: "671"})
			if err == nil {
				t.Fatalf("库存请求失败或格式错误应返回错误，实际 items=%+v", items)
			}
		})
	}
}

func TestSMSPoolNonTargetedPriceCatalogDoesNotQueryStock(t *testing.T) {
	for _, request := range []CatalogRequest{
		{Kind: CatalogPrice},
		{Kind: CatalogPrice, Country: "1"},
		{Kind: CatalogPrice, Service: "671"},
	} {
		t.Run(request.Country+"_"+request.Service, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
				if incoming.URL.Path != "/request/pricing" {
					t.Errorf("非定向目录不应查询库存: %s", incoming.URL.Path)
					http.NotFound(writer, incoming)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`[{"country":1,"service":671,"pool":7,"price":"0.24"}]`))
			}))
			defer server.Close()
			items, err := NewSMSPool(server.URL).Catalog(context.Background(), testAPIKey, request)
			if err != nil || len(items) != 1 || items[0].Price == nil || *items[0].Price != 0.24 {
				t.Fatalf("非定向价格目录解析错误: items=%+v err=%v", items, err)
			}
		})
	}
}
