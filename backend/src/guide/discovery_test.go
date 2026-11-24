package guide

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

type discoveryCatalog struct {
	testCatalog
	snapshot ticketing.CatalogSnapshot
}

func (f *discoveryCatalog) GlobalReviews(context.Context, bool) (ticketing.ReviewReport, error) {
	return ticketing.ReviewReport{Items: []ticketing.Review{{ID: 101, ShowID: 1, Title: "舞台很棒", Score: 5, Author: "观众甲", CreatedAt: "2026-01-01"}, {ID: 999, ShowID: 13, Score: 2, Author: "观众乙", Content: "声音很响", CreatedAt: "2026-01-02"}}, Summary: ticketing.RatingSummary{Total: 2, Count: 2, Available: true, Complete: true}, Warnings: []string{}}, nil
}

func (f *discoveryCatalog) Catalog(context.Context, bool) (ticketing.CatalogSnapshot, error) {
	return f.snapshot, nil
}
func (f *discoveryCatalog) Reviews(_ context.Context, id int64, _ bool) (ticketing.ReviewReport, error) {
	v, ok := f.snapshot.Reviews[id]
	if !ok {
		return v, fmt.Errorf("not public")
	}
	return v, nil
}

func richFixture() *discoveryCatalog {
	price := 280.0
	substantial := ticketing.RatingSummary{Average: 4, Count: 10, Total: 10, AdjustedScore: 57.5 / 15, Complete: true, Available: true, PositiveRate: .8}
	small := ticketing.RatingSummary{Average: 5, Count: 1, Total: 1, AdjustedScore: 22.5 / 6, Complete: true, Available: true, LowSample: true, PositiveRate: 1}
	noRatings := ticketing.RatingSummary{Complete: true, Available: true, LowSample: true}
	return &discoveryCatalog{snapshot: ticketing.CatalogSnapshot{
		Items: []ticketing.Show{
			{ID: 1, Name: "历史话剧", TypeName: "话剧", Area: "黄浦区", Address: "上海大剧院", Lifecycle: "ended", OpenTime: "2000-01-01 20:00", EndTime: "2000-01-01 22:00", Score: 5, Rating: substantial, MinPrice: &price, TicketsKnown: true, AvailableStock: 8},
			{ID: 2, Name: "未来话剧", TypeName: "话剧", Area: "黄浦区", Address: "上海剧院", Lifecycle: "upcoming", OpenTime: "2099-01-01 20:00", EndTime: "2099-01-01 22:00", Score: 5, Rating: small, MinPrice: &price, TicketsKnown: true, AvailableStock: 8},
			{ID: 3, Name: "北京音乐会", TypeName: "音乐会", Area: "朝阳区", Address: "北京音乐厅", Lifecycle: "upcoming", OpenTime: "2099-02-01 20:00", EndTime: "2099-02-01 22:00", Rating: noRatings, MinPrice: &price, TicketsKnown: true, AvailableStock: 8},
		}, Categories: []string{"话剧", "音乐会"}, Cities: []string{"上海", "北京", "黄浦区"}, Coverage: ticketing.CatalogCoverage{PublicTotal: 3, Loaded: 3, Active: 2, Ended: 1, ReviewCount: 11, Complete: true, Warnings: []string{}},
		Reviews: map[int64]ticketing.ReviewReport{
			1: {Summary: substantial, Items: []ticketing.Review{}, Positive: []ticketing.Review{{ID: 101, ShowID: 1, Content: "舞台很棒", Author: "观众甲", Score: 5}}, Critical: []ticketing.Review{{ID: 102, ShowID: 1, Content: "声音偏大", Author: "观众乙", Score: 2}}, Warnings: []string{}},
			2: {Summary: small, Items: []ticketing.Review{{ID: 201, ShowID: 2, Score: 5}}, Positive: []ticketing.Review{}, Critical: []ticketing.Review{}, Warnings: []string{}},
			3: {Summary: noRatings, Items: []ticketing.Review{}, Positive: []ticketing.Review{}, Critical: []ticketing.Review{}, Warnings: []string{}},
		},
	}}
}

func requestRich(t *testing.T, fixture *discoveryCatalog, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	out := httptest.NewRecorder()
	NewHandler(fixture, llm.Config{}).ServeHTTP(out, httptest.NewRequest(method, "/api/v1/ticketing"+path, strings.NewReader(body)))
	return out
}

