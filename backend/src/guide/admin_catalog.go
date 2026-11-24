package guide

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

// The console answers "where are the shows and what is actually on sale",
// which the assistant needs to stay honest about coverage and pricing.

type cityRow struct {
	City           string   `json:"city"`
	Shows          int      `json:"shows"`
	Active         int      `json:"active"`
	Ended          int      `json:"ended"`
	Rated          int      `json:"rated"`
	RatingCount    int      `json:"rating_count"`
	Average        float64  `json:"average"`
	Reviews        int      `json:"reviews"`
	MinPrice       *float64 `json:"min_price"`
	MaxPrice       *float64 `json:"max_price"`
	Tiers          int      `json:"tiers"`
	Selectable     int      `json:"selectable"`
	AvailableStock int      `json:"available_stock"`
	MissingTickets int      `json:"missing_tickets"`
	MissingReviews int      `json:"missing_reviews"`
}

type categoryRow struct {
	Name        string `json:"name"`
	Shows       int    `json:"shows"`
	Active      int    `json:"active"`
	Purchasable int    `json:"purchasable"`
}

type ticketSummary struct {
	Tiers           int     `json:"tiers"`
	Purchasable     int     `json:"purchasable"`
	SoldOut         int     `json:"sold_out"`
	Seckill         int     `json:"seckill"`
	Selectable      int     `json:"selectable"`
	NoSeat          int     `json:"no_seat"`
	SnapshotStock   int     `json:"snapshot_stock"`
	MissingRules    int     `json:"missing_rules"`
	EndedTiers      int     `json:"ended_tiers"`
	Stock           int     `json:"stock"`
	AveragePrice    float64 `json:"average_price"`
	MinPrice        float64 `json:"min_price"`
	MaxPrice        float64 `json:"max_price"`
	ShowsNoTickets  int     `json:"shows_without_tickets"`
	SuspiciousPrice int     `json:"suspicious_price"`
}

