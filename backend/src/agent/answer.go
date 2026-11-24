package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildAnswer 是「数据版回答」：不经过模型，完全由工具结果拼出来。
//
// 它有两个作用——模型不可用时的最终答案，以及模型回答的自检锚点：
// 模型写跑偏了，就用它替换，保证用户至少拿到一份可信的结论。
func buildAnswer(plan Plan, slots Slots, found evidence, memoryNote string) string {
	var builder strings.Builder
	builder.WriteString("已记住：" + describeSlots(slots) + "。" + memoryNote + "\n")

	if len(found.showIDs) == 0 && len(found.tips) == 0 && found.budget == nil {
		builder.WriteString(noResultText(slots))
		return builder.String()
	}

	builder.WriteString("\n")
	details := detailIndex(found.details)
	shown := 0
	for _, id := range found.showIDs {
		detail, ok := details[id]
		if !ok {
			continue
		}
		shown++
		writeDetail(&builder, detail, slots)
		if shown >= 3 {
			break
		}
	}
	if shown == 0 && len(found.shows) > 0 {
		writeShows(&builder, found.shows, 3)
	}

	if found.budget != nil {
		builder.WriteString("\n" + budgetSentence(*found.budget) + "\n")
	}
	if len(found.actions) > 0 {
		builder.WriteString("\n需要你确认后才会执行：\n")
		for _, action := range found.actions {
			fmt.Fprintf(&builder, "- %s（编号 %s，当前未执行）\n", action.Summary, action.ID)
		}
	}
	if len(found.tips) > 0 {
		builder.WriteString("\n通用建议（具体以场馆公告为准）：\n")
		for index, tip := range found.tips {
			if index >= 2 {
				break
			}
			fmt.Fprintf(&builder, "- %s：%s\n", tip.Title, tip.Content)
		}
	}
	if len(found.tools) > 0 {
		builder.WriteString("\n本轮查询：" + strings.Join(uniqueStrings(found.tools), " → ") + "\n")
	}
	builder.WriteString("\n需要买票时，点票档下的「确认订单」到 Echo 核对数量、座位与退改规则后自行提交，付款需单独确认；我只能帮你判断，不能代下单。")
	return builder.String()
}

func writeDetail(builder *strings.Builder, detail Detail, slots Slots) {
	show := detail.Show
	location := strings.TrimSpace(show.Area)
	if address := strings.TrimSpace(show.Address); address != "" && address != location {
		location += " · " + address
	}
	fmt.Fprintf(builder, "《%s》 · %s · %s\n", show.Name, show.TypeName, location)
	if show.OpenTime != "" {
		fmt.Fprintf(builder, "开演时间：%s%s\n", show.OpenTime, endedNote(show.Lifecycle))
	}
	if show.RatingCount > 0 {
		fmt.Fprintf(builder, "观众均分 %.2f/5（%d 个有效评分%s）\n", show.Rating, show.RatingCount, sampleNote(show.LowSample))
	} else {
		builder.WriteString("暂无有效观众评分，口碑无样本参考。\n")
	}

	available := make([]Ticket, 0, len(detail.Tickets))
	for _, ticket := range detail.Tickets {
		if !ticket.SoldOut {
			available = append(available, ticket)
		}
	}
	for index, ticket := range available {
		if index >= 3 {
			break
		}
		meta := []string{fmt.Sprintf("¥%.2f", ticket.Price), fmt.Sprintf("余票 %d", ticket.Stock)}
		if ticket.HasSeats {
			meta = append(meta, "需选座")
		}
		if ticket.Seckill && ticket.SeckillActive {
			meta = append(meta, "秒杀进行中")
		}
		fmt.Fprintf(builder, "票档：%s（%s）%s\n", ticket.Title, strings.Join(meta, " · "), rulesShort(ticket.Rules))
	}
	if len(available) == 0 && len(detail.Tickets) > 0 {
		builder.WriteString("票档：当前票档均已售罄，可关注后续放票。\n")
	}
	if len(detail.Tickets) == 0 {
		builder.WriteString("票档：暂未读到可售票档。\n")
	}
	if slots.Party > 1 && len(available) > 0 {
		low := available[0]
		if low.Stock < slots.Party {
			fmt.Fprintf(builder, "注意：最便宜的票档余票 %d 张，少于你需要 %d 人连座/同行。\n", low.Stock, slots.Party)
		}
	}

	quoted := 0
	for _, quote := range append(append([]Quote{}, detail.Positive...), detail.Critical...) {
		if quoted >= 2 {
			break
		}
		fmt.Fprintf(builder, "观众原话 #%d，%s（%.0f 星）：「%s」\n", quote.ID, quote.Author, quote.Score, trimQuote(quote.Content, 90))
		quoted++
	}
	for _, reason := range detail.Reasons {
		if len([]rune(reason)) > 0 {
			fmt.Fprintf(builder, "依据：%s\n", reason)
			break
		}
	}
	for index, caution := range detail.Cautions {
		if index >= 2 {
			break
		}
		fmt.Fprintf(builder, "注意：%s\n", caution)
	}
	if len(detail.Keywords) > 0 {
		fmt.Fprintf(builder, "观众反复提到：%s\n", strings.Join(detail.Keywords, "、"))
	}
	builder.WriteString("\n")
}

