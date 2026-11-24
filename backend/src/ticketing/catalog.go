package ticketing

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Review struct {
	Purchased     bool           `json:"purchased"`
	Verified      bool           `json:"verified"`
	EvidenceKnown bool           `json:"evidence_known"`
	SeatArea      string         `json:"seat_area"`
	Dimensions    map[string]int `json:"dimensions"`
	ID            int64          `json:"review_id,string"`
	ShowID        int64          `json:"show_id,string"`
	Title         string         `json:"title"`
	Content       string         `json:"content"`
	Score         float64        `json:"score"`
	Author        string         `json:"author"`
	Likes         int            `json:"likes"`
	Comments      int            `json:"comments"`
	CreatedAt     string         `json:"created_at"`
	DetailURL     string         `json:"detail_url"`
}

type RatingSummary struct {
	Average       float64 `json:"average"`
	Count         int     `json:"count"`
	Total         int     `json:"total"`
	Distribution  [5]int  `json:"distribution"`
	PositiveRate  float64 `json:"positive_rate"`
	AdjustedScore float64 `json:"adjusted_score"`
	Complete      bool    `json:"complete"`
	Available     bool    `json:"available"`
	TextCount     int     `json:"text_count"`
	LowSample     bool    `json:"low_sample"`
}

type ReviewReport struct {
	Insights ReviewInsights `json:"insights"`
	Items    []Review       `json:"items"`
	Summary  RatingSummary  `json:"summary"`
	Positive []Review       `json:"positive"`
	Critical []Review       `json:"critical"`
	Warnings []string       `json:"warnings"`
}

type CatalogCoverage struct {
	PublicTotal int      `json:"public_total"`
	Loaded      int      `json:"loaded"`
	Active      int      `json:"active"`
	Ended       int      `json:"ended"`
	ReviewCount int      `json:"review_count"`
	RatedCount  int      `json:"rated_count"`
	Complete    bool     `json:"complete"`
	Warnings    []string `json:"warnings"`
	UpdatedAt   string   `json:"updated_at"`
}

type CatalogSnapshot struct {
	Items      []Show                 `json:"items"`
	Categories []string               `json:"categories"`
	Cities     []string               `json:"cities"`
	Coverage   CatalogCoverage        `json:"coverage"`
	Reviews    map[int64]ReviewReport `json:"-"`
}