func TestDiscoveryIncludesHistoryAndPaginatesWithFacets(t *testing.T) {
	fixture := richFixture()
	for _, test := range []struct {
		query        string
		total, first int
	}{
		{"?page_size=2", 3, 2}, {"?page_size=2&page=2", 3, 1}, {"?lifecycle=ended", 1, 1}, {"?lifecycle=active&city=上海", 1, 2}, {"?available_only=true", 2, 2}, {"?budget=200", 0, 0}, {"?category=话剧&min_score=4.5", 1, 2}, {"?date_from=2099-02-01", 1, 3}, {"?ids=1,3&sort=soon", 2, 3}, {"?ids=0", 0, 0},
	} {
		t.Run(test.query, func(t *testing.T) {
			response := requestRich(t, fixture, "GET", "/shows"+test.query, "")
			var result struct {
				Items              []ticketing.Show
				Total              int
				Categories, Cities []string
				Coverage           ticketing.CatalogCoverage
			}
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total != test.total || test.first > 0 && (len(result.Items) == 0 || result.Items[0].ID != int64(test.first)) || len(result.Categories) != 2 || len(result.Cities) != 3 || result.Coverage.Loaded != 3 {
				t.Fatal(response.Body.String())
			}
		})
	}
	if fixture.snapshot.Items[0].ID != 1 {
		t.Fatal("filter sorting mutated the cached snapshot")
	}
}

func TestDiscoveryRejectsInvalidFiltersAndComparisons(t *testing.T) {
	fixture := richFixture()
	for _, path := range []string{"/shows?budget=NaN", "/shows?min_score=NaN", "/shows?page=0", "/shows?page_size=101", "/shows?lifecycle=made-up", "/shows?date_from=2099-02-30", "/shows?date_from=2099-02-01&date_to=2000-01-01", "/shows?ids=x", "/compare?ids=1", "/compare?ids=1,1", "/compare?ids=1,2,3,4,5"} {
		response := requestRich(t, fixture, "GET", path, "")
		if response.Code != 400 {
			t.Fatalf("%s returned %d: %s", path, response.Code, response.Body.String())
		}
	}
	if r := requestRich(t, fixture, "GET", "/compare?ids=1,999", ""); r.Code != 404 {
		t.Fatal(r.Body.String())
	}
}

