package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 工具名常量。新增工具必须同时补三处：常量、specs() 里的说明书、executeTool 的分支，
// 以及 intentTools 的允许清单——少一处模型就永远调不到或调了也执行不了。
const (
	ToolSearchShows  = "search_shows"
	ToolShowDetail   = "show_detail"
	ToolListReviews  = "list_reviews"
	ToolCompareShows = "compare_shows"
	ToolVenueTips    = "venue_tips"
	ToolBudgetPlan   = "budget_plan"

	// 写工具：只生成待确认动作，不直接落库。
	ToolAddWatchlist   = "add_watchlist"
	ToolSavePreference = "save_preference"
)

// toolSpec 是给模型看的工具说明书。description 写清「查什么、什么时候用」，
// args 用一句话列出参数含义，模型据此决定调用与填参。
type toolSpec struct {
	Name        string
	Description string
	Args        string
}

var toolSpecs = []toolSpec{
	{ToolSearchShows, "按城市、类型、日期、单张预算、评分、是否有余票筛选演出，返回候选列表。任何推荐都从这一步开始。", `{"query":"关键词，可空","city":"城市，可空","category":"话剧/音乐剧/舞蹈等，可空","date_from":"YYYY-MM-DD","date_to":"YYYY-MM-DD","budget":"单张预算上限，0 不限","min_score":"最低均分 0-5","available_only":true}`},
	{ToolShowDetail, "读取单场演出的票档、价格、余票、秒杀状态、选座要求与退改规则。", `{"show_id":123}`},
	{ToolListReviews, "读取单场演出的观众评测原文与评分分布，可按关注点检索（如音效、座位、剧情）。", `{"show_id":123,"focus":"关注点，可空","limit":3}`},
	{ToolCompareShows, "并排比较 2–4 场演出的价格、评分、票档与注意事项。", `{"show_ids":[1,2]}`},
	{ToolVenueTips, "查询通用观演常识：入场时间、迟到、儿童、选座、交通、礼仪、字幕等。只给通用建议，不含具体场次事实。", `{"topic":"如 儿童 / 选座 / 迟到"}`},
	{ToolBudgetPlan, "按人数与单张票价计算合计金额、剩余预算、是否超支。金额必须由它计算，不要心算。", `{"party":2,"ticket_price":180,"total_budget":500}`},
	{ToolAddWatchlist, "把某场演出加入用户的观演清单（写操作）。用户说「记一下」「加个提醒」「收藏这场」时使用；调用后只是准备好动作，需要用户确认。", `{"show_id":123,"note":"想看的理由，可空"}`},
	{ToolSavePreference, "保存用户的长期偏好，例如常驻城市、偏好类型、预算上限、座位偏好（写操作）。用户说「以后都看上海的」「我一般预算 500」时使用。", `{"key":"city|category|budget|seat","value":"上海"}`},
}

// writeTools 标记哪些工具是写操作：它们不直接执行，只生成待确认动作。
var writeTools = map[string]bool{ToolAddWatchlist: true, ToolSavePreference: true}

// IsWriteTool 供业务层与测试判断某个工具是否需要人工确认。
func IsWriteTool(tool string) bool { return writeTools[tool] }

// intentTools 是「意图 → 可用工具」的白名单。
// 白名单不是限制能力，而是防止模型在推荐场景里跑去回答退改政策、
// 或者在对比场景里只查一场就下结论。
var intentTools = map[string][]string{
	IntentRecommend:  {ToolSearchShows, ToolShowDetail, ToolListReviews, ToolBudgetPlan, ToolVenueTips, ToolAddWatchlist, ToolSavePreference},
	IntentReviews:    {ToolSearchShows, ToolListReviews},
	IntentCompare:    {ToolCompareShows, ToolShowDetail},
	IntentVenue:      {ToolSearchShows, ToolVenueTips, ToolShowDetail},
	IntentPolicy:     {ToolSearchShows, ToolShowDetail},
	IntentExplore:    {ToolSearchShows, ToolVenueTips, ToolAddWatchlist},
	IntentWatchlist:  {ToolSearchShows, ToolAddWatchlist},
	IntentPreference: {ToolSavePreference},
}

// toolsFor 返回意图允许的工具；未知意图退回最小可用集。
func toolsFor(intent string) []string {
	if tools, ok := intentTools[intent]; ok {
		return tools
	}
	return []string{ToolSearchShows}
}

// toolAllowed 判断某个工具在当前意图下是否可用。
func toolAllowed(intent string, tool string) bool {
	for _, item := range toolsFor(intent) {
		if item == tool {
			return true
		}
	}
	return false
}