type apiReview struct {
	ID         wireString `json:"id"`
	ShowID     wireString `json:"showId"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	Score      float64    `json:"score"`
	UserName   string     `json:"userName"`
	Liked      int        `json:"liked"`
	Comments   int        `json:"comments"`
	CreateTime string     `json:"createTime"`
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)
var breakTagPattern = regexp.MustCompile(`(?i)<br\s*/?>|</(?:p|div|li|h[1-6])\s*>`)

// Derive city choices only from places actually present in the public catalog.
var guideCities = []string{"北京", "上海", "广州", "深圳", "杭州", "成都", "南京", "武汉", "天津", "重庆", "西安", "长沙", "苏州", "郑州", "青岛", "厦门", "福州", "合肥", "济南", "沈阳", "大连", "昆明", "南昌", "宁波", "无锡", "佛山", "东莞", "珠海", "海口", "南宁", "贵阳", "太原", "石家庄", "哈尔滨", "长春", "香港", "澳门", "首尔"}

// cityName keeps only area values that read as a city, so district and placeholder
// values ("朝阳区1", "123", "市区") do not leak into the city filter.
func cityName(area string) string {
	area = strings.TrimSpace(area)
	if !strings.HasSuffix(area, "市") {
		return ""
	}
	name := strings.TrimSuffix(area, "市")
	runes := []rune(name)
	if len(runes) < 2 || len(runes) > 8 {
		return ""
	}
	for _, r := range runes {
		if r < 0x4e00 || r > 0x9fff {
			return ""
		}
	}
	return name
}

func plainText(value string) string {
	return strings.TrimSpace(html.UnescapeString(tagPattern.ReplaceAllString(breakTagPattern.ReplaceAllString(value, "\n"), "")))
}

func Lifecycle(show Show) string {
	if showEnded(show.EndTime) {
		return "ended"
	}
	if start, ok := parseBusinessTime(show.OpenTime); ok {
		if start.After(time.Now()) {
			return "upcoming"
		}
		return "live"
	}
	return "unknown"
}

func resolveImage(base, value string) string {
	var images []string
	if json.Unmarshal([]byte(value), &images) == nil {
		if len(images) == 0 {
			return ""
		}
		value = images[0]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	origin, err := url.Parse(base + "/")
	if err != nil {
		return ""
	}
	return origin.ResolveReference(parsed).String()
}

// Fetch every public page; failure or repeated pages are reported instead of silently returning a prefix.
func (c *Client) publicShows(ctx context.Context) ([]Show, int, []string, error) {
	var types apiEnvelope[[]struct {
		ID   wireString `json:"id"`
		Name string     `json:"name"`
	}]
	warnings := []string{}
	names := map[string]string{}
	if err := c.getJSON(ctx, "/show-type/list", nil, &types); err != nil || !types.Success {
		warnings = append(warnings, "演出类型暂未读取完整")
	} else {
		for _, item := range types.Data {
			names[string(item.ID)] = item.Name
		}
	}
	items := make([]Show, 0)
	seen := map[int64]bool{}
	total := 0
	for page := 1; page <= 1000; page++ {
		query := url.Values{"current": {strconv.Itoa(page)}, "size": {"50"}, "sortBy": {"id"}, "sortOrder": {"asc"}}
		var envelope apiEnvelope[apiShowPage]
		if err := c.getJSON(ctx, "/show/of/name", query, &envelope); err != nil {
			return nil, 0, nil, err
		}
		if !envelope.Success {
			return nil, 0, nil, fmt.Errorf("public shows rejected: %s", envelope.Message)
		}
		total = max(total, int(parseInt64(envelope.Data.Total)))
		added := 0
		for _, record := range envelope.Data.Records {
			id := parseInt64(record.ID)
			if id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			added++
			if record.Status != nil && *record.Status != 1 {
				continue
			}
			if record.TypeName == "" {
				record.TypeName = names[string(record.TypeID)]
			}
			show := record.toShow()
			show.PosterURL = resolveImage(c.apiBase, record.Images)
			show.Description = plainText(record.Description)
			show.Lifecycle = Lifecycle(show)
			items = append(items, show)
		}
		if total > 0 && len(seen) >= total {
			return items, total, warnings, nil
		}
		if len(envelope.Data.Records) == 0 || added == 0 {
			if total > len(seen) {
				warnings = append(warnings, "Echo 分页未返回全部记录，请刷新重试")
			}
			return items, total, warnings, nil
		}
		if total == 0 && len(envelope.Data.Records) < 50 {
			return items, len(items), warnings, nil
		}
	}
	warnings = append(warnings, "演出超过本次同步范围，当前结果不完整")
	return items, total, warnings, nil
}

func (c *Client) fetchReviews(ctx context.Context, showID int64) ReviewReport {
	result := ReviewReport{Items: []Review{}, Positive: []Review{}, Critical: []Review{}, Warnings: []string{}}
	result.Summary.Available = true
	seen := map[int64]bool{}
	for page := 1; page <= 2000; page++ {
		query := url.Values{"showId": {strconv.FormatInt(showID, 10)}, "current": {strconv.Itoa(page)}, "size": {"50"}}
		var envelope apiEnvelope[struct {
			Records []apiReview `json:"records"`
			Total   wireString  `json:"total"`
		}]
		if err := c.getJSON(ctx, "/show-review/of/show", query, &envelope); err != nil || !envelope.Success {
			result.Summary.Available = len(result.Items) > 0
			result.Warnings = append(result.Warnings, "观众评测暂未读取完整，可稍后刷新")
			break
		}
		result.Summary.Total = max(result.Summary.Total, int(parseInt64(envelope.Data.Total)))
		added := 0
		for _, item := range envelope.Data.Records {
			id := parseInt64(item.ID)
			if id <= 0 || seen[id] || parseInt64(item.ShowID) != showID {
				continue
			}
			seen[id] = true
			added++
			result.Items = append(result.Items, item.toReview())
		}
		if len(result.Items) >= result.Summary.Total && result.Summary.Total > 0 {
			result.Summary.Complete = true
			break
		}
		if len(envelope.Data.Records) == 0 || added == 0 || (result.Summary.Total == 0 && len(envelope.Data.Records) < 50) {
			result.Summary.Complete = result.Summary.Total == 0 || len(result.Items) >= result.Summary.Total
			break
		}
	}
	return summarizeReviews(c.enrichReviews(ctx, showID, result))
}

func (item apiReview) toReview() Review {
	author := strings.TrimSpace(item.UserName)
	if author == "" {
		author = "Echo 观众"
	}
	return Review{ID: parseInt64(item.ID), ShowID: parseInt64(item.ShowID), Title: plainText(item.Title), Content: plainText(item.Content), Score: item.Score, Author: author, Likes: item.Liked, Comments: item.Comments, CreatedAt: item.CreateTime, DetailURL: strings.TrimSuffix(ShowURL(0), "/show/0") + "/review/" + string(item.ID)}
}

func summarizeReviews(result ReviewReport) ReviewReport {
	result.Insights = reviewInsights(result.Items)
	result.Summary.Total = max(result.Summary.Total, len(result.Items))
	sum := 0.0
	positive := 0
	for _, review := range result.Items {
		if review.Title != "" || review.Content != "" {
			result.Summary.TextCount++
		}
		if review.Score >= 1 && review.Score <= 5 && review.Score == float64(int(review.Score)) {
			result.Summary.Distribution[int(review.Score)-1]++
			result.Summary.Count++
			sum += review.Score
			if review.Score >= 4 {
				positive++
			}
		}
		if review.Content == "" {
			continue
		}
		if review.Score >= 4 && review.Score <= 5 && review.Score == float64(int(review.Score)) {
			result.Positive = append(result.Positive, review)
		}
		if review.Score >= 1 && review.Score <= 3 && review.Score == float64(int(review.Score)) {
			result.Critical = append(result.Critical, review)
		}
	}
	if result.Summary.Count > 0 {
		count := float64(result.Summary.Count)
		result.Summary.Average = sum / count
		result.Summary.PositiveRate = float64(positive) / count
		result.Summary.AdjustedScore = (sum + 5*3.5) / (count + 5)
	}
	result.Summary.LowSample = result.Summary.Count < 5
	sort.SliceStable(result.Positive, func(i, j int) bool { return result.Positive[i].Likes > result.Positive[j].Likes })
	sort.SliceStable(result.Critical, func(i, j int) bool { return result.Critical[i].Score < result.Critical[j].Score })
	if len(result.Positive) > 3 {
		result.Positive = result.Positive[:3]
	}
	if len(result.Critical) > 3 {
		result.Critical = result.Critical[:3]
	}
	if !result.Summary.Complete && len(result.Warnings) == 0 {
		result.Warnings = append(result.Warnings, "当前评分统计仅覆盖已读取的评测")
	}
	return result
}

// Catalog caches public snapshots for 30 seconds and reports any partial sources.
func (c *Client) Catalog(ctx context.Context, refresh bool) (CatalogSnapshot, error) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if !refresh && c.catalogSnapshot != nil && time.Now().Before(c.catalogExpires) {
		return *c.catalogSnapshot, nil
	}
	shows, total, warnings, err := c.publicShows(ctx)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	result := CatalogSnapshot{Items: shows, Categories: []string{}, Cities: []string{}, Reviews: map[int64]ReviewReport{}, Coverage: CatalogCoverage{PublicTotal: total, Loaded: len(shows), Warnings: warnings, Complete: len(shows) == total, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}}
	reports := make([]ReviewReport, len(shows))
	tickets := make([][]TicketTier, len(shows))
	ticketErrors := make([]error, len(shows))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				reports[index] = c.fetchReviews(ctx, shows[index].ID)
				tickets[index], ticketErrors[index] = c.ticketRecords(ctx, shows[index].ID, shows[index].Name)
			}
		}()
	}
	for index := range shows {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return CatalogSnapshot{}, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	categories := map[string]bool{}
	cities := map[string]bool{}
	for index := range result.Items {
		show := &result.Items[index]
		show.Rating = reports[index].Summary
		result.Reviews[show.ID] = reports[index]
		result.Coverage.ReviewCount += len(reports[index].Items)
		result.Coverage.RatedCount += show.Rating.Count
		if !show.Rating.Complete {
			result.Coverage.Warnings = append(result.Coverage.Warnings, fmt.Sprintf("《%s》评测未读取完整", show.Name))
		}
		show.TicketsKnown = ticketErrors[index] == nil
		if ticketErrors[index] != nil {
			result.Coverage.Warnings = append(result.Coverage.Warnings, fmt.Sprintf("《%s》票档暂未读取", show.Name))
		}
		for _, tier := range tickets[index] {
			if !tier.OnSale {
				continue
			}
			show.TicketCount++
			if show.MinPrice == nil || tier.Price < *show.MinPrice {
				price := tier.Price
				show.MinPrice = &price
			}
			if show.MaxPrice == nil || tier.Price > *show.MaxPrice {
				price := tier.Price
				show.MaxPrice = &price
			}
			if !tier.SoldOut {
				show.AvailableStock += tier.Stock
			}
			if tier.IsSeckill {
				show.HasSeckill = true
			}
		}
		if show.Lifecycle == "ended" {
			result.Coverage.Ended++
		} else {
			result.Coverage.Active++
		}
		if show.TypeName != "" {
			categories[show.TypeName] = true
		}
		// Only real city names become filter options; upstream area values also carry
		// districts, venue text and even digits, which made the dropdown unusable.
		if city := cityName(show.Area); city != "" {
			cities[city] = true
		}
		for _, city := range guideCities {
			if strings.Contains(show.Area+show.Address, city) {
				cities[city] = true
			}
		}
	}
	for value := range categories {
		result.Categories = append(result.Categories, value)
	}
	for value := range cities {
		result.Cities = append(result.Cities, value)
	}
	sort.Strings(result.Categories)
	sort.Strings(result.Cities)
	c.catalogSnapshot = &result
	c.catalogExpires = time.Now().Add(30 * time.Second)
	if len(result.Coverage.Warnings) > 0 {
		c.catalogExpires = time.Now().Add(5 * time.Second)
	}
	return result, nil
}

func (c *Client) ticketRecords(ctx context.Context, showID int64, showName string) ([]TicketTier, error) {
	var envelope apiEnvelope[[]apiTicket]
	if err := c.getJSON(ctx, "/ticket/list/"+strconv.FormatInt(showID, 10), nil, &envelope); err != nil {
		return nil, err
	}
	if !envelope.Success {
		return nil, fmt.Errorf("ticket list rejected")
	}
	items := make([]TicketTier, 0, len(envelope.Data))
	for _, record := range envelope.Data {
		if parseInt64(record.ShowID) == 0 {
			record.ShowID = wireString(strconv.FormatInt(showID, 10))
		}
		tier := record.toTicketTier(showName)
		if tier.OnSale {
			items = append(items, tier)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Price < items[j].Price })
	return items, nil
}

func (c *Client) Reviews(ctx context.Context, showID int64, refresh bool) (ReviewReport, error) {
	snapshot, err := c.Catalog(ctx, refresh)
	if err != nil {
		return ReviewReport{}, err
	}
	report, ok := snapshot.Reviews[showID]
	if !ok {
		return ReviewReport{}, fmt.Errorf("public show not found")
	}
	return report, nil
}
