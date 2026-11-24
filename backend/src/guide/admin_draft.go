package guide

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// draftShow is a structured show proposal generated from the operator's text.
// It is a draft only: the guide cannot write to Echo, so the operator reviews
// and imports it on the Echo side.
type draftShow struct {
	Name      string      `json:"name"`
	TypeName  string      `json:"type_name"`
	City      string      `json:"city"`
	Address   string      `json:"address"`
	Intro     string      `json:"intro"`
	OpenTime  string      `json:"open_time"`
	Tiers     []draftTier `json:"tiers"`
	Generated string      `json:"generated"`
}

type draftTier struct {
	Title        string  `json:"title"`
	Price        float64 `json:"price"`
	Stock        int     `json:"stock"`
	HasSeats     bool    `json:"has_seats"`
	IsSeckill    bool    `json:"is_seckill"`
	SeckillBegin string  `json:"seckill_begin,omitempty"`
	SeckillEnd   string  `json:"seckill_end,omitempty"`
	Rules        string  `json:"rules"`
}

func (h *Handler) adminDraft(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len([]rune(strings.TrimSpace(input.Text))) < 10 {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "请粘贴 10 字以上的演出描述。"})
		return
	}
	if h.model == nil || !h.model.Enabled() {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "草稿生成需要模型服务：请在启动导购前配置 AI_* 环境变量。"})
		return
	}
	system := "你是票务数据录入助手。把用户给出的演出描述整理成 JSON，字段：name（演出名，不超过40字）、type_name（类型，如 话剧/音乐会/儿童剧）、city、address、intro（不超过120字）、open_time（YYYY-MM-DD HH:MM，描述里没有就留空字符串）、tiers 数组（每项 title、price 数字单位元、stock 数字、has_seats 布尔、is_seckill 布尔、seckill_begin/seckill_end（YYYY-MM-DD HH:MM，无则空串）、rules（退改规则一句话）。只输出 JSON，不要解释。价格和余票只能来自用户文字，不能编造；没提到的字段留空或 false。"
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := h.model.Complete(ctx, h.model.SmallModel(), []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: strings.TrimSpace(input.Text)}}, 0.2)
	if err != nil {
		h.write(w, http.StatusBadGateway, map[string]string{"message": "模型生成失败：" + err.Error()})
		return
	}
	cleaned := stripJSONFence(result)
	var draft draftShow
	if err := json.Unmarshal([]byte(cleaned), &draft); err != nil {
		h.write(w, http.StatusBadGateway, map[string]string{"message": "模型没有返回可解析的 JSON，请重试或简化描述。"})
		return
	}
	draft.Name = strings.TrimSpace(draft.Name)
	draft.Generated = time.Now().UTC().Format(time.RFC3339)
	if draft.Name == "" || len(draft.Tiers) == 0 {
		h.write(w, http.StatusBadGateway, map[string]string{"message": "生成的草稿缺少演出名或票档，请补充描述后重试。"})
		return
	}
	draft.Tiers = sanitizeTiers(draft.Tiers)
	if len(draft.Tiers) == 0 {
		h.write(w, http.StatusBadGateway, map[string]string{"message": "生成的票档价格无效，请检查描述里的价格。"})
		return
	}
	h.write(w, http.StatusOK, map[string]any{"draft": draft, "csv": draftCSV(draft)})
}

func sanitizeTiers(tiers []draftTier) []draftTier {
	kept := tiers[:0]
	for _, tier := range tiers {
		tier.Title = strings.TrimSpace(tier.Title)
		if tier.Title == "" {
			tier.Title = "标准票"
		}
		if tier.Price <= 0 || tier.Price > 100000 {
			continue
		}
		if tier.Stock < 0 || tier.Stock > 100000 {
			tier.Stock = 0
		}
		kept = append(kept, tier)
	}
	return kept
}

func stripJSONFence(result string) string {
	text := strings.TrimSpace(result)
	if start := strings.Index(text, "{"); start > 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			return text[start : end+1]
		}
	}
	return text
}

func draftCSV(draft draftShow) string {
	rows := [][]string{{"演出名", "类型", "城市", "场馆", "开演时间", "简介", "票档", "价格(元)", "余票", "需选座", "秒杀", "秒杀开始", "秒杀结束", "退改规则"}}
	for _, tier := range draft.Tiers {
		rows = append(rows, []string{
			draft.Name, draft.TypeName, draft.City, draft.Address, draft.OpenTime, draft.Intro,
			tier.Title, fmt.Sprintf("%.2f", tier.Price), fmt.Sprintf("%d", tier.Stock),
			boolText(tier.HasSeats), boolText(tier.IsSeckill), tier.SeckillBegin, tier.SeckillEnd, tier.Rules,
		})
	}
	return buildCSV(rows)
}

func buildCSV(rows [][]string) string {
	var builder strings.Builder
	builder.WriteString("\uFEFF") // BOM so Excel opens UTF-8 correctly.
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = "\"" + strings.ReplaceAll(cell, "\"", "\"\"") + "\""
		}
		builder.WriteString(strings.Join(cells, ","))
		builder.WriteString("\r\n")
	}
	return builder.String()
}

func boolText(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// csvForCities / csvForTiers let the console download the analytics tables.
func csvForCities(data *catalogAnalytics) string {
	rows := [][]string{{"城市", "演出", "未结束", "历史", "有效评分", "加权均分", "评测数", "最低价", "最高价", "票档", "需选座", "缺票档", "评测不全"}}
	for _, row := range data.Cities {
		rows = append(rows, []string{
			row.City, fmt.Sprintf("%d", row.Shows), fmt.Sprintf("%d", row.Active), fmt.Sprintf("%d", row.Ended),
			fmt.Sprintf("%d", row.RatingCount), fmt.Sprintf("%.2f", row.Average), fmt.Sprintf("%d", row.Reviews),
			priceCell(row.MinPrice), priceCell(row.MaxPrice), fmt.Sprintf("%d", row.Tiers),
			fmt.Sprintf("%d", row.Selectable), fmt.Sprintf("%d", row.MissingTickets), fmt.Sprintf("%d", row.MissingReviews),
		})
	}
	return buildCSV(rows)
}

func csvForTiers(data *catalogAnalytics) string {
	rows := [][]string{{"演出", "城市", "状态", "票档", "副标题", "价格(元)", "余票", "需选座", "秒杀", "秒杀进行中", "售罄", "上架", "缺退改规则"}}
	for _, tier := range data.Tiers {
		rows = append(rows, []string{
			tier.Show, tier.City, tier.Lifecycle, tier.Title, tier.SubTitle,
			fmt.Sprintf("%.2f", tier.Price), fmt.Sprintf("%d", tier.Stock),
			boolText(tier.HasSeats), boolText(tier.IsSeckill), boolText(tier.SeckillActive),
			boolText(tier.SoldOut), boolText(tier.OnSale), boolText(tier.RulesMissing),
		})
	}
	return buildCSV(rows)
}

func priceCell(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%.2f", *value)
}