// ToolNames 返回全部工具名，供管理端与 /config 展示「这个 Agent 会做什么」。
func ToolNames() []string {
	out := make([]string, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		out = append(out, spec.Name)
	}
	return out
}

// toolSpecText 把说明书拼成提示词段落。
func toolSpecText(intent string) string {
	var builder strings.Builder
	for _, spec := range toolSpecs {
		if !toolAllowed(intent, spec.Name) {
			continue
		}
		fmt.Fprintf(&builder, "- %s：%s 参数：%s\n", spec.Name, spec.Description, spec.Args)
	}
	return builder.String()
}

// toolResult 是一次工具执行的完整结果：既有给模型看的事实，也有给前端看的摘要。
type toolResult struct {
	Tool        string         `json:"tool"`
	OK          bool           `json:"ok"`
	Summary     string         `json:"summary"`
	Observation string         `json:"observation"`
	Shows       []Show         `json:"shows,omitempty"`
	Details     []Detail       `json:"details,omitempty"`
	Budget      *BudgetPlan    `json:"budget,omitempty"`
	Tips        []KnowledgeHit `json:"tips,omitempty"`
	// Pending 仅写工具有值：描述「准备执行什么」，由运行时交给审批队列。
	Pending *PendingAction `json:"pending,omitempty"`
}

// searchDefaults 把业务层已经解析好的需求转成检索默认值，
// 让模型只需要填「它额外想限制的部分」，减少参数幻觉。
//
// 注意：不要把整句用户问题当作 Query 关键词——「推荐一场能买票的话剧」
// 作为一个整体去做包含匹配，永远匹配不到任何演出名。
func searchDefaults(slots Slots) SearchQuery {
	return SearchQuery{
		City:          slots.City,
		Category:      slots.Category,
		DateFrom:      slots.DateFrom,
		DateTo:        slots.DateTo,
		Budget:        slots.PerTicketBudget(),
		AvailableOnly: true,
		Limit:         6,
	}
}

// executeTool 是所有工具的唯一入口（不带检索索引）：解析参数 → 调业务层 → 生成观察。
// 任何参数错误都返回 OK=false 而不是 panic，让 Agent 有机会自我修正。
func executeTool(ctx context.Context, kit Toolkit, tool string, raw json.RawMessage, slots Slots, query string) toolResult {
	return executeToolWith(ctx, kit, nil, tool, raw, slots, query)
}