type priceBand struct {
	Label string  `json:"label"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
	Share float64 `json:"share"`
}

type tierRow struct {
	ShowID        string  `json:"show_id"`
	Show          string  `json:"show"`
	City          string  `json:"city"`
	Lifecycle     string  `json:"lifecycle"`
	Title         string  `json:"title"`
	SubTitle      string  `json:"sub_title"`
	Price         float64 `json:"price"`
	Stock         int     `json:"stock"`
	StockSource   string  `json:"stock_source"`
	HasSeats      bool    `json:"has_seats"`
	IsSeckill     bool    `json:"is_seckill"`
	SeckillActive bool    `json:"seckill_active"`
	SoldOut       bool    `json:"sold_out"`
	OnSale        bool    `json:"on_sale"`
	RulesMissing  bool    `json:"rules_missing"`
}

type catalogAnalytics struct {
	UpdatedAt  string        `json:"updated_at"`
	Cities     []cityRow     `json:"cities"`
	Categories []categoryRow `json:"categories"`
	Summary    ticketSummary `json:"ticket_summary"`
	Bands      []priceBand   `json:"price_bands"`
	Tiers      []tierRow     `json:"tiers"`
	Notes      []string      `json:"notes"`
}

type analyticsCache struct {
	mu   sync.Mutex
	at   time.Time
	data *catalogAnalytics
}

func newAnalyticsCache() *analyticsCache { return &analyticsCache{} }

func (h *Handler) adminCatalog(w http.ResponseWriter, r *http.Request) {
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前数据源不支持城市与票档分析。"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	data, err := h.analytics(ctx, catalog, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	h.write(w, http.StatusOK, data)
}

func (h *Handler) analytics(ctx context.Context, catalog richCatalog, refresh bool) (*catalogAnalytics, error) {
	cache := h.analyticsStore
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !refresh && cache.data != nil && time.Since(cache.at) < 30*time.Second {
		return cache.data, nil
	}
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		return nil, err
	}
	data := aggregateAnalytics(ctx, h.catalog, snapshot)
	if err := ctx.Err(); err != nil {
		// Partial ticket reads must not be cached as if complete.
		return nil, err
	}
	cache.data = data
	cache.at = time.Now()
	return data, nil
}

func aggregateAnalytics(ctx context.Context, catalog Catalog, snapshot ticketing.CatalogSnapshot) *catalogAnalytics {
	data := &catalogAnalytics{Cities: []cityRow{}, Categories: []categoryRow{}, Bands: priceBands(), Tiers: []tierRow{}, Notes: []string{}}

	tiers := make([][]ticketing.TicketTier, len(snapshot.Items))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				list, err := catalog.ListTickets(ctx, snapshot.Items[index].ID)
				if err != nil {
					continue
				}
				tiers[index] = list
			}
		}()
	}
	for index := range snapshot.Items {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return data
		}
	}
	close(jobs)
	workers.Wait()

	cities := map[string]*cityRow{}
	categories := map[string]*categoryRow{}
	bands := make([]priceBand, len(data.Bands))
	copy(bands, data.Bands)
	summary := ticketSummary{}
	priceSum := 0.0
	for index, show := range snapshot.Items {
		city := cityOf(show, snapshot.Cities)
		row, exists := cities[city]
		if !exists {
			row = &cityRow{City: city}
			cities[city] = row
		}
		row.Shows++
		if show.Lifecycle == "ended" {
			row.Ended++
		} else {
			row.Active++
		}
		if show.Rating.Count > 0 {
			row.Rated++
			row.RatingCount += show.Rating.Count
			row.Average += show.Rating.Average * float64(show.Rating.Count)
		}
		row.Reviews += len(snapshot.Reviews[show.ID].Items)
		if !show.Rating.Complete {
			row.MissingReviews++
		}
		if !show.TicketsKnown {
			row.MissingTickets++
			summary.ShowsNoTickets++
		}
		low, high := show.MinPrice, show.MaxPrice
		if low != nil && (row.MinPrice == nil || *low < *row.MinPrice) {
			value := *low
			row.MinPrice = &value
		}
		if high == nil {
			high = low
		}
		if high != nil && (row.MaxPrice == nil || *high > *row.MaxPrice) {
			value := *high
			row.MaxPrice = &value
		}

		category := show.TypeName
		if category == "" {
			category = "未标注类型"
		}
		entry, exists := categories[category]
		if !exists {
			entry = &categoryRow{Name: category}
			categories[category] = entry
		}
		entry.Shows++

		purchasableShow := false
		if len(tiers[index]) == 0 {
			summary.ShowsNoTickets++
			row.MissingTickets++
		}
		for _, tier := range tiers[index] {
			if tier.Price > 0 && tier.Price < 10 {
				summary.SuspiciousPrice++
			}
			summary.Tiers++
			row.Tiers++
			if tier.IsSeckill {
				summary.Seckill++
			}
			if tier.HasSeats {
				summary.Selectable++
				row.Selectable++
			} else {
				summary.NoSeat++
			}
			if tier.StockSource == "mysql_snapshot" {
				summary.SnapshotStock++
			}
			if strings.TrimSpace(tier.Rules) == "" {
				summary.MissingRules++
			}
			if tier.SoldOut {
				summary.SoldOut++
			}
			if show.Lifecycle == "ended" {
				summary.EndedTiers++
			}
			available := tier.OnSale && !tier.SoldOut && show.Lifecycle != "ended"
			if available {
				summary.Purchasable++
				summary.Stock += tier.Stock
				row.AvailableStock += tier.Stock
				purchasableShow = true
			}
			if summary.MinPrice == 0 || tier.Price < summary.MinPrice {
				summary.MinPrice = tier.Price
			}
			if tier.Price > summary.MaxPrice {
				summary.MaxPrice = tier.Price
			}
			priceSum += tier.Price
			for bandIndex := range bands {
				if tier.Price >= bands[bandIndex].Min && tier.Price < bands[bandIndex].Max {
					bands[bandIndex].Count++
					break
				}
			}
			data.Tiers = append(data.Tiers, tierRow{
				ShowID:        strconv.FormatInt(show.ID, 10),
				Show:          show.Name,
				City:          city,
				Lifecycle:     show.Lifecycle,
				Title:         tier.Title,
				SubTitle:      tier.SubTitle,
				Price:         tier.Price,
				Stock:         tier.Stock,
				StockSource:   tier.StockSource,
				HasSeats:      tier.HasSeats,
				IsSeckill:     tier.IsSeckill,
				SeckillActive: tier.SeckillActive,
				SoldOut:       tier.SoldOut,
				OnSale:        tier.OnSale,
				RulesMissing:  strings.TrimSpace(tier.Rules) == "",
			})
		}
		if purchasableShow && show.Lifecycle != "ended" {
			entry.Active++
			entry.Purchasable++
		}
	}

	for _, row := range cities {
		if row.RatingCount > 0 {
			row.Average = math.Round(row.Average/float64(row.RatingCount)*100) / 100
		}
		data.Cities = append(data.Cities, *row)
	}
	sort.Slice(data.Cities, func(i, j int) bool {
		if data.Cities[i].Shows != data.Cities[j].Shows {
			return data.Cities[i].Shows > data.Cities[j].Shows
		}
		return data.Cities[i].City < data.Cities[j].City
	})
	for _, row := range categories {
		data.Categories = append(data.Categories, *row)
	}
	sort.Slice(data.Categories, func(i, j int) bool {
		if data.Categories[i].Shows != data.Categories[j].Shows {
			return data.Categories[i].Shows > data.Categories[j].Shows
		}
		return data.Categories[i].Name < data.Categories[j].Name
	})
	total := 0
	for _, band := range bands {
		total += band.Count
	}
	for index := range bands {
		if total > 0 {
			bands[index].Share = math.Round(float64(bands[index].Count)/float64(total)*1000) / 10
		}
	}
	data.Bands = bands
	if summary.Tiers > 0 {
		summary.AveragePrice = math.Round(priceSum/float64(summary.Tiers)*100) / 100
	}
	data.Summary = summary
	sort.SliceStable(data.Tiers, func(i, j int) bool {
		if data.Tiers[i].Show != data.Tiers[j].Show {
			return data.Tiers[i].Show < data.Tiers[j].Show
		}
		return data.Tiers[i].Price < data.Tiers[j].Price
	})
	if summary.SnapshotStock > 0 {
		data.Notes = append(data.Notes, "部分票档库存来自数据库快照，购票前以 Echo 确认页为准。")
	}
	if summary.MissingRules > 0 {
		data.Notes = append(data.Notes, "存在未填写退改规则的票档，详情页会明确提示“尚未提供规则”。")
	}
	if summary.SuspiciousPrice > 0 {
		data.Notes = append(data.Notes, fmt.Sprintf("有 %d 个票档价格低于 ¥10，可能是测试或占位数据，建议核对后再对外推荐。", summary.SuspiciousPrice))
	}
	if summary.Stock == 0 {
		data.Notes = append(data.Notes, "当前没有可售余票，推荐会自动转为历史回顾或备选。")
	}
	data.UpdatedAt = snapshot.Coverage.UpdatedAt
	return data
}

func cityOf(show ticketing.Show, cities []string) string {
	haystack := show.Area + show.Address
	best := ""
	for _, city := range cities {
		if city == "" || strings.HasSuffix(city, "区") || strings.HasSuffix(city, "县") || strings.HasSuffix(city, "镇") {
			continue
		}
		if strings.Contains(haystack, city) && len([]rune(city)) > len([]rune(best)) {
			best = city
		}
	}
	if best == "" {
		return "未标注城市"
	}
	return best
}

func priceBands() []priceBand {
	return []priceBand{
		{Label: "¥0–99", Min: 0, Max: 100},
		{Label: "¥100–199", Min: 100, Max: 200},
		{Label: "¥200–499", Min: 200, Max: 500},
		{Label: "¥500–999", Min: 500, Max: 1000},
		{Label: "¥1000 以上", Min: 1000, Max: math.MaxFloat64},
	}
}