func writeShows(builder *strings.Builder, shows []Show, limit int) {
	seen := map[int64]bool{}
	count := 0
	for _, show := range shows {
		if seen[show.ID] || count >= limit {
			continue
		}
		seen[show.ID] = true
		count++
		fmt.Fprintf(builder, "《%s》 · %s · %s · 均分 %.2f（%d 个评分%s）· 最低 ¥%.0f\n",
			show.Name, show.TypeName, show.Area, show.Rating, show.RatingCount, sampleNote(show.LowSample), priceOf(show.MinPrice))
	}
}

func budgetSentence(plan BudgetPlan) string {
	if plan.TotalBudget <= 0 {
		return fmt.Sprintf("门票合计：%d 人 × ¥%.2f = ¥%.2f（未设置总预算）。", plan.Party, plan.TicketPrice, plan.TicketCost)
	}
	if plan.OverBudget {
		return fmt.Sprintf("门票合计：%d 人 × ¥%.2f = ¥%.2f，超出总预算 ¥%.2f，需要换更低的票档或减少人数。", plan.Party, plan.TicketPrice, plan.TicketCost, -plan.Remaining)
	}
	return fmt.Sprintf("门票合计：%d 人 × ¥%.2f = ¥%.2f，在总预算 ¥%.2f 内，剩余 ¥%.2f。", plan.Party, plan.TicketPrice, plan.TicketCost, plan.TotalBudget, plan.Remaining)
}

func noResultText(slots Slots) string {
	parts := []string{}
	if slots.City != "" {
		parts = append(parts, "城市 "+slots.City)
	}
	if slots.Category != "" {
		parts = append(parts, "类型 "+slots.Category)
	}
	if slots.DateFrom != "" {
		parts = append(parts, "日期 "+slots.DateFrom)
	}
	if budget := slots.PerTicketBudget(); budget > 0 {
		parts = append(parts, fmt.Sprintf("单张预算 ¥%.2f", budget))
	}
	scope := "当前条件"
	if len(parts) > 0 {
		scope = strings.Join(parts, "、")
	}
	return "按" + scope + "没有找到可购演出。可以放宽一个条件再试：换个日期、取消类型限制、提高预算，或者只看有余票的场次。"
}

func endedNote(lifecycle string) string {
	if lifecycle == "ended" {
		return "（已结束，仅作历史回顾，不能购买）"
	}
	return ""
}

func rulesShort(rules string) string {
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return ""
	}
	return "｜" + trimQuote(rules, 30)
}

func trimQuote(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func detailIndex(details []Detail) map[int64]Detail {
	index := make(map[int64]Detail, len(details))
	for _, detail := range details {
		if detail.Show.ID > 0 {
			index[detail.Show.ID] = detail
		}
	}
	return index
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// buildFacts 把工具结果压成给模型的「唯一数据来源」。
// 它同时是审计材料：模型说的每个数字，都应该能在这里找到。
func buildFacts(plan Plan, slots Slots, found evidence) string {
	facts := map[string]any{
		"intent": plan.Intent,
		"slots":  slots,
		"tools":  uniqueStrings(found.tools),
		"shows":  found.shows,
	}
	if len(found.details) > 0 {
		facts["details"] = found.details
	}
	if found.budget != nil {
		facts["budget"] = found.budget
	}
	if len(found.tips) > 0 {
		facts["knowledge"] = found.tips
	}
	payload, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return "{}"
	}
	return truncateRunes(string(payload), 6000)
}
