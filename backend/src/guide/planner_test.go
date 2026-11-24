package guide

import (
	"encoding/json"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
	"net/url"
	"testing"
	"time"
)

func TestConversationRetainsTotalBudgetAndAcceptsCorrections(t *testing.T) {
	snapshot := ticketing.CatalogSnapshot{Cities: []string{"上海", "杭州"}, Categories: []string{"话剧", "音乐会"}}
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	p := resolvePlan(assistRequest{Message: "上海周六，两个人，总预算 600 元，看话剧"}, snapshot, now)
	if p.PartySize != 2 || p.Budget != 600 || p.Filters.Budget != 300 || p.Filters.City != "上海" || p.Filters.Category != "话剧" || p.Filters.DateFrom != "2026-11-21" || p.Filters.DateTo != "2026-11-21" {
		t.Fatalf("lost constraints: %+v", p)
	}
	p = resolvePlan(assistRequest{Plan: &p, Message: "想坐近一点"}, snapshot, now)
	if p.SeatPreference != "front" || p.Filters.Budget != 300 || p.Filters.DateFrom != "2026-11-21" {
		t.Fatalf("followup lost constraints: %+v", p)
	}
	p = resolvePlan(assistRequest{Plan: &p, Budget: 100, Message: "改成三个人，总预算改为 900 元，换到杭州，下周六"}, snapshot, now)
	if p.PartySize != 3 || p.Budget != 900 || p.Filters.Budget != 300 || p.Filters.City != "杭州" || p.Filters.DateFrom != "2026-11-28" {
		t.Fatalf("correction did not win: %+v", p)
	}
	p = resolvePlan(assistRequest{Plan: &p, Message: "不限预算，不限日期，不限座位"}, snapshot, now)
	if p.Budget != 0 || p.Filters.Budget != 0 || p.SeatPreference != "" || p.Filters.DateFrom != "" || p.Filters.DateTo != "" {
		t.Fatalf("clear preferences failed: %+v", p)
	}
}

func TestPartyBudgetAndAvailabilityCarryIntoCheckout(t *testing.T) {
	p := shoppingPlan{PartySize: 2, Budget: 600, BudgetMode: "total", SeatPreference: "adjacent", Filters: discoveryFilters{Budget: 300}}
	base := ticketing.TicketTier{ID: 9007199254740993, Price: 280, Stock: 2, OnSale: true, HasSeats: true, CheckoutURL: "http://localhost/guide/checkout/9007199254740993"}
	shortage := base
	shortage.Stock = 1
	expensive := base
	expensive.Price = 320
	unseated := base
	unseated.HasSeats = false
	flash := base
	flash.IsSeckill = true
	flash.SeckillActive = true
	result := plannedTickets([]ticketing.TicketTier{shortage, expensive, unseated, flash, base}, p)
	if len(result) != 1 {
		t.Fatalf("recommended impossible tickets: %+v", result)
	}
	u, _ := url.Parse(result[0].CheckoutURL)
	if u.Path != "/guide/checkout/9007199254740993" || u.Query().Get("count") != "2" || u.Query().Get("budget") != "600.00" || u.Query().Get("seatPreference") != "adjacent" {
		t.Fatalf("lost checkout intent: %s", u)
	}
	if base.CheckoutURL != "http://localhost/guide/checkout/9007199254740993" {
		t.Fatal("mutated cached ticket")
	}
}

func TestPerPersonBudgetAndShanghaiWeekend(t *testing.T) {
	p := resolvePlan(assistRequest{Message: "两个人，每人预算 300 元，这周末"}, ticketing.CatalogSnapshot{}, time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC))
	if p.BudgetMode != "per_person" || p.Filters.Budget != 300 || p.Filters.DateFrom != "2026-11-21" || p.Filters.DateTo != "2026-11-22" {
		t.Fatalf("wrong local date or budget: %+v", p)
	}
}

func TestInvalidPartyAndDateAreRejectedByAssistant(t *testing.T) {
	for _, message := range []string{"0个人预算600元", "11个人预算600元", "2026-02-30 两个人"} {
		body, _ := json.Marshal(map[string]string{"message": message})
		response := requestRich(t, richFixture(), "POST", "/assist", string(body))
		if response.Code != 400 {
			t.Fatalf("%s returned %d: %s", message, response.Code, response.Body.String())
		}
	}
}
