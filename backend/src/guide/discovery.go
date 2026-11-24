package guide

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

type richCatalog interface {
	Catalog(context.Context, bool) (ticketing.CatalogSnapshot, error)
	Reviews(context.Context, int64, bool) (ticketing.ReviewReport, error)
}

type discoveryFilters struct {
	Query         string  `json:"query"`
	Category      string  `json:"category"`
	City          string  `json:"city"`
	Lifecycle     string  `json:"lifecycle"`
	Sort          string  `json:"sort"`
	Budget        float64 `json:"budget"`
	MinScore      float64 `json:"min_score"`
	AvailableOnly bool    `json:"available_only"`
	DateFrom      string  `json:"date_from"`
	DateTo        string  `json:"date_to"`
}

func validFilters(f discoveryFilters) bool {
	if len([]rune(f.Query)) > 100 || f.Budget < 0 || math.IsNaN(f.Budget) || math.IsInf(f.Budget, 0) || f.MinScore < 0 || f.MinScore > 5 || math.IsNaN(f.MinScore) || (f.Lifecycle != "" && f.Lifecycle != "all" && f.Lifecycle != "active" && f.Lifecycle != "ended") {
		return false
	}
	for _, date := range []string{f.DateFrom, f.DateTo} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return false
			}
		}
	}
	return f.DateFrom == "" || f.DateTo == "" || f.DateFrom <= f.DateTo
}

func filterShows(items []ticketing.Show, f discoveryFilters) []ticketing.Show {
	result := make([]ticketing.Show, 0)
	for _, show := range items {
		haystack := strings.ToLower(strings.Join([]string{show.Name, show.TypeName, show.Area, show.Address, show.Description}, " "))
		matched := true
		for _, word := range strings.Fields(strings.ToLower(f.Query)) {
			if !strings.Contains(haystack, word) {
				matched = false
				break
			}
		}
		if !matched || (f.Category != "" && show.TypeName != f.Category) || (f.City != "" && !strings.Contains(show.Area+show.Address, f.City)) {
			continue
		}
		ended := show.Lifecycle == "ended"
		if f.Lifecycle == "active" && ended || f.Lifecycle == "ended" && !ended {
			continue
		}
		if f.MinScore > 0 && (!show.Rating.Available || !show.Rating.Complete || show.Rating.Count == 0 || show.Rating.Average < f.MinScore) {
			continue
		}
		if f.Budget > 0 && (show.MinPrice == nil || *show.MinPrice > f.Budget) {
			continue
		}
		if f.AvailableOnly && (ended || !show.TicketsKnown || show.AvailableStock <= 0) {
			continue
		}
		date := ""
		if len(show.OpenTime) >= 10 {
			date = show.OpenTime[:10]
		}
		if f.DateFrom != "" && (date == "" || date < f.DateFrom) || f.DateTo != "" && (date == "" || date > f.DateTo) {
			continue
		}
		result = append(result, show)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if f.Sort == "rating" || f.Sort == "reputation" || f.Sort == "discussed" {
			aReliable := a.Rating.Available && a.Rating.Complete && a.Rating.Count > 0
			bReliable := b.Rating.Available && b.Rating.Complete && b.Rating.Count > 0
			if aReliable != bReliable {
				return aReliable
			}
		}
		switch f.Sort {
		case "rating":
			if a.Rating.Average != b.Rating.Average {
				return a.Rating.Average > b.Rating.Average
			}
			return a.Rating.Count > b.Rating.Count
		case "reputation":
			// A single five-star review must not outrank a show with a usable sample.
			if sampleTier(a.Rating) != sampleTier(b.Rating) {
				return sampleTier(a.Rating) < sampleTier(b.Rating)
			}
			if a.Rating.AdjustedScore != b.Rating.AdjustedScore {
				return a.Rating.AdjustedScore > b.Rating.AdjustedScore
			}
			return a.Rating.Count > b.Rating.Count
		case "discussed":
			return a.Rating.Count > b.Rating.Count
		case "price":
			if a.MinPrice == nil {
				return false
			}
			if b.MinPrice == nil {
				return true
			}
			return *a.MinPrice < *b.MinPrice
		case "soon":
			if a.Lifecycle == "ended" && b.Lifecycle != "ended" {
				return false
			}
			if b.Lifecycle == "ended" && a.Lifecycle != "ended" {
				return true
			}
			return a.OpenTime < b.OpenTime
		default:
			if (a.Lifecycle == "ended") != (b.Lifecycle == "ended") {
				return a.Lifecycle != "ended"
			}
			return a.Sold > b.Sold
		}
	})
	return result
}