func TestReputationRankingUsesSampleSizeAndOmitsUnrated(t *testing.T) {
	response := requestRich(t, richFixture(), "GET", "/rankings", "")
	var result struct {
		Items  []ticketing.Show
		Method string
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	if response.Code != 200 || len(result.Items) != 2 || result.Items[0].ID != 1 || result.Items[0].Rating.Average != 4 || result.Items[1].Rating.Average != 5 || result.Method == "" {
		t.Fatal(response.Body.String())
	}
}

func TestEndedDetailAndComparisonRetainReviewsWithoutPurchaseClaim(t *testing.T) {
	response := requestRich(t, richFixture(), "GET", "/shows/1/detail", "")
	var detail struct {
		Purchasable bool
		Reviews     ticketing.ReviewReport
		Items       []ticketing.TicketTier
	}
	json.Unmarshal(response.Body.Bytes(), &detail)
	if response.Code != 200 || detail.Purchasable || detail.Reviews.Summary.Count != 10 || len(detail.Items) == 0 {
		t.Fatal(response.Body.String())
	}
	response = requestRich(t, richFixture(), "GET", "/compare?ids=1,2", "")
	var comparison struct{ Items []comparisonEntry }
	json.Unmarshal(response.Body.Bytes(), &comparison)
	if response.Code != 200 || len(comparison.Items) != 2 || !strings.Contains(strings.Join(comparison.Items[0].Cautions, ","), "已结束") || !strings.Contains(strings.Join(comparison.Items[1].Cautions, ","), "样本较少") || len(comparison.Items[1].Reviews.Positive) != 0 {
		t.Fatal(response.Body.String())
	}
}

func TestReviewAssistantQuotesEvidenceAndLabelsHistory(t *testing.T) {
	response := requestRich(t, richFixture(), "POST", "/assist", `{"message":"比较这两场的口碑","compare_ids":["1","2"]}`)
	var result struct {
		Answer, Mode, Intent string
		Items                []comparisonEntry
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	if response.Code != 200 || result.Mode != "catalog" || result.Intent != "compare" || len(result.Items) != 2 {
		t.Fatal(response.Body.String())
	}
	for _, fact := range []string{"评测均分 4.00/5", "10 个有效评分", "Echo 演出展示分：5.0/5", "#101", "观众甲", "舞台很棒", "声音偏大", "已结束", "样本较少"} {
		if !strings.Contains(result.Answer, fact) {
			t.Fatalf("answer missing %q: %s", fact, result.Answer)
		}
	}
	if len(result.Items[1].Reviews.Positive) > 0 {
		t.Fatal("invented text for a pure score")
	}
}

func TestAssistantRespectsCityBudgetAndAvailableReviews(t *testing.T) {
	for _, body := range []string{
		`{"message":"上海话剧，500元以内，推荐能买票的演出"}`,
		`{"message":"口碑好而且能买票的上海话剧","budget":500}`,
		`{"message":"推荐一场","history":[{"role":"user","content":"我想在上海看话剧，预算500元"}]}`,
	} {
		response := requestRich(t, richFixture(), "POST", "/assist", body)
		var result struct{ Items []comparisonEntry }
		json.Unmarshal(response.Body.Bytes(), &result)
		if response.Code != 200 || len(result.Items) != 1 || result.Items[0].Show.ID != 2 {
			t.Fatal(response.Body.String())
		}
		for _, tier := range result.Items[0].Tickets {
			if tier.Price > 500 || tier.SoldOut {
				t.Fatal("budget or availability ignored")
			}
		}
	}
}

func TestReputationRankingPutsSampleSizeBeforeAOneStarFive(t *testing.T) {
	price := 200.0
	one := ticketing.RatingSummary{Average: 5, Count: 1, Total: 1, AdjustedScore: 22.5 / 6, Complete: true, Available: true, LowSample: true, PositiveRate: 1}
	three := ticketing.RatingSummary{Average: 4, Count: 3, Total: 3, AdjustedScore: 29.5 / 8, Complete: true, Available: true, LowSample: true, PositiveRate: 1}
	if one.AdjustedScore <= three.AdjustedScore {
		t.Fatal("fixture should make the single five-star win on adjusted score alone")
	}
	shows := []ticketing.Show{
		{ID: 11, Name: "只有一个五星", Lifecycle: "upcoming", Rating: one, MinPrice: &price},
		{ID: 12, Name: "三个四星", Lifecycle: "upcoming", Rating: three, MinPrice: &price},
	}
	if ranked := filterShows(shows, discoveryFilters{Sort: "reputation"}); ranked[0].ID != 12 {
		t.Fatalf("a single five-star review led the reputation list: %d", ranked[0].ID)
	}
	if byAverage := filterShows(shows, discoveryFilters{Sort: "rating"}); byAverage[0].ID != 11 {
		t.Fatal("explicit average sorting should stay unchanged")
	}
}

func TestAssistantOffersLabelledAlternativesInsteadOfDeadEnd(t *testing.T) {
	response := requestRich(t, richFixture(), "POST", "/assist", `{"message":"2099-03-05，两个人，总预算 600 元，推荐能买票的演出"}`)
	var result struct {
		Answer       string
		Items        []comparisonEntry
		Alternatives []comparisonEntry
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	if response.Code != 200 || len(result.Items) != 0 || len(result.Alternatives) == 0 {
		t.Fatal(response.Body.String())
	}
	if !strings.Contains(result.Answer, "没有找到可购演出") || !strings.Contains(result.Answer, "备选") {
		t.Fatalf("dead end not explained: %s", result.Answer)
	}
	for _, entry := range result.Alternatives {
		if len(entry.Tickets) == 0 || entry.Show.Lifecycle == "ended" {
			t.Fatal("alternative must stay purchasable")
		}
		if !strings.Contains(strings.Join(entry.Reasons, ","), "不在你要求的日期内") {
			t.Fatal("alternative did not state which condition differs")
		}
	}
}

func TestGlobalReviewEndpointKeepsUnlistedReviewAndFilters(t *testing.T) {
	for _, test := range []struct {
		query        string
		total, first int
	}{{"", 2, 999}, {"?filter=positive", 1, 101}, {"?query=舞台", 1, 101}, {"?filter=text&sort=low", 2, 999}} {
		response := requestRich(t, richFixture(), "GET", "/review-feed"+test.query, "")
		var result struct {
			Items    []reviewFeedEntry
			Total    int
			Unlisted int `json:"unlisted_count"`
		}
		json.Unmarshal(response.Body.Bytes(), &result)
		if response.Code != 200 || result.Total != test.total || len(result.Items) == 0 || result.Items[0].ID != int64(test.first) || result.Unlisted != 1 {
			t.Fatal(response.Body.String())
		}
		if test.first == 999 && (result.Items[0].ShowAvailable || !strings.Contains(result.Items[0].ShowName, "当前未公开")) {
			t.Fatal("unlisted show presented as available")
		}
	}
}
