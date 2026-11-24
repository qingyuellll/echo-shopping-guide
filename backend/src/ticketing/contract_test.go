package ticketing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{apiBase: server.URL, http: server.Client()}
}

func TestEchoWireIDsAndCents(t *testing.T) {
	for _, literal := range []string{`"9007199254740993"`, `9007199254740993`} {
		t.Run(literal, func(t *testing.T) {
			var record apiShow
			if err := json.Unmarshal([]byte(`{"id":`+literal+`,"avgPrice":16800,"name":"音乐会"}`), &record); err != nil {
				t.Fatal(err)
			}
			show := record.toShow()
			if show.ID != 9007199254740993 || show.AvgPrice != 168 {
				t.Fatalf("wire conversion: %+v", show)
			}
			raw, _ := json.Marshal(show)
			if !strings.Contains(string(raw), `"show_id":"9007199254740993"`) {
				t.Fatalf("lost JS ID precision: %s", raw)
			}
		})
	}
}

func TestHTTPGenreCityVenueSearchAndLimit(t *testing.T) {
	pages := 0
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/show-type/list":
			fmt.Fprint(w, `{"success":true,"data":[{"id":3,"name":"话剧"}]}`)
		case "/show/of/name":
			if r.URL.Query().Get("current") == "" {
				t.Error("must use Echo's current parameter")
			}
			pages++
			if r.URL.Query().Get("current") == "1" {
				records := make([]map[string]any, 20)
				for i := range records {
					records[i] = map[string]any{"id": i + 1, "name": "音乐会", "typeId": "2", "status": 1}
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"records": records, "total": "21"}})
			} else {
				fmt.Fprint(w, `{"success":true,"data":{"records":[{"id":"21","typeId":"3","name":"测试剧目","area":"上海","address":"人民广场剧院","status":1,"avgPrice":"28000"}],"total":21}}`)
			}
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	for _, query := range []string{"话剧", "上海", "人民广场"} {
		shows, err := client.SearchShows(context.Background(), query, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(shows) != 1 || shows[0].ID != 21 || shows[0].TypeName != "话剧" || shows[0].AvgPrice != 280 {
			t.Fatalf("query %q: %+v", query, shows)
		}
	}
	if pages != 6 {
		t.Fatalf("pagination calls=%d", pages)
	}
}

func TestExpiredAndOfflineShowsAreExcluded(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/show-type/list" {
			fmt.Fprint(w, `{"success":true,"data":[]}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"data":{"records":[{"id":"1","status":0},{"id":"2","status":1,"endTime":"2000-01-01T20:00:00"},{"id":"3","status":1,"endTime":"2099-01-01T20:00:00"}],"total":"3"}}`)
	})
	shows, err := client.SearchShows(context.Background(), "", 20)
	if err != nil || len(shows) != 1 || shows[0].ID != 3 {
		t.Fatalf("shows=%+v err=%v", shows, err)
	}
}

func TestSeckillStockAndActivityAreIndependent(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		stock      int
		begin, end time.Time
		active     bool
	}{
		{"running", 8, now.Add(-time.Hour), now.Add(time.Hour), true},
		{"upcoming", 8, now.Add(time.Hour), now.Add(2 * time.Hour), false},
		{"ended", 8, now.Add(-2 * time.Hour), now.Add(-time.Hour), false},
		{"sold_out", 0, now.Add(-time.Hour), now.Add(time.Hour), false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			stock := item.stock
			tier := (apiTicket{ID: "1", ShowID: "2", Type: 1, OnSale: true, Stock: 0, SoldOut: true, SeckillStock: &stock, BeginTime: item.begin.Format(time.RFC3339), EndTime: item.end.Format(time.RFC3339), PayValueInYuan: 199}).toTicketTier("测试演出")
			if tier.Stock != stock || tier.SoldOut != (stock == 0) || tier.SeckillActive != item.active || !tier.IsSeckill || tier.Price != 199 {
				t.Fatalf("tier=%+v", tier)
			}
		})
	}
}

func TestRemainingStockZeroIsNotReplacedByTotal(t *testing.T) {
	var record apiTicket
	err := json.Unmarshal([]byte(`{"id":1,"showId":"2","status":1,"stock":100,"remainingStock":0,"payValueInYuan":268}`), &record)
	if err != nil {
		t.Fatal(err)
	}
	tier := record.toTicketTier("测试")
	if !tier.SoldOut || tier.Stock != 0 || tier.Price != 268 {
		t.Fatalf("tier=%+v", tier)
	}
}

func TestTicketQueriesRejectOfflineShowAndFilterOfflineTier(t *testing.T) {
	offline := false
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/show/") {
			status := 1
			if offline {
				status = 0
			}
			fmt.Fprintf(w, `{"success":true,"data":{"id":"2","name":"演出","status":%d}}`, status)
			return
		}
		fmt.Fprint(w, `{"success":true,"data":[{"id":"1","showId":"2","status":0,"stock":20},{"id":"3","showId":"2","status":1,"stock":20,"remainingStock":8,"payValueInYuan":280}]}`)
	})
	tiers, err := client.ListTickets(context.Background(), 2)
	if err != nil || len(tiers) != 1 || tiers[0].ID != 3 {
		t.Fatalf("tiers=%+v err=%v", tiers, err)
	}
	offline = true
	if _, err = client.ListTickets(context.Background(), 2); err == nil {
		t.Fatal("offline show must not expose tickets")
	}
}

func TestHTTPBusinessFailureIsNotEmptySuccess(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"message":"unavailable","data":null}`)
	})
	if _, err := client.SearchShows(context.Background(), "", 5); err == nil {
		t.Fatal("expected error")
	}
}