// executeToolWith 多接收一个检索索引：常识工具走向量召回 + 重排，
// 传 nil 时退回词面匹配（测试与降级路径都走这条）。
func executeToolWith(ctx context.Context, kit Toolkit, kb *KnowledgeIndex, tool string, raw json.RawMessage, slots Slots, query string) toolResult {
	result := toolResult{Tool: tool}
	switch tool {
	case ToolSearchShows:
		var args struct {
			Query         string  `json:"query"`
			City          string  `json:"city"`
			Category      string  `json:"category"`
			DateFrom      string  `json:"date_from"`
			DateTo        string  `json:"date_to"`
			Budget        float64 `json:"budget"`
			MinScore      float64 `json:"min_score"`
			AvailableOnly *bool   `json:"available_only"`
			Limit         int     `json:"limit"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				result.Summary = "参数不是合法 JSON"
				result.Observation = "错误：search_shows 参数应为 JSON 对象，例如 {\"city\":\"上海\"}"
				return result
			}
		}
		q := searchDefaults(slots)
		if strings.TrimSpace(args.Query) != "" {
			q.Query = strings.TrimSpace(args.Query)
		}
		if strings.TrimSpace(args.City) != "" {
			q.City = strings.TrimSpace(args.City)
		}
		if strings.TrimSpace(args.Category) != "" {
			q.Category = strings.TrimSpace(args.Category)
		}
		if args.DateFrom != "" {
			q.DateFrom = args.DateFrom
		}
		if args.DateTo != "" {
			q.DateTo = args.DateTo
		}
		if args.Budget > 0 {
			q.Budget = args.Budget
		}
		if args.MinScore > 0 && args.MinScore <= 5 {
			q.MinScore = args.MinScore
		}
		if args.AvailableOnly != nil {
			q.AvailableOnly = *args.AvailableOnly
		}
		if args.Limit > 0 && args.Limit <= 8 {
			q.Limit = args.Limit
		}
		shows, err := kit.Search(ctx, q)
		if err != nil {
			result.Summary = "检索失败"
			result.Observation = "错误：" + err.Error()
			return result
		}
		result.OK = true
		result.Shows = shows
		result.Summary = fmt.Sprintf("检索到 %d 场符合条件的演出", len(shows))
		result.Observation = describeShows(shows)

	case ToolShowDetail:
		id, err := argID(raw, "show_id")
		if err != nil {
			result.Summary = "缺少演出编号"
			result.Observation = "错误：" + err.Error()
			return result
		}
		detail, err := kit.Detail(ctx, id)
		if err != nil {
			result.Summary = "读取演出失败"
			result.Observation = "错误：" + err.Error()
			return result
		}
		result.OK = true
		result.Details = []Detail{detail}
		result.Summary = fmt.Sprintf("读取《%s》的 %d 个票档", detail.Show.Name, len(detail.Tickets))
		result.Observation = describeDetail(detail, true)

	case ToolListReviews:
		id, err := argID(raw, "show_id")
		if err != nil {
			result.Summary = "缺少演出编号"
			result.Observation = "错误：" + err.Error()
			return result
		}
		var args struct {
			Focus string `json:"focus"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(raw, &args)
		limit := args.Limit
		if limit <= 0 || limit > 5 {
			limit = 3
		}
		detail, err := kit.Reviews(ctx, id, args.Focus, limit)
		if err != nil {
			result.Summary = "读取评测失败"
			result.Observation = "错误：" + err.Error()
			return result
		}
		result.OK = true
		result.Details = []Detail{detail}
		result.Summary = fmt.Sprintf("读取《%s》的 %d 条评测原文", detail.Show.Name, len(detail.Positive)+len(detail.Critical))
		result.Observation = describeDetail(detail, false)

	case ToolCompareShows:
		var args struct {
			ShowIDs []int64 `json:"show_ids"`
		}
		_ = json.Unmarshal(raw, &args)
		ids := args.ShowIDs
		if len(ids) == 0 {
			ids = slots.ShowIDs
		}
		if len(ids) < 2 || len(ids) > 4 {
			result.Summary = "对比需要 2–4 场演出"
			result.Observation = "错误：compare_shows 需要 2–4 个 show_ids"
			return result
		}
		details, err := kit.Compare(ctx, ids)
		if err != nil {
			result.Summary = "对比失败"
			result.Observation = "错误：" + err.Error()
			return result
		}
		result.OK = true
		result.Details = details
		result.Summary = fmt.Sprintf("对比了 %d 场演出", len(details))
		var builder strings.Builder
		for index, detail := range details {
			fmt.Fprintf(&builder, "第 %d 场：\n%s\n", index+1, describeDetail(detail, true))
		}
		result.Observation = builder.String()

	case ToolVenueTips:
		var args struct {
			Topic string `json:"topic"`
		}
		_ = json.Unmarshal(raw, &args)
		topic := strings.TrimSpace(args.Topic)
		if topic == "" {
			topic = query
		}
		tips := knowledgeHits(ctx, kb, topic, 3)
		result.OK = len(tips) > 0
		result.Tips = tips
		if len(tips) == 0 {
			result.Summary = "常识库里没有匹配条目"
			result.Observation = "没有检索到相关通用建议；不要编造，直接说明暂时没有这条资料。"
			return result
		}
		result.Summary = fmt.Sprintf("命中 %d 条观演常识", len(tips))
		var builder strings.Builder
		for _, tip := range tips {
			fmt.Fprintf(&builder, "【%s】%s\n", tip.Title, tip.Content)
		}
		result.Observation = builder.String()

	case ToolBudgetPlan:
		var args struct {
			Party       int     `json:"party"`
			TicketPrice float64 `json:"ticket_price"`
			TotalBudget float64 `json:"total_budget"`
		}
		_ = json.Unmarshal(raw, &args)
		party := args.Party
		if party <= 0 {
			party = max(slots.Party, 1)
		}
		total := args.TotalBudget
		if total <= 0 {
			total = slots.TotalBudget()
		}
		price := args.TicketPrice
		if price <= 0 {
			price = cheapestPrice(slots, total, party)
		}
		if price <= 0 {
			result.Summary = "缺少票价"
			result.Observation = "错误：budget_plan 需要 ticket_price（单张票价）"
			return result
		}
		plan := PlanBudget(party, price, total)
		result.OK = true
		result.Budget = &plan
		result.Summary = fmt.Sprintf("%d 人 × ¥%.0f = ¥%.0f", party, price, plan.TicketCost)
		result.Observation = fmt.Sprintf("人数 %d，单张 ¥%.2f，门票合计 ¥%.2f，%s。",
			party, price, plan.TicketCost, budgetVerdict(plan))

	case ToolAddWatchlist:
		var args struct {
			ShowID int64  `json:"show_id"`
			Note   string `json:"note"`
		}
		_ = json.Unmarshal(raw, &args)
		showID := args.ShowID
		if showID <= 0 && len(slots.ShowIDs) > 0 {
			showID = slots.ShowIDs[0]
		}
		if showID <= 0 {
			result.Summary = "缺少演出编号"
			result.Observation = "错误：add_watchlist 需要 show_id，可先用 search_shows 查到"
			return result
		}
		name := showNameByID(slots, showID)
		summary := "把《" + fallbackName(name, showID) + "》加入观演清单"
		if note := strings.TrimSpace(args.Note); note != "" {
			summary += "，备注：" + truncateRunes(note, 40)
		}
		result.Pending = &PendingAction{
			Kind: ActionWatchlist, Title: "加入观演清单", Summary: summary, Risk: "low",
			ShowID: showID, ShowName: name, Value: strings.TrimSpace(args.Note),
		}
		result.OK = true
		result.Summary = "已准备：加入观演清单（待确认）"
		result.Observation = "写操作已准备，等待用户确认后执行。"

	case ToolSavePreference:
		var args struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &args)
		key, value, err := normalizePreference(args.Key, args.Value)
		if err != nil {
			result.Summary = "偏好参数不完整"
			result.Observation = "错误：" + err.Error()
			return result
		}
		result.Pending = &PendingAction{
			Kind: ActionPreference, Title: "记住偏好", Risk: "low",
			Summary: "记住偏好：" + preferenceLabel(key) + " = " + value,
			Key:     key, Value: value,
		}
		result.OK = true
		result.Summary = "已准备：保存偏好（待确认）"
		result.Observation = "写操作已准备，等待用户确认后执行。"

	default:
		result.Summary = "未知工具"
		result.Observation = "错误：没有名为 " + tool + " 的工具，请只使用说明书里列出的工具。"
	}
	return result
}

