package ticketing

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicCatalogReadsBeyondOldPageLimitAndRetainsHistory(t *testing.T) {
	calls := 0
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected write: %s", r.Method)
		}
		if r.URL.Path == "/show-type/list" {
			fmt.Fprint(w, `{"success":true,"data":[{"id":"3","name":"话剧"}]}`)
			return
		}
		calls++
		page, _ := strconv.Atoi(r.URL.Query().Get("current"))
		// Echo may clamp each requested 50-row page to 20 rows.
		records := []map[string]any{}
		for id := (page-1)*20 + 1; id <= min(page*20, 521); id++ {
			records = append(records, map[string]any{"id": strconv.Itoa(id), "typeId": "3", "status": 1, "name": "历史剧目", "endTime": "2000-01-01T20:00:00", "description": "<p>第一段</p><p>" + strings.Repeat("完整介绍", 80) + "</p>", "images": "[\"/poster.jpg\"]"})
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"records": records, "total": "521"}})
	})
	shows, total, warnings, err := client.publicShows(context.Background())
	if err != nil || len(shows) != 521 || total != 521 || calls != 27 || len(warnings) != 0 {
		t.Fatalf("loaded=%d total=%d calls=%d warnings=%v err=%v", len(shows), total, calls, warnings, err)
	}
	last := shows[len(shows)-1]
	if last.ID != 521 || last.TypeName != "话剧" || last.Lifecycle != "ended" || len([]rune(last.Description)) < 300 || strings.Contains(last.Description, "<p>") || !strings.Contains(last.Description, "\n") || !strings.HasSuffix(last.PosterURL, "/poster.jpg") {
		t.Fatalf("lost catalog detail: %+v", last)
	}
}

func TestRepeatedPublicPageIsReportedAsIncomplete(t *testing.T) {
	calls := 0
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/show-type/list" {
			fmt.Fprint(w, `{"success":true,"data":[]}`)
			return
		}
		calls++
		fmt.Fprint(w, `{"success":true,"data":{"records":[{"id":"1","status":1}],"total":"22"}}`)
	})
	shows, total, warnings, err := client.publicShows(context.Background())
	if err != nil || len(shows) != 1 || total != 22 || calls != 2 || len(warnings) == 0 {
		t.Fatalf("%d/%d calls=%d warnings=%v err=%v", len(shows), total, calls, warnings, err)
	}
}

func TestReviewsPageAllScoresAndPreserveEvidence(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/show-review/guide-insights" {
			fmt.Fprint(w, `{"success":true,"data":{"items":[{"reviewId":"2","purchased":true,"verified":false,"seatArea":"A区","dimensions":{"view":4}}]}}`)
			return
		}
		if r.URL.Path != "/show-review/of/show" || r.URL.Query().Get("showId") != "12" || r.Method != "GET" {
			t.Errorf("wrong review request: %s %s", r.Method, r.URL)
		}
		if r.URL.Query().Get("current") == "1" {
			fmt.Fprint(w, `{"success":true,"data":{"total":"4","records":[{"id":"9007199254740993","showId":"12","score":5,"userName":"纯评分用户"},{"id":"2","showId":"12","score":4,"content":"<p>舞台很棒</p>","userName":"甲","liked":8}]}}`)
		} else {
			fmt.Fprint(w, `{"success":true,"data":{"total":4,"records":[{"id":"2","showId":"12","score":4},{"id":"3","showId":"12","score":1,"content":"声音太响","userName":"乙"},{"id":"4","showId":"12","score":0,"title":"无评分记录"},{"id":"5","showId":"99","score":5}]}}`)
		}
	})
	report := client.fetchReviews(context.Background(), 12)
	s := report.Summary
	if !s.Complete || !s.Available || s.Total != 4 || s.Count != 3 || s.Distribution != [5]int{1, 0, 0, 1, 1} || math.Abs(s.Average-10.0/3) > 1e-10 || math.Abs(s.PositiveRate-2.0/3) > 1e-10 || s.TextCount != 3 || !s.LowSample {
		t.Fatalf("incorrect rating aggregation: %+v", s)
	}
	if len(report.Positive) != 1 || report.Positive[0].Content != "舞台很棒" || len(report.Critical) != 1 || report.Critical[0].Content != "声音太响" || report.Items[0].Content != "" || report.Items[0].ID != 9007199254740993 {
		t.Fatalf("invalid review evidence: %+v", report)
	}
	if !report.Items[1].Purchased || report.Items[1].Verified || report.Items[1].SeatArea != "A区" || report.Insights.Dimensions["view"].Average != 4 {
		t.Fatalf("lost authoritative metadata: %+v", report)
	}
	raw, _ := json.Marshal(report.Items[0])
	if !strings.Contains(string(raw), `"review_id":"9007199254740993"`) {
		t.Fatal("review ID lost precision")
	}
}

func TestReviewFailureCannotAppearAsCompleteOrZeroOpinion(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("current") == "1" {
			fmt.Fprint(w, `{"success":true,"data":{"total":"51","records":[{"id":"1","showId":"1","score":5}]}}`)
			return
		}
		http.Error(w, "unavailable", 503)
	})
	report := client.fetchReviews(context.Background(), 1)
	if report.Summary.Complete || !report.Summary.Available || report.Summary.Count != 1 || report.Summary.Total != 51 || len(report.Warnings) == 0 {
		t.Fatalf("hid partial reviews: %+v", report)
	}
}

