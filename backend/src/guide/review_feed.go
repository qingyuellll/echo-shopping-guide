package guide

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

type globalReviews interface {
	GlobalReviews(context.Context, bool) (ticketing.ReviewReport, error)
}

type reviewFeedEntry struct {
	ticketing.Review
	ShowName      string `json:"show_name"`
	ShowAvailable bool   `json:"show_available"`
	Lifecycle     string `json:"lifecycle"`
}

func (h *Handler) reviewFeed(w http.ResponseWriter, r *http.Request) {
	source, ok := h.catalog.(globalReviews)
	catalog, rich := h.catalog.(richCatalog)
	if !ok || !rich {
		h.write(w, 503, map[string]string{"message": "全站评测暂不可用"})
		return
	}
	q := r.URL.Query()
	page, e1 := strconv.Atoi(defaultString(q.Get("page"), "1"))
	size, e2 := strconv.Atoi(defaultString(q.Get("page_size"), "10"))
	filter := q.Get("filter")
	search := strings.TrimSpace(q.Get("query"))
	if e1 != nil || e2 != nil || page < 1 || page > 100000 || size < 1 || size > 100 || len([]rune(search)) > 100 || filter != "" && filter != "all" && filter != "text" && filter != "positive" && filter != "critical" {
		h.write(w, 400, map[string]string{"message": "评测筛选或分页无效"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	report, err := source.GlobalReviews(ctx, q.Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	shows := map[int64]ticketing.Show{}
	for _, show := range snapshot.Items {
		shows[show.ID] = show
	}
	evidence := map[int64]ticketing.Review{}
	for _, report := range snapshot.Reviews {
		for _, review := range report.Items {
			evidence[review.ID] = review
		}
	}
	items := make([]reviewFeedEntry, 0)
	unlisted := 0
	for _, review := range report.Items {
		if item, ok := evidence[review.ID]; ok {
			review.Purchased = item.Purchased
			review.Verified = item.Verified
			review.EvidenceKnown = item.EvidenceKnown
			review.SeatArea = item.SeatArea
			review.Dimensions = item.Dimensions
		}
		show, available := shows[review.ShowID]
		if !available {
			unlisted++
		}
		name := show.Name
		if !available {
			name = "演出 #" + strconv.FormatInt(review.ShowID, 10) + "（当前未公开）"
		}
		if filter == "text" && review.Content == "" && review.Title == "" || filter == "positive" && (review.Score < 4 || review.Score > 5) || filter == "critical" && (review.Score < 1 || review.Score > 3) {
			continue
		}
		haystack := strings.ToLower(name + " " + review.Title + " " + review.Content + " " + review.Author)
		match := true
		for _, word := range strings.Fields(strings.ToLower(search)) {
			if !strings.Contains(haystack, word) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		items = append(items, reviewFeedEntry{Review: review, ShowName: name, ShowAvailable: available, Lifecycle: show.Lifecycle})
	}
	sort.SliceStable(items, func(i, j int) bool {
		switch q.Get("sort") {
		case "helpful":
			return items[i].Likes > items[j].Likes
		case "low":
			return (items[i].Score == 0) == (items[j].Score == 0) && items[i].Score < items[j].Score || items[j].Score == 0 && items[i].Score > 0
		default:
			if items[i].CreatedAt == items[j].CreatedAt {
				return items[i].ID > items[j].ID
			}
			return items[i].CreatedAt > items[j].CreatedAt
		}
	})
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	h.write(w, 200, map[string]any{"items": items[start:end], "summary": report.Summary, "total": len(items), "page": page, "has_more": end < len(items), "unlisted_count": unlisted, "warnings": report.Warnings})
}