func (h *Handler) discovery(w http.ResponseWriter, r *http.Request, catalog richCatalog) {
	query := r.URL.Query()
	budget, err1 := strconv.ParseFloat(defaultString(query.Get("budget"), "0"), 64)
	minimum, err2 := strconv.ParseFloat(defaultString(query.Get("min_score"), "0"), 64)
	page, err3 := strconv.Atoi(defaultString(query.Get("page"), "1"))
	size, err4 := strconv.Atoi(defaultString(query.Get("page_size"), "12"))
	f := discoveryFilters{Query: strings.TrimSpace(query.Get("query")), Category: query.Get("category"), City: query.Get("city"), Lifecycle: query.Get("lifecycle"), Sort: query.Get("sort"), Budget: budget, MinScore: minimum, AvailableOnly: query.Get("available_only") == "true", DateFrom: query.Get("date_from"), DateTo: query.Get("date_to")}
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || !validFilters(f) || page < 1 || page > 100000 || size < 1 || size > 100 {
		h.write(w, 400, map[string]string{"message": "筛选条件或分页无效"})
		return
	}
	for _, date := range []string{f.DateFrom, f.DateTo} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				h.write(w, 400, map[string]string{"message": "日期格式应为 YYYY-MM-DD"})
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := catalog.Catalog(ctx, query.Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	items := filterShows(snapshot.Items, f)
	if raw := query.Get("ids"); raw != "" {
		selected := map[int64]bool{}
		values := strings.Split(raw, ",")
		if len(values) > 200 {
			h.write(w, 400, map[string]string{"message": "收藏演出过多"})
			return
		}
		for _, value := range values {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 0 {
				h.write(w, 400, map[string]string{"message": "演出编号无效"})
				return
			}
			selected[id] = true
		}
		filtered := make([]ticketing.Show, 0)
		for _, show := range items {
			if selected[show.ID] {
				filtered = append(filtered, show)
			}
		}
		items = filtered
	}
	total := len(items)
	start := min((page-1)*size, total)
	end := min(start+size, total)
	h.write(w, 200, map[string]any{"items": items[start:end], "total": total, "page": page, "page_size": size, "has_more": end < total, "categories": snapshot.Categories, "cities": snapshot.Cities, "coverage": snapshot.Coverage})
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// sampleTier orders confidence before the average, so one enthusiastic review
// never leads a reputation ranking over shows with a usable sample.
func sampleTier(r ticketing.RatingSummary) int {
	switch {
	case !r.Available || !r.Complete || r.Count <= 0:
		return 4
	case r.Count >= 5:
		return 0
	case r.Count >= 3:
		return 1
	case r.Count == 2:
		return 2
	default:
		return 3
	}
}

// displayCity keeps the filter value stable while showing the standard name.
func displayCity(city string) string {
	switch city {
	case "香港":
		return "中国香港"
	case "澳门":
		return "中国澳门"
	case "台湾":
		return "中国台湾"
	}
	return city
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		h.write(w, 400, map[string]string{"message": "演出编号无效"})
		return
	}
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.tickets(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := catalog.Catalog(ctx, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	for _, show := range snapshot.Items {
		if show.ID == id {
			tiers, tierErr := h.catalog.ListTickets(ctx, id)
			warnings := append([]string{}, snapshot.Reviews[id].Warnings...)
			if tierErr != nil {
				tiers = []ticketing.TicketTier{}
				warnings = append(warnings, "票档暂时不可用，请前往 Echo 确认")
			}
			purchasable := false
			for _, tier := range tiers {
				if show.Lifecycle != "ended" && tier.OnSale && !tier.SoldOut {
					purchasable = true
				}
			}
			h.write(w, 200, map[string]any{"show": show, "items": tiers, "reviews": snapshot.Reviews[id], "warnings": warnings, "purchasable": purchasable, "updated_at": snapshot.Coverage.UpdatedAt, "tickets_updated_at": time.Now().UTC().Format(time.RFC3339)})
			return
		}
	}
	h.write(w, 404, map[string]string{"message": "未找到这场公开演出"})
}

func (h *Handler) reviews(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		h.write(w, 400, map[string]string{"message": "演出编号无效"})
		return
	}
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, 503, map[string]string{"message": "评测查询暂不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	report, err := catalog.Reviews(ctx, id, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	h.write(w, 200, report)
}

func (h *Handler) rankings(w http.ResponseWriter, r *http.Request) {
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, 503, map[string]string{"message": "口碑排行暂不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	items := filterShows(snapshot.Items, discoveryFilters{Lifecycle: r.URL.Query().Get("lifecycle"), Sort: "reputation"})
	rated := make([]ticketing.Show, 0)
	for _, show := range items {
		if show.Rating.Complete && show.Rating.Count > 0 {
			rated = append(rated, show)
		}
	}
	if len(rated) > 10 {
		rated = rated[:10]
	}
	h.write(w, 200, map[string]any{"items": rated, "coverage": snapshot.Coverage, "method": "先按有效评分数量分层（5 个及以上、3–4 个、2 个、1 个、无评分），同一层内按 (评分总和 + 5 × 3.5) ÷ (评价数 + 5) 排序。少量评价会向 3.5 分收敛；排序值不作为观众评分展示。"})
}

type comparisonEntry struct {
	UpdatedAt string                 `json:"updated_at"`
	Show      ticketing.Show         `json:"show"`
	Tickets   []ticketing.TicketTier `json:"tickets"`
	Reviews   ticketing.ReviewReport `json:"reviews"`
	Reasons   []string               `json:"reasons"`
	Cautions  []string               `json:"cautions"`
	// Keywords repeat across this show's reviews (similar runs merged);
	// Related cites reviews whose own words match the question.
	Keywords []string      `json:"keywords,omitempty"`
	Related  []reviewQuote `json:"related,omitempty"`
}

func (h *Handler) compare(w http.ResponseWriter, r *http.Request) {
	values := strings.Split(r.URL.Query().Get("ids"), ",")
	if len(values) < 2 || len(values) > 4 {
		h.write(w, 400, map[string]string{"message": "请选择 2–4 场演出进行比较"})
		return
	}
	ids := make([]int64, 0, len(values))
	seen := map[int64]bool{}
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 || seen[id] {
			h.write(w, 400, map[string]string{"message": "演出编号无效或重复"})
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, 503, map[string]string{"message": "演出比较暂不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	entries := make([]comparisonEntry, 0, len(ids))
	for _, id := range ids {
		found := false
		for _, show := range snapshot.Items {
			if show.ID == id {
				entries = append(entries, h.comparison(ctx, show, snapshot.Reviews[id], 0))
				found = true
				break
			}
		}
		if !found {
			h.write(w, 404, map[string]string{"message": "所选演出已不可公开查看，请刷新列表"})
			return
		}
	}
	h.write(w, 200, map[string]any{"items": entries, "updated_at": snapshot.Coverage.UpdatedAt})
}

func (h *Handler) comparison(ctx context.Context, show ticketing.Show, reviews ticketing.ReviewReport, budget float64) comparisonEntry {
	result := comparisonEntry{Show: show, Reviews: reviews, Reasons: []string{}, Cautions: []string{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	tiers, err := h.catalog.ListTickets(ctx, show.ID)
	if tiers == nil {
		tiers = []ticketing.TicketTier{}
	}
	result.Tickets = tiers
	if err != nil {
		result.Cautions = append(result.Cautions, "票档暂未完整读取")
	}
	if show.Lifecycle == "ended" {
		result.Cautions = append(result.Cautions, "历史演出，已结束；票价与口碑仅供回顾")
	}
	if reviews.Summary.Count > 0 {
		result.Reasons = append(result.Reasons, fmt.Sprintf("观众评测均分 %.2f/5，%d 个有效评分", reviews.Summary.Average, reviews.Summary.Count))
	}
	if reviews.Summary.Count == 0 {
		result.Cautions = append(result.Cautions, "暂无有效观众评分，不能据此判断口碑")
	} else if reviews.Summary.LowSample {
		result.Cautions = append(result.Cautions, "有效评分少于 5 个，口碑样本较少")
	}
	if !reviews.Summary.Complete {
		result.Cautions = append(result.Cautions, "评测尚未完整读取，评分仅供参考")
	}
	result.Cautions = append(result.Cautions, "价格与库存为查询时快照，下单前 Echo 会重新核对")
	if reviews.Summary.Count > 0 && reviews.Summary.Complete {
		result.Reasons = append(result.Reasons, fmt.Sprintf("按样本数修正的口碑参考分 %.2f/5", reviews.Summary.AdjustedScore))
	}
	if budget > 0 {
		for _, tier := range tiers {
			if tier.Price <= budget && !tier.SoldOut && tier.OnSale {
				result.Reasons = append(result.Reasons, fmt.Sprintf("预算内票档：%s ¥%.2f", tier.Title, tier.Price))
				break
			}
		}
	}
	if len(reviews.Critical) > 0 {
		result.Cautions = append(result.Cautions, "有 3 星及以下文字评测，建议阅读原文再决定")
	}
	return result
}

var prefixBudget = regexp.MustCompile(`预算\s*(\d+(?:\.\d+)?)`)
var minimumScore = regexp.MustCompile(`(\d(?:\.\d+)?)\s*分\s*(?:以上|起)`)

// assistComputation holds everything computed from real data before any
// model call, so both the plain and the streaming endpoint share one path.
type assistComputation struct {
	Entries       []comparisonEntry
	Alternatives  []comparisonEntry
	Plan          shoppingPlan
	Filters       discoveryFilters
	Intent        string
	Snapshot      ticketing.CatalogSnapshot
	CatalogAnswer string
}

// computeAssist resolves the plan, filters shows, builds entries and the
// data-mode answer. A non-zero code means the caller should return an error.
func (h *Handler) computeAssist(ctx context.Context, input assistRequest, catalog richCatalog) (*assistComputation, int, string) {
	if !validFilters(input.Filters) || len(input.CompareIDs) > 4 {
		return nil, 400, "咨询筛选条件无效"
	}
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		// 上游地址、驱动报错只留在服务端日志里；页面只看到可执行的提示。
		slog.Warn("echo catalog unavailable", "error", err)
		return nil, 503, "暂时无法获取演出信息，请确认 Echo 票务服务已启动后重试。"
	}
	plan := resolvePlan(input, snapshot, time.Now())
	f := plan.Filters
	if plan.PartySize < 1 || plan.PartySize > 10 || plan.Budget < 0 || plan.Budget > 10000000 || (plan.BudgetMode != "total" && plan.BudgetMode != "per_person") || (plan.SeatPreference != "" && plan.SeatPreference != "front" && plan.SeatPreference != "adjacent") || !validFilters(f) {
		return nil, 400, "请使用有效日期、预算和 1–10 人的购票人数"
	}
	intent := "recommend"
	for _, word := range []string{"评测", "评分", "口碑", "评价", "避雷", "历史", "回顾"} {
		if strings.Contains(input.Message, word) {
			intent = "reviews"
			break
		}
	}
	if len(input.CompareIDs) > 1 || strings.Contains(input.Message, "比较") || strings.Contains(input.Message, "对比") {
		intent = "compare"
	}
	if f.Lifecycle == "" || f.Lifecycle == "all" {
		if intent == "recommend" {
			f.Lifecycle = "active"
		} else {
			f.Lifecycle = "all"
		}
	}
	if intent != "compare" {
		for _, word := range []string{"能买票", "可购", "购票", "买票", "在售", "未结束"} {
			if strings.Contains(input.Message, word) {
				f.Lifecycle = "active"
				f.AvailableOnly = true
				break
			}
		}
	}
	if intent == "reviews" && (f.Sort == "popular" || f.Sort == "") {
		f.Sort = "reputation"
	}
	selectedIDs := map[int64]bool{}
	for _, value := range append(input.CompareIDs, input.ShowID) {
		if value == "" {
			continue
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, 400, "演出编号无效"
		}
		selectedIDs[id] = true
	}
	if len(selectedIDs) == 0 {
		for _, show := range snapshot.Items {
			if len([]rune(show.Name)) >= 2 && strings.Contains(input.Message, show.Name) {
				selectedIDs[show.ID] = true
			}
		}
	}
	shows := filterShows(snapshot.Items, f)
	if len(selectedIDs) > 0 {
		shows = []ticketing.Show{}
		for _, show := range snapshot.Items {
			if selectedIDs[show.ID] {
				shows = append(shows, show)
			}
		}
	}
	entries := make([]comparisonEntry, 0, 4)
	for _, show := range shows {
		if len(entries) >= 4 {
			break
		}
		entry := h.comparison(ctx, show, snapshot.Reviews[show.ID], f.Budget)
		if intent == "recommend" || f.AvailableOnly {
			entry.Tickets = plannedTickets(entry.Tickets, plan)
			if show.Lifecycle == "ended" {
				entry.Tickets = []ticketing.TicketTier{}
			}
			if len(entry.Tickets) > 0 {
				entry.Reasons = append(entry.Reasons, fmt.Sprintf("%d 人合计 ¥%.2f 起", plan.PartySize, entry.Tickets[0].Price*float64(plan.PartySize)))
			}
			if plan.SeatPreference != "" {
				entry.Cautions = append(entry.Cautions, "已优先筛选可选座票档；前排和连座须在座位图确认，尚未锁座")
			}
		}
		if f.City != "" && strings.Contains(show.Area+show.Address, f.City) {
			entry.Reasons = append(entry.Reasons, "城市符合："+displayCity(f.City))
		}
		if f.DateFrom != "" && len(show.OpenTime) >= 10 && show.OpenTime[:10] >= f.DateFrom && show.OpenTime[:10] <= f.DateTo {
			entry.Reasons = append(entry.Reasons, "日期符合："+f.DateFrom+" 至 "+f.DateTo)
		}
		if (intent == "recommend" || f.AvailableOnly) && len(selectedIDs) == 0 {
			found := false
			for _, tier := range entry.Tickets {
				if tier.OnSale && !tier.SoldOut && (f.Budget == 0 || tier.Price <= f.Budget) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if intent == "recommend" || f.AvailableOnly {
			filtered := make([]ticketing.TicketTier, 0)
			for _, tier := range entry.Tickets {
				if tier.OnSale && !tier.SoldOut && (f.Budget == 0 || tier.Price <= f.Budget) {
					filtered = append(filtered, tier)
				}
			}
			entry.Tickets = filtered
		}
		entries = append(entries, entry)
	}
	plan.Filters = f
	alternatives := []comparisonEntry{}
	if len(entries) == 0 && intent == "recommend" && len(selectedIDs) == 0 {
		alternatives = h.alternatives(ctx, snapshot, plan, f)
	}
	// Keywords merge runs that repeat across reviews; Related cites reviews
	// that literally mention what the visitor asked about.
	keywords := extractKeywords(entries, 5)
	related := relatedQuotes(entries, input.Message, 2)
	for i := range entries {
		entries[i].Keywords = keywords[entries[i].Show.ID]
		entries[i].Related = related[entries[i].Show.ID]
	}
	answer := "已记住：" + planDescription(plan) + "。\n" + catalogAnswer(entries, intent)
	if len(entries) == 0 {
		answer = "已记住：" + planDescription(plan) + "。\n" + noMatchAnswer(f)
		if len(alternatives) > 0 {
			answer += "\n" + catalogAnswer(alternatives, "alternative")
		}
	}
	return &assistComputation{Entries: entries, Alternatives: alternatives, Plan: plan, Filters: f, Intent: intent, Snapshot: snapshot, CatalogAnswer: answer}, 0, ""
}

func (h *Handler) assistRich(w http.ResponseWriter, r *http.Request, input assistRequest, catalog richCatalog) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	comp, code, message := h.computeAssist(ctx, input, catalog)
	if code != 0 {
		h.write(w, code, map[string]string{"message": message})
		return
	}
	entries, alternatives, plan, f, intent := comp.Entries, comp.Alternatives, comp.Plan, comp.Filters, comp.Intent
	answer := comp.CatalogAnswer
	mode := "catalog"
	if h.model != nil && h.model.Enabled() {
		messages := h.modelMessages(input, intent, plan, f, entries, alternatives)
		modelCtx, modelCancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := h.model.Complete(modelCtx, h.model.LargeModel(), messages, 0.2)
		modelCancel()
		if err == nil {
			answer = result
			mode = "ai"
		}
	}
	h.stats.record(askRecord{At: time.Now().UTC().Format(time.RFC3339), Question: input.Message, Mode: mode, Intent: intent, LatencyMS: time.Since(started).Milliseconds(), Items: len(entries), Alternatives: len(alternatives)})
	if h.model != nil && h.model.Enabled() && mode != "ai" {
		h.stats.markFallback()
	}
	h.write(w, 200, map[string]any{"answer": answer, "mode": mode, "intent": intent, "items": entries, "alternatives": alternatives, "filters": f, "plan": plan, "coverage": comp.Snapshot.Coverage, "updated_at": comp.Snapshot.Coverage.UpdatedAt})
}

// alternatives keeps a dead end usable: when date or budget removes every show,
// offer the closest purchasable options and say exactly which condition differs.
func (h *Handler) alternatives(ctx context.Context, snapshot ticketing.CatalogSnapshot, plan shoppingPlan, f discoveryFilters) []comparisonEntry {
	type attempt struct {
		reason  string
		filters discoveryFilters
	}
	attempts := make([]attempt, 0, 2)
	relaxed := f
	if relaxed.DateFrom != "" || relaxed.DateTo != "" {
		relaxed.DateFrom, relaxed.DateTo = "", ""
		attempts = append(attempts, attempt{"不在你要求的日期内", relaxed})
	}
	if relaxed.Budget > 0 {
		free := relaxed
		free.Budget = 0
		attempts = append(attempts, attempt{"超出你设定的预算", free})
	}
	seen := map[int64]bool{}
	out := []comparisonEntry{}
	for _, item := range attempts {
		candidate := plan
		candidate.Filters = item.filters
		if item.filters.Budget == 0 {
			candidate.Budget = 0
		}
		for _, show := range filterShows(snapshot.Items, item.filters) {
			if len(out) >= 2 {
				return out
			}
			if show.Lifecycle == "ended" || seen[show.ID] {
				continue
			}
			entry := h.comparison(ctx, show, snapshot.Reviews[show.ID], item.filters.Budget)
			entry.Tickets = plannedTickets(entry.Tickets, candidate)
			if len(entry.Tickets) == 0 {
				continue
			}
			entry.Reasons = append([]string{item.reason}, entry.Reasons...)
			if len(show.OpenTime) >= 10 {
				entry.Reasons = append(entry.Reasons, "可购日期："+show.OpenTime[:10])
			}
			seen[show.ID] = true
			out = append(out, entry)
		}
	}
	return out
}

func noMatchAnswer(f discoveryFilters) string {
	parts := []string{}
	if f.DateFrom != "" {
		date := f.DateFrom
		if f.DateTo != "" && f.DateTo != f.DateFrom {
			date += " 至 " + f.DateTo
		}
		parts = append(parts, "日期 "+date)
	}
	if f.City != "" {
		parts = append(parts, "城市 "+displayCity(f.City))
	}
	if f.Category != "" {
		parts = append(parts, "类型 "+f.Category)
	}
	if f.Budget > 0 {
		parts = append(parts, fmt.Sprintf("单张预算 ¥%.2f", f.Budget))
	}
	if f.AvailableOnly {
		parts = append(parts, "只看有余票")
	}
	scope := "当前条件"
	if len(parts) > 0 {
		scope = strings.Join(parts, "、")
	}
	return "按" + scope + "没有找到可购演出。可以放宽其中一个条件再试，例如换个日期、取消类型限制或提高预算。"
}

func catalogAnswer(entries []comparisonEntry, intent string) string {
	if len(entries) == 0 {
		return "当前没有找到符合条件的演出。可切换‘全部演出’查看历史口碑，或调整城市、评分和预算。"
	}
	var answer strings.Builder
	if intent == "compare" {
		answer.WriteString("根据票档、评分与原文评测，对比结果如下：\n")
	} else if intent == "reviews" {
		answer.WriteString("根据 Echo 的公开观众评测，为你整理口碑依据：\n")
	} else if intent == "alternative" {
		answer.WriteString("下面这些与你的条件略有不同，可以作为备选：\n")
	} else {
		answer.WriteString("根据预算、票档和观众评分，为你找到以下选择：\n")
	}
	for _, entry := range entries {
		fmt.Fprintf(&answer, "\n《%s》 · %s\n", entry.Show.Name, entry.Show.TypeName)
		summary := entry.Reviews.Summary
		if summary.Count > 0 {
			fmt.Fprintf(&answer, "评测均分 %.2f/5 · %d 个有效评分 · 4–5 星占 %.0f%%\n", summary.Average, summary.Count, summary.PositiveRate*100)
		} else {
			answer.WriteString("暂无有效观众评分，不能据此判断口碑。\n")
		}
		if entry.Show.Score > 0 {
			fmt.Fprintf(&answer, "Echo 演出展示分：%.1f/5（与评测均分口径不同）\n", entry.Show.Score)
		}
		for i, tier := range entry.Tickets {
			if i >= 3 {
				break
			}
			seat := "无需选座"
			if tier.HasSeats {
				seat = "需要选座"
			}
			fmt.Fprintf(&answer, "%s ¥%.2f · %s\n", tier.Title, tier.Price, seat)
		}
		for _, quote := range append(append([]ticketing.Review{}, entry.Reviews.Positive...), entry.Reviews.Critical...) {
			content := []rune(quote.Content)
			if len(content) > 180 {
				content = append(content[:180], '…')
			}
			fmt.Fprintf(&answer, "原文依据 #%d，%s（%.0f 星）：「%s」\n", quote.ID, quote.Author, quote.Score, string(content))
		}
		if len(entry.Keywords) > 0 {
			fmt.Fprintf(&answer, "观众反复提到：%s\n", strings.Join(entry.Keywords, "、"))
		}
		for _, quote := range entry.Related {
			fmt.Fprintf(&answer, "与提问相关的原文 #%s，%s（%.0f 星）：「%s」\n", quote.ReviewID, quote.Author, quote.Score, quote.Excerpt)
		}
		for _, caution := range entry.Cautions {
			fmt.Fprintf(&answer, "注意：%s。\n", caution)
		}
	}
	answer.WriteString("\n可打开演出详情阅读全部评测与评分分布。需要买票时，点击票档下的‘确认订单’，在 Echo 核对数量、座位和规则后提交，付款需要单独确认。写评测可使用‘写评分与评测’，根据自己的感受整理草稿并审核发布。")
	return answer.String()
}