func TestCatalogEnrichmentCachingAndPartialIsolation(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/show-review/guide-insights":
			fmt.Fprint(w, `{"success":true,"data":{"items":[]}}`)
		case "/show-type/list":
			fmt.Fprint(w, `{"success":true,"data":[{"id":1,"name":"话剧"}]}`)
		case "/show/of/name":
			fmt.Fprint(w, `{"success":true,"data":{"total":"2","records":[{"id":"1","name":"历史好戏","typeId":1,"area":"黄浦区","address":"上海大剧院","status":1,"score":5,"comments":76,"endTime":"2000-01-01T20:00:00"},{"id":"2","name":"未来音乐会","status":1,"endTime":"2099-01-01T20:00:00"}]}}`)
		case "/show-review/of/show":
			if r.URL.Query().Get("showId") == "2" {
				http.Error(w, "down", 503)
				return
			}
			fmt.Fprint(w, `{"success":true,"data":{"total":"2","records":[{"id":1,"showId":1,"score":5},{"id":2,"showId":1,"score":3}]}}`)
		case "/ticket/list/1":
			fmt.Fprint(w, `{"success":true,"data":[{"id":"10","showId":"1","payValueInYuan":280,"remainingStock":8,"onSale":true},{"id":"11","showId":"1","payValueInYuan":10,"remainingStock":99,"onSale":false}]}`)
		case "/ticket/list/2":
			http.Error(w, "down", 503)
		default:
			t.Errorf("unexpected catalog endpoint %s", r.URL)
			http.NotFound(w, r)
		}
	})
	snapshot, err := client.Catalog(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := snapshot.Coverage
	if c.Loaded != 2 || c.PublicTotal != 2 || !c.Complete || c.Ended != 1 || c.Active != 1 || c.ReviewCount != 2 || c.RatedCount != 2 || len(c.Warnings) != 2 {
		t.Fatalf("incorrect coverage: %+v", c)
	}
	show := snapshot.Items[0]
	if show.Score != 5 || show.Rating.Average != 4 || show.Rating.Count != 2 || show.Comments != 76 || show.MinPrice == nil || *show.MinPrice != 280 || show.AvailableStock != 8 || !show.TicketsKnown {
		t.Fatalf("score/price context lost: %+v", show)
	}
	if snapshot.Items[1].Rating.Available || snapshot.Items[1].TicketsKnown || snapshot.Items[1].MinPrice != nil {
		t.Fatal("failed source invented values")
	}
	if !strings.Contains(strings.Join(snapshot.Cities, ","), "上海") {
		t.Fatal("missing city inferred from actual address")
	}
	if strings.Contains(strings.Join(snapshot.Cities, ","), "黄浦区") {
		t.Fatal("district leaked into the city filter")
	}
	before := calls.Load()
	if _, err = client.Catalog(context.Background(), false); err != nil || calls.Load() != before {
		t.Fatal("cache did not avoid repeated upstream reads")
	}
	if _, err = client.Catalog(context.Background(), true); err != nil || calls.Load() == before {
		t.Fatal("explicit refresh did not read upstream")
	}
	if _, err = client.Reviews(context.Background(), 999, false); err == nil {
		t.Fatal("non-public reviews were exposed")
	}
}

func TestGlobalReviewFeedReadsTopLevelTotalAndUnlistedShows(t *testing.T) {
	calls := 0
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/show-review/hot" {
			t.Errorf("unexpected feed operation: %s %s", r.Method, r.URL)
		}
		if r.URL.Query().Get("current") == "1" {
			fmt.Fprint(w, `{"success":true,"total":"3","data":[{"id":"1","showId":"1","score":5},{"id":"2","showId":"13","score":2,"content":"真实不同意见"}]}`)
		} else {
			fmt.Fprint(w, `{"success":true,"total":"3","data":[{"id":"2","showId":"13","score":2},{"id":"3","showId":"99","score":4}]}`)
		}
	})
	report, err := client.GlobalReviews(context.Background(), false)
	if err != nil || !report.Summary.Complete || report.Summary.Total != 3 || len(report.Items) != 3 || calls != 2 || report.Items[1].ShowID != 13 || len(report.Critical) != 1 {
		t.Fatalf("global feed data lost: %+v %v", report, err)
	}
	_, err = client.GlobalReviews(context.Background(), false)
	if err != nil || calls != 2 {
		t.Fatal("feed cache missed")
	}
}

func TestGlobalReviewFeedDoesNotHideRepeatedPage(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"total":"8","data":[{"id":"1","showId":"13","score":5}]}`)
	})
	report, err := client.GlobalReviews(context.Background(), false)
	if err != nil || report.Summary.Complete || report.Summary.Total != 8 || len(report.Items) != 1 || len(report.Warnings) == 0 {
		t.Fatalf("hid incomplete feed: %+v %v", report, err)
	}
}