// normalizePreference 只接受白名单键，值做长度与范围校验。
// 不允许模型自由发挥键名，否则偏好表会变成一堆没人认识的自定义字段。
func normalizePreference(key string, value string) (string, string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	switch key {
	case "city":
		if value == "" || len([]rune(value)) > 12 {
			return "", "", fmt.Errorf("城市不能为空且不超过 12 字")
		}
		return key, value, nil
	case "category":
		if value == "" || len([]rune(value)) > 12 {
			return "", "", fmt.Errorf("类型不能为空且不超过 12 字")
		}
		return key, value, nil
	case "seat":
		if value != "front" && value != "adjacent" && value != "" {
			return "", "", fmt.Errorf("seat 只能是 front / adjacent")
		}
		return key, value, nil
	case "budget":
		amount, err := strconv.ParseFloat(strings.TrimPrefix(value, "¥"), 64)
		if err != nil || amount <= 0 || amount > 100000 {
			return "", "", fmt.Errorf("budget 需要 0–100000 之间的数字")
		}
		return key, strconv.FormatFloat(amount, 'f', -1, 64), nil
	default:
		return "", "", fmt.Errorf("key 只能是 city / category / seat / budget")
	}
}

func preferenceLabel(key string) string {
	switch key {
	case "city":
		return "常驻城市"
	case "category":
		return "偏好类型"
	case "seat":
		return "座位偏好"
	case "budget":
		return "预算习惯"
	default:
		return key
	}
}

func showNameByID(slots Slots, id int64) string {
	for index, showID := range slots.ShowIDs {
		if showID == id && index < len(slots.ShowNames) {
			return slots.ShowNames[index]
		}
	}
	return ""
}

func fallbackName(name string, id int64) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return "演出 #" + strconv.FormatInt(id, 10)
}

// PlanBudget 是预算计算器本体。抽成纯函数，单测直接覆盖边界（0 人、超支、刚好打平）。
func PlanBudget(party int, price float64, total float64) BudgetPlan {
	if party < 1 {
		party = 1
	}
	cost := round2(price * float64(party))
	plan := BudgetPlan{Party: party, TotalBudget: round2(total), TicketPrice: round2(price), TicketCost: cost}
	if total > 0 {
		plan.Remaining = round2(total - cost)
		plan.OverBudget = plan.Remaining < 0
	}
	return plan
}

func budgetVerdict(plan BudgetPlan) string {
	switch {
	case plan.TotalBudget <= 0:
		return "未设置总预算"
	case plan.OverBudget:
		return fmt.Sprintf("超出总预算 ¥%.2f", -plan.Remaining)
	default:
		return fmt.Sprintf("总预算 ¥%.2f 内，剩余 ¥%.2f", plan.TotalBudget, plan.Remaining)
	}
}

