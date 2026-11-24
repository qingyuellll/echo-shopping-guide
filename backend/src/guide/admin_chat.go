package guide

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

// The console chat answers two kinds of admin requests: data questions about
// the catalog itself, and "what would a visitor get" simulations that reuse the
// exact planner and filter pipeline behind the public assistant.

type adminAskRow struct {
	Title string   `json:"title"`
	Sub   string   `json:"sub"`
	Value string   `json:"value"`
	Tags  []string `json:"tags"`
}

type adminAskResponse struct {
	Answer    string            `json:"answer"`
	Kind      string            `json:"kind"`
	Mode      string            `json:"mode"`
	Intent    string            `json:"intent"`
	Plan      *shoppingPlan     `json:"plan,omitempty"`
	Filters   *discoveryFilters `json:"filters,omitempty"`
	Rows      []adminAskRow     `json:"rows"`
	Flags     []string          `json:"flags"`
	LatencyMS int64             `json:"latency_ms"`
	UpdatedAt string            `json:"updated_at"`
}

func (h *Handler) adminAsk(w http.ResponseWriter, r *http.Request) {
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前数据源不支持对话试验。"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var input struct {
		Message  string        `json:"message"`
		Plan     *shoppingPlan `json:"plan"`
		UseModel bool          `json:"use_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "请求内容不是有效 JSON。"})
		return
	}
	message := strings.TrimSpace(input.Message)
	if message == "" || len([]rune(message)) > 300 {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "请用 1–300 字描述要核对的需求或数据问题。"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	started := time.Now()
	snapshot, err := catalog.Catalog(ctx, false)
	if err != nil {
		h.unavailable(w, err)
		return
	}

	if kind := dataIntent(message); kind != "" {
		data, analyticsErr := h.analytics(ctx, catalog, false)
		if analyticsErr != nil {
			h.unavailable(w, analyticsErr)
			return
		}
		answer, rows, flags := answerDataIntent(kind, message, snapshot, data)
		h.write(w, http.StatusOK, adminAskResponse{
			Answer:    answer,
			Kind:      "data",
			Mode:      "data",
			Intent:    kind,
			Rows:      rows,
			Flags:     flags,
			LatencyMS: time.Since(started).Milliseconds(),
			UpdatedAt: snapshot.Coverage.UpdatedAt,
		})
		return
	}

	probe := &Handler{catalog: h.catalog, prompt: h.prompt, stats: newAskStats(), admin: h.admin, started: h.started, analyticsStore: h.analyticsStore}
	if input.UseModel {
		probe.model = h.model
	}
	result := probe.runProbe(ctx, probeInput{Message: message, Plan: input.Plan})
	rows := make([]adminAskRow, 0, len(result.Items))
	flags := []string{}
	for _, entry := range result.Items {
		rows = append(rows, askRow(entry))
	}
	if len(result.Items) == 0 && len(result.Alternatives) > 0 {
		flags = append(flags, fmt.Sprintf("主结果为空，已给出 %d 条放宽条件的备选。", len(result.Alternatives)))
		for _, entry := range result.Alternatives {
			rows = append(rows, askRow(entry))
		}
	}
	if len(result.Items) == 0 && len(result.Alternatives) == 0 {
		flags = append(flags, "没有匹配的演出或票档，助手会说明是哪一项条件限制了结果。")
	}
	if probe.model == nil {
		flags = append(flags, "本次只使用票务数据与评测，没有调用模型。")
	}
	plan := result.Plan
	filters := result.Filters
	h.write(w, http.StatusOK, adminAskResponse{
		Answer:    result.Answer,
		Kind:      "guide",
		Mode:      result.Mode,
		Intent:    result.Intent,
		Plan:      &plan,
		Filters:   &filters,
		Rows:      rows,
		Flags:     flags,
		LatencyMS: time.Since(started).Milliseconds(),
		UpdatedAt: snapshot.Coverage.UpdatedAt,
	})
}

func askRow(entry comparisonEntry) adminAskRow {
	rating := entry.Show.Rating
	value := "暂无评分"
	if rating.Count > 0 {
		value = fmt.Sprintf("★%.2f · %d 个评分", rating.Average, rating.Count)
	}
	if len(entry.Tickets) > 0 {
		low := entry.Tickets[0].Price
		for _, tier := range entry.Tickets {
			if tier.Price < low {
				low = tier.Price
			}
		}
		value += fmt.Sprintf(" · ¥%.0f 起", low)
	}
	tags := []string{}
	if entry.Show.Lifecycle == "ended" {
		tags = append(tags, "历史场次")
	}
	if rating.Count == 0 {
		tags = append(tags, "无评分")
	} else if rating.LowSample {
		tags = append(tags, "样本少")
	}
	if !entry.Show.TicketsKnown || len(entry.Tickets) == 0 {
		tags = append(tags, "缺票档")
	}
	for _, tier := range entry.Tickets {
		if strings.TrimSpace(tier.Rules) == "" {
			tags = append(tags, "缺退改规则")
			break
		}
	}
	place := strings.TrimSpace(entry.Show.Area)
	if address := strings.TrimSpace(entry.Show.Address); address != "" && address != place {
		if place == "" {
			place = address
		} else {
			place += " · " + address
		}
	}
	sub := place
	if len(entry.Show.OpenTime) >= 10 {
		sub = strings.TrimSpace(entry.Show.OpenTime[:10] + " · " + sub)
	}
	return adminAskRow{Title: entry.Show.Name, Sub: sub, Value: value, Tags: tags}
}

func dataIntent(message string) string {
	switch {
	case containsAny(message, "缺票档", "没有票档", "无票档", "票档缺失", "零票档"):
		return "tickets_missing"
	case containsAny(message, "缺规则", "退改规则", "规则缺", "没有规则"):
		return "rules_missing"
	case containsAny(message, "异常低价", "低于10", "低于 10", "1元", "一元", "价格异常", "占位价"):
		return "price_suspicious"
	case containsAny(message, "样本少", "评分少", "没人评", "无评分", "没有评分", "评分样本"):
		return "rating_sample"
	case containsAny(message, "价格带", "价位", "票价分布", "价格分布", "多少钱"):
		return "price_bands"
	case containsAny(message, "城市", "分布", "覆盖"):
		return "cities"
	}
	return ""
}

func containsAny(message string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(message, word) {
			return true
		}
	}
	return false
}

func answerDataIntent(kind, message string, snapshot ticketing.CatalogSnapshot, data *catalogAnalytics) (string, []adminAskRow, []string) {
	rows := []adminAskRow{}
	flags := []string{}
	switch kind {
	case "tickets_missing":
		withTiers := map[string]bool{}
		for _, tier := range data.Tiers {
			withTiers[tier.Show] = true
		}
		missing := []string{}
		for _, show := range snapshot.Items {
			if !withTiers[show.Name] {
				missing = append(missing, show.Name)
				rows = append(rows, adminAskRow{Title: show.Name, Sub: show.Address, Value: "0 个票档", Tags: []string{"无法购票", lifecycleTag(show)}})
			}
		}
		answer := fmt.Sprintf("共 %d 场演出没有任何票档记录（%d / %d 场）。这些场次不会进入购票推荐，助手只会把它们当历史回顾。", len(missing), len(missing), len(snapshot.Items))
		if len(rows) > 8 {
			rows = rows[:8]
			flags = append(flags, fmt.Sprintf("只列出前 8 场，其余 %d 场见“城市数据”卡片的待补列。", len(missing)-8))
		}
		return answer, rows, flags
	case "rules_missing":
		type bucket struct {
			show  string
			count int
			total int
		}
		byShow := map[string]*bucket{}
		for _, tier := range data.Tiers {
			entry, ok := byShow[tier.Show]
			if !ok {
				entry = &bucket{show: tier.Show}
				byShow[tier.Show] = entry
			}
			entry.total++
			if tier.RulesMissing {
				entry.count++
			}
		}
		list := make([]*bucket, 0, len(byShow))
		for _, entry := range byShow {
			if entry.count > 0 {
				list = append(list, entry)
			}
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].count != list[j].count {
				return list[i].count > list[j].count
			}
			return list[i].show < list[j].show
		})
		for _, entry := range list {
			rows = append(rows, adminAskRow{Title: entry.show, Sub: "票档规则", Value: fmt.Sprintf("%d / %d 档缺规则", entry.count, entry.total), Tags: []string{"需补规则"}})
		}
		answer := fmt.Sprintf("%d / %d 个票档没有填写退改规则，涉及 %d 场演出。详情页会提示“此票档尚未提供具体规则”，助手也不会编造退改条件。", data.Summary.MissingRules, data.Summary.Tiers, len(list))
		if len(rows) > 8 {
			rows = rows[:8]
		}
		return answer, rows, flags
	case "price_suspicious":
		for _, tier := range data.Tiers {
			if tier.Price > 0 && tier.Price < 10 {
				rows = append(rows, adminAskRow{Title: tier.Show, Sub: tier.Title + " · " + tier.City, Value: fmt.Sprintf("¥%.2f", tier.Price), Tags: []string{"疑似测试价", lifecycleTagName(tier.Lifecycle)}})
			}
		}
		answer := fmt.Sprintf("有 %d 个票档价格低于 ¥10，最低 ¥%.2f。这类价格可能是测试或占位数据，建议核对后再对外推荐。", data.Summary.SuspiciousPrice, data.Summary.MinPrice)
		if len(rows) > 8 {
			rows = rows[:8]
		}
		return answer, rows, flags
	case "rating_sample":
		low, none := 0, 0
		for _, show := range snapshot.Items {
			if show.Rating.Count == 0 {
				none++
				rows = append(rows, adminAskRow{Title: show.Name, Sub: show.Address, Value: "无有效评分", Tags: []string{"无评分", lifecycleTag(show)}})
			} else if show.Rating.Count < 5 {
				low++
				rows = append(rows, adminAskRow{Title: show.Name, Sub: show.Address, Value: fmt.Sprintf("%.2f 分 · %d 个评分", show.Rating.Average, show.Rating.Count), Tags: []string{"样本少", lifecycleTag(show)}})
			}
		}
		answer := fmt.Sprintf("%d 场演出有效评分少于 5 个，另有 %d 场完全没有评分。口碑排序会把它们排在样本充足的演出之后，助手回答时也会提示样本少。", low, none)
		if len(rows) > 8 {
			rows = rows[:8]
		}
		return answer, rows, flags
	case "price_bands":
		lowest := data.Summary.MinPrice
		highest := data.Summary.MaxPrice
		for _, tier := range data.Tiers {
			rows = append(rows, adminAskRow{Title: tier.Title, Sub: tier.Show + " · " + tier.City, Value: fmt.Sprintf("¥%.2f", tier.Price), Tags: []string{lifecycleTagName(tier.Lifecycle)}})
		}
		answer := fmt.Sprintf("票价区间 ¥%.2f – ¥%.2f，均价 ¥%.2f。价格带占比：%s。", lowest, highest, data.Summary.AveragePrice, bandText(data.Bands))
		if len(rows) > 8 {
			rows = rows[:8]
		}
		return answer, rows, flags
	default: // cities
		city := ""
		for _, row := range data.Cities {
			if row.City != "未标注城市" && strings.Contains(message, row.City) && len([]rune(row.City)) > len([]rune(city)) {
				city = row.City
			}
		}
		for _, row := range data.Cities {
			if city != "" && row.City != city {
				continue
			}
			value := fmt.Sprintf("%d 场 · 可购 %d · 票档 %d", row.Shows, row.Active, row.Tiers)
			tags := []string{}
			if row.MissingTickets > 0 {
				tags = append(tags, fmt.Sprintf("缺票档 %d", row.MissingTickets))
			}
			if row.MissingReviews > 0 {
				tags = append(tags, fmt.Sprintf("评测不全 %d", row.MissingReviews))
			}
			if row.RatingCount > 0 {
				value += fmt.Sprintf(" · %.2f 分（%d 个评分）", row.Average, row.RatingCount)
			} else {
				tags = append(tags, "无评分")
			}
			rows = append(rows, adminAskRow{Title: row.City, Sub: priceRangeText(row.MinPrice, row.MaxPrice), Value: value, Tags: tags})
		}
		answer := fmt.Sprintf("当前公开演出分布在 %d 个城市，共 %d 个票档。表里按演出数量排序，缺票档与评测不全的城市会单独标注。", len(data.Cities), data.Summary.Tiers)
		if city != "" {
			answer = fmt.Sprintf("%s 共 %d 场演出，其中 %d 场可购；票档与口碑明细见下表。", city, cityShows(data, city), cityActive(data, city))
		}
		if len(rows) > 8 {
			flags = append(flags, fmt.Sprintf("只列出前 8 个城市，其余 %d 个见“城市数据”卡片。", len(rows)-8))
			rows = rows[:8]
		}
		return answer, rows, flags
	}
}

func bandText(bands []priceBand) string {
	parts := make([]string, 0, len(bands))
	for _, band := range bands {
		parts = append(parts, fmt.Sprintf("%s %d 档（%.1f%%）", band.Label, band.Count, band.Share))
	}
	return strings.Join(parts, "、")
}

func priceRangeText(min, max *float64) string {
	if min == nil && max == nil {
		return "票价待公布"
	}
	if min == nil {
		return fmt.Sprintf("最高 ¥%.0f", *max)
	}
	if max == nil || *max == *min {
		return fmt.Sprintf("¥%.0f", *min)
	}
	return fmt.Sprintf("¥%.0f – ¥%.0f", *min, *max)
}

func lifecycleTag(show ticketing.Show) string {
	if show.Lifecycle == "ended" {
		return "历史场次"
	}
	return "未结束"
}

func lifecycleTagName(lifecycle string) string {
	if lifecycle == "ended" {
		return "历史票档"
	}
	return "可售场次"
}

func cityShows(data *catalogAnalytics, city string) int {
	for _, row := range data.Cities {
		if row.City == city {
			return row.Shows
		}
	}
	return 0
}

func cityActive(data *catalogAnalytics, city string) int {
	for _, row := range data.Cities {
		if row.City == city {
			return row.Active
		}
	}
	return 0
}
