package guide

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

type testCatalog struct {
	fail  bool
	query string
}

func (f *testCatalog) SearchShows(ctx context.Context, q string, n int) ([]ticketing.Show, error) {
	f.query = q
	if f.fail {
		return nil, errors.New("private upstream detail")
	}
	return []ticketing.Show{{ID: 1, Name: "测试话剧", Area: "上海", Address: "测试剧院"}}, nil
}
func (f *testCatalog) ShowDetail(ctx context.Context, id int64) (ticketing.Show, error) {
	return ticketing.Show{ID: id, Name: "测试话剧"}, nil
}
func (f *testCatalog) ListTickets(ctx context.Context, id int64) ([]ticketing.TicketTier, error) {
	return []ticketing.TicketTier{{ID: 11, ShowID: id, Title: "普通票", OnSale: true, Price: 280, Stock: 8}, {ID: 12, ShowID: id, Title: "VIP", OnSale: true, Price: 680, Stock: 20}, {ID: 13, ShowID: id, Title: "售罄票", OnSale: true, Price: 100, Stock: 0, SoldOut: true}}, nil
}
func (f *testCatalog) ListSeckillTickets(context.Context, int) ([]ticketing.TicketTier, error) {
	return nil, nil
}

func TestAssistantUsesVerifiedBudgetTicketsWithoutModel(t *testing.T) {
	fixture := &testCatalog{}
	handler := NewHandler(fixture, llm.Config{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/assist", strings.NewReader(`{"message":"上海话剧，500元以内"}`)))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var data struct {
		Answer string           `json:"answer"`
		Mode   string           `json:"mode"`
		Items  []recommendation `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Mode != "catalog" || len(data.Items) != 1 || len(data.Items[0].Tickets) != 1 || data.Items[0].Tickets[0].Price != 280 {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	if strings.Contains(data.Answer, "680") || strings.Contains(data.Answer, "售罄票") {
		t.Fatal("recommended unavailable or over-budget tickets")
	}
}

func TestAssistantNoMatchDoesNotInventInventory(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(&testCatalog{}, llm.Config{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/assist", strings.NewReader(`{"message":"推荐一场","budget":50}`)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"items":[]`) || !strings.Contains(response.Body.String(), "暂时没有找到") {
		t.Fatal(response.Body.String())
	}
}

func TestGuideUpstreamErrorIsActionableAndPrivate(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(&testCatalog{fail: true}, llm.Config{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/ticketing/shows", nil))
	if response.Code != 503 || strings.Contains(response.Body.String(), "private upstream detail") || !strings.Contains(response.Body.String(), "Echo") {
		t.Fatal(response.Body.String())
	}
}

// failingRichCatalog 让全量快照读取失败，用来验证富模式（/assist 与 /assist/stream）
// 的错误出口和列表页一样只给可执行提示，不把地址、驱动原文抛给访客。
type failingRichCatalog struct {
	testCatalog
}

func (f *failingRichCatalog) Catalog(context.Context, bool) (ticketing.CatalogSnapshot, error) {
	return ticketing.CatalogSnapshot{}, errors.New("private upstream detail")
}

func (f *failingRichCatalog) Reviews(context.Context, int64, bool) (ticketing.ReviewReport, error) {
	return ticketing.ReviewReport{}, errors.New("private upstream detail")
}

func TestAssistantUpstreamErrorIsActionableAndPrivate(t *testing.T) {
	handler, mux := New(&failingRichCatalog{}, llm.Config{})
	for name, target := range map[string]struct {
		handler http.Handler
		path    string
	}{
		"assist": {mux, "/api/v1/ticketing/assist"},
		"stream": {handler.StreamHandler(), "/api/v1/ticketing/assist/stream"},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, target.path, strings.NewReader(`{"message":"上海话剧，500元以内"}`))
			target.handler.ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != http.StatusServiceUnavailable || strings.Contains(body, "private upstream detail") || !strings.Contains(body, "Echo") {
				t.Fatalf("status=%d body=%s", response.Code, body)
			}
		})
	}
}

func TestGuideValidatesInputAndNeverAcceptsPurchase(t *testing.T) {
	handler := NewHandler(&testCatalog{}, llm.Config{})
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/ticketing/shows/not-an-id/tickets", "", 400},
		{"POST", "/api/v1/ticketing/assist", `{"message":""}`, 400},
		{"POST", "/api/v1/ticketing/assist", `{"message":"推荐","budget":-1}`, 400},
		{"POST", "/api/v1/ticketing/shows", "", 405},
		{"POST", "/api/v1/ticketing/orders", "", 404},
	} {
		t.Run(test.path+test.body, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
			if response.Code != test.status {
				t.Fatalf("status=%d %s", response.Code, response.Body.String())
			}
		})
	}
}
