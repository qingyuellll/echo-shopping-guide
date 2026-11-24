package guide

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/agent"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

// guideToolkit 是 agent.Toolkit 在导购业务里的实现。
//
// 它把 Agent 的语义化动作（检索、详情、对比、评测）翻译成既有的业务函数：
// filterShows 负责筛选与排序，comparison 负责票档、理由与注意事项。
// 这样 Agent 不需要知道 catalog 结构，业务规则也只有一份。
type guideToolkit struct {
	h *Handler
}

// snapshot 复用 Catalog 的 30 秒缓存，Agent 与页面看到的是同一份数据。
func (t guideToolkit) snapshot(ctx context.Context) (ticketing.CatalogSnapshot, error) {
	rich, ok := t.h.catalog.(richCatalog)
	if !ok {
		return ticketing.CatalogSnapshot{}, errors.New("当前数据源不支持 Agent 查询")
	}
	return rich.Catalog(ctx, false)
}

// Search 按 Agent 给出的条件筛选演出，排序与页面口碑榜保持一致（按样本分层）。
func (t guideToolkit) Search(ctx context.Context, q agent.SearchQuery) ([]agent.Show, error) {
	snapshot, err := t.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	filters := discoveryFilters{
		Query:         q.Query,
		City:          q.City,
		Category:      q.Category,
		DateFrom:      q.DateFrom,
		DateTo:        q.DateTo,
		Budget:        q.Budget,
		MinScore:      q.MinScore,
		AvailableOnly: q.AvailableOnly,
		Sort:          "reputation",
	}
	if q.AvailableOnly && !q.IncludeEnded {
		filters.Lifecycle = "active"
	}
	items := filterShows(snapshot.Items, filters)
	limit := q.Limit
	if limit <= 0 {
		limit = 6
	}
	if limit > 8 {
		limit = 8
	}
	if len(items) > limit {
		items = items[:limit]
	}
	shows := make([]agent.Show, 0, len(items))
	for _, show := range items {
		shows = append(shows, toAgentShow(show))
	}
	return shows, nil
}

// Detail 读取单场演出的票档、评测与注意事项。
func (t guideToolkit) Detail(ctx context.Context, id int64) (agent.Detail, error) {
	snapshot, err := t.snapshot(ctx)
	if err != nil {
		return agent.Detail{}, err
	}
	for _, show := range snapshot.Items {
		if show.ID != id {
			continue
		}
		entry := t.h.comparison(ctx, show, snapshot.Reviews[id], 0)
		return toAgentDetail(entry), nil
	}
	return agent.Detail{}, errors.New("这场演出当前不可公开查看")
}

// Compare 并排读取 2–4 场演出，缺一场就整体失败，避免「只比了一半还下结论」。
func (t guideToolkit) Compare(ctx context.Context, ids []int64) ([]agent.Detail, error) {
	if len(ids) < 2 || len(ids) > 4 {
		return nil, errors.New("对比需要 2–4 场演出")
	}
	snapshot, err := t.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[int64]ticketing.Show, len(snapshot.Items))
	for _, show := range snapshot.Items {
		index[show.ID] = show
	}
	out := make([]agent.Detail, 0, len(ids))
	for _, id := range ids {
		show, ok := index[id]
		if !ok {
			return nil, errors.New("所选演出已不可公开查看")
		}
		out = append(out, toAgentDetail(t.h.comparison(ctx, show, snapshot.Reviews[id], 0)))
	}
	return out, nil
}

// Reviews 读取评测原文，可按关注点做一次轻量相关度排序：
// 命中的原文排前面，但不会隐藏差评——「只挑好评」比「不给原文」更危险。
func (t guideToolkit) Reviews(ctx context.Context, id int64, focus string, limit int) (agent.Detail, error) {
	detail, err := t.Detail(ctx, id)
	if err != nil {
		return agent.Detail{}, err
	}
	if limit <= 0 || limit > 5 {
		limit = 3
	}
	focus = strings.TrimSpace(focus)
	if focus != "" {
		rank := func(quotes []agent.Quote) []agent.Quote {
			sorted := append([]agent.Quote{}, quotes...)
			sort.SliceStable(sorted, func(i, j int) bool {
				return focusScore(sorted[i].Content, focus) > focusScore(sorted[j].Content, focus)
			})
			return sorted
		}
		detail.Positive = rank(detail.Positive)
		detail.Critical = rank(detail.Critical)
	}
	if len(detail.Positive) > limit {
		detail.Positive = detail.Positive[:limit]
	}
	if len(detail.Critical) > limit {
		detail.Critical = detail.Critical[:limit]
	}
	return detail, nil
}

// focusScore 用二元组命中数给评测排序，和常识库检索保持同一套轻量做法。
func focusScore(content string, focus string) int {
	runes := []rune(strings.TrimSpace(focus))
	score := 0
	for index := 0; index+1 < len(runes); index++ {
		if strings.Contains(content, string(runes[index:index+2])) {
			score++
		}
	}
	if len(runes) == 1 && strings.Contains(content, focus) {
		score++
	}
	return score
}

func toAgentShow(show ticketing.Show) agent.Show {
	return agent.Show{
		ID: show.ID, Name: show.Name, TypeName: show.TypeName,
		Area: show.Area, Address: show.Address, OpenTime: show.OpenTime,
		Lifecycle: show.Lifecycle,
		Rating:    show.Rating.Average, RatingCount: show.Rating.Count, LowSample: show.Rating.LowSample,
		MinPrice: show.MinPrice, MaxPrice: show.MaxPrice,
		AvailableStock: show.AvailableStock, HasSeckill: show.HasSeckill,
	}
}

func toAgentDetail(entry comparisonEntry) agent.Detail {
	tickets := make([]agent.Ticket, 0, len(entry.Tickets))
	for _, tier := range entry.Tickets {
		tickets = append(tickets, agent.Ticket{
			ID: tier.ID, Title: tier.Title, Price: tier.Price,
			Stock: tier.Stock, SoldOut: tier.SoldOut,
			Seckill: tier.IsSeckill, SeckillActive: tier.SeckillActive,
			HasSeats: tier.HasSeats, Rules: tier.Rules,
		})
	}
	quotes := func(reviews []ticketing.Review) []agent.Quote {
		out := make([]agent.Quote, 0, len(reviews))
		for _, review := range reviews {
			out = append(out, agent.Quote{ID: review.ID, Author: review.Author, Score: review.Score, Content: review.Content})
		}
		return out
	}
	purchasable := false
	for _, tier := range entry.Tickets {
		if tier.OnSale && !tier.SoldOut && entry.Show.Lifecycle != "ended" {
			purchasable = true
			break
		}
	}
	return agent.Detail{
		Show:        toAgentShow(entry.Show),
		Tickets:     tickets,
		Positive:    quotes(entry.Reviews.Positive),
		Critical:    quotes(entry.Reviews.Critical),
		Reasons:     entry.Reasons,
		Cautions:    entry.Cautions,
		Keywords:    entry.Keywords,
		Purchasable: purchasable,
	}
}