func round2(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

// cheapestPrice 在没有显式票价时给一个兜底：用单张预算，保证计算器仍能演示。
func cheapestPrice(slots Slots, total float64, party int) float64 {
	if slots.PerTicketBudget() > 0 {
		return slots.PerTicketBudget()
	}
	if total > 0 && party > 0 {
		return total / float64(party)
	}
	return 0
}

func argID(raw json.RawMessage, key string) (int64, error) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return 0, fmt.Errorf("参数不是合法 JSON")
	}
	value, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("缺少 %s", key)
	}
	switch typed := value.(type) {
	case float64:
		if typed <= 0 {
			return 0, fmt.Errorf("%s 必须为正整数", key)
		}
		return int64(typed), nil
	case string:
		id, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("%s 必须为正整数", key)
		}
		return id, nil
	default:
		return 0, fmt.Errorf("%s 类型不正确", key)
	}
}

func describeShows(shows []Show) string {
	if len(shows) == 0 {
		return "没有符合条件的演出。"
	}
	var builder strings.Builder
	for _, show := range shows {
		fmt.Fprintf(&builder, "show_id=%d 《%s》 %s/%s，%s，均分 %.2f（%d 个评分%s），最低 ¥%.0f，余票 %d。\n",
			show.ID, show.Name, show.TypeName, show.Area, show.OpenTime,
			show.Rating, show.RatingCount, sampleNote(show.LowSample), priceOf(show.MinPrice), show.AvailableStock)
	}
	return builder.String()
}

func describeDetail(detail Detail, withTickets bool) string {
	var builder strings.Builder
	show := detail.Show
	fmt.Fprintf(&builder, "show_id=%d 《%s》 %s/%s，%s，lifecycle=%s，均分 %.2f（%d 个评分%s），最低 ¥%.0f。\n",
		show.ID, show.Name, show.TypeName, show.Area, show.OpenTime, show.Lifecycle,
		show.Rating, show.RatingCount, sampleNote(show.LowSample), priceOf(show.MinPrice))
	if withTickets {
		for _, ticket := range detail.Tickets {
			meta := []string{fmt.Sprintf("¥%.2f", ticket.Price)}
			if ticket.SoldOut {
				meta = append(meta, "已售罄")
			} else {
				meta = append(meta, fmt.Sprintf("余票 %d", ticket.Stock))
			}
			if ticket.HasSeats {
				meta = append(meta, "需选座")
			}
			if ticket.Seckill {
				status := "秒杀未开始/已结束"
				if ticket.SeckillActive {
					status = "秒杀进行中"
				}
				meta = append(meta, status)
			}
			fmt.Fprintf(&builder, "  票档：%s（%s）%s\n", ticket.Title, strings.Join(meta, " · "), ruleNote(ticket.Rules))
		}
	}
	for _, quote := range append(append([]Quote{}, detail.Positive...), detail.Critical...) {
		content := []rune(quote.Content)
		if len(content) > 100 {
			content = append(content[:100], '…')
		}
		fmt.Fprintf(&builder, "  评测 #%d %s（%.0f 星）：「%s」\n", quote.ID, quote.Author, quote.Score, string(content))
	}
	for _, reason := range detail.Reasons {
		fmt.Fprintf(&builder, "  依据：%s\n", reason)
	}
	for _, caution := range detail.Cautions {
		fmt.Fprintf(&builder, "  注意：%s\n", caution)
	}
	if len(detail.Keywords) > 0 {
		fmt.Fprintf(&builder, "  观众反复提到：%s\n", strings.Join(detail.Keywords, "、"))
	}
	return builder.String()
}

func sampleNote(low bool) string {
	if low {
		return "，样本少"
	}
	return ""
}

func priceOf(price *float64) float64 {
	if price == nil {
		return 0
	}
	return *price
}

func ruleNote(rules string) string {
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return "（未提供退改规则）"
	}
	runes := []rune(rules)
	if len(runes) > 40 {
		rules = string(runes[:40]) + "…"
	}
	return "｜" + rules
}

// knowledgeHits 统一常识检索入口：有索引走召回+重排，没有就走词面匹配。
func knowledgeHits(ctx context.Context, kb *KnowledgeIndex, topic string, limit int) []KnowledgeHit {
	if kb != nil {
		if hits := kb.Search(ctx, topic, limit); len(hits) > 0 {
			return hits
		}
	}
	return SearchKnowledge(topic, limit)
}
