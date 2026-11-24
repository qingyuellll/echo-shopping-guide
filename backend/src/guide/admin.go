package guide

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"
)

type adminSettings struct {
	token  string
	hashes [][]byte
}

func (a adminSettings) enabled() bool { return strings.TrimSpace(a.token) != "" || len(a.hashes) > 0 }

// matches compares a candidate against the plaintext token (if configured) or
// against any SHA-256 hash, using constant-time comparisons. Hashes let the
// operator rotate or issue several tokens without storing them in plaintext.
func (a adminSettings) matches(candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if a.token != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1 {
		return true
	}
	if len(a.hashes) == 0 {
		return false
	}
	digest := sha256.Sum256([]byte(candidate))
	for _, hash := range a.hashes {
		if subtle.ConstantTimeCompare(digest[:], hash) == 1 {
			return true
		}
	}
	return false
}

func loadAdminSettings() adminSettings {
	settings := adminSettings{token: strings.TrimSpace(os.Getenv("GUIDE_ADMIN_TOKEN"))}
	for _, value := range strings.Split(os.Getenv("GUIDE_ADMIN_TOKEN_SHA256"), ",") {
		value = strings.ToLower(strings.TrimSpace(value))
		if len(value) != 64 {
			continue
		}
		if digest, err := hex.DecodeString(value); err == nil {
			settings.hashes = append(settings.hashes, digest)
		}
	}
	return settings
}

// AdminHandler serves the management console for the shopping assistant.
// It is mounted separately from the public ticketing routes.
func (h *Handler) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/guide-admin/session", h.adminSession)
	mux.Handle("GET /api/v1/guide-admin/overview", h.requireAdmin(h.adminOverview))
	mux.Handle("GET /api/v1/guide-admin/prompt", h.requireAdmin(h.adminPrompt))
	mux.Handle("PUT /api/v1/guide-admin/prompt", h.requireAdmin(h.adminPromptSave))
	mux.Handle("POST /api/v1/guide-admin/eval", h.requireAdmin(h.adminEval))
	mux.Handle("GET /api/v1/guide-admin/catalog", h.requireAdmin(h.adminCatalog))
	mux.Handle("POST /api/v1/guide-admin/ask", h.requireAdmin(h.adminAsk))
	mux.Handle("GET /api/v1/guide-admin/cases", h.requireAdmin(h.adminCasesList))
	mux.Handle("POST /api/v1/guide-admin/cases", h.requireAdmin(h.adminCasesAdd))
	mux.Handle("DELETE /api/v1/guide-admin/cases/{id}", h.requireAdmin(h.adminCasesDelete))
	mux.Handle("POST /api/v1/guide-admin/draft", h.requireAdmin(h.adminDraft))
	mux.Handle("GET /api/v1/guide-admin/agent/runs", h.requireAdmin(h.adminAgentRuns))
	mux.Handle("GET /api/v1/guide-admin/agent/runs/{id}", h.requireAdmin(h.adminAgentRun))
	mux.Handle("GET /api/v1/guide-admin/agent/actions", h.requireAdmin(h.adminAgentActions))
	mux.Handle("GET /api/v1/guide-admin/agent/checkpoints", h.requireAdmin(h.adminAgentCheckpoints))
	mux.Handle("POST /api/v1/guide-admin/agent/rag-eval", h.requireAdmin(h.adminAgentRagEval))
	mux.Handle("POST /api/v1/guide-admin/agent/bench", h.requireAdmin(h.adminAgentBench))
	return mux
}

func (h *Handler) authorized(r *http.Request) bool {
	if !h.admin.enabled() {
		return false
	}
	return h.admin.matches(r.Header.Get("X-Guide-Admin-Token"))
}

func (h *Handler) requireAdmin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.admin.enabled() {
			h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "管理端未启用：请在启动导购服务前设置 GUIDE_ADMIN_TOKEN。"})
			return
		}
		if !h.authorized(r) {
			h.write(w, http.StatusUnauthorized, map[string]string{"message": "管理口令无效，请重新输入。"})
			return
		}
		next(w, r)
	})
}

func (h *Handler) adminSession(w http.ResponseWriter, r *http.Request) {
	model := map[string]any{"enabled": false}
	if h.model != nil {
		config := h.model.Config()
		model = map[string]any{"enabled": h.model.Enabled(), "model": config.LargeModel, "thinking": config.EnableThinking}
	}
	_, version, updatedAt := h.prompt.current()
	h.write(w, http.StatusOK, map[string]any{
		"enabled":       h.admin.enabled(),
		"authenticated": h.authorized(r),
		"model":         model,
		"prompt":        map[string]any{"version": version, "updated_at": updatedAt},
		"started_at":    h.started,
	})
}

type healthItem struct {
	Level  string `json:"level"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前数据源不支持管理概览。"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := catalog.Catalog(ctx, r.URL.Query().Get("refresh") == "true")
	if err != nil {
		h.unavailable(w, err)
		return
	}
	coverage := snapshot.Coverage
	health := make([]healthItem, 0, 6)

	if coverage.Complete {
		health = append(health, healthItem{"ok", "演出读取完整", fmt.Sprintf("已读取 %d / %d 场公开演出。", coverage.Loaded, coverage.PublicTotal)})
	} else {
		health = append(health, healthItem{"warn", "演出读取不完整", fmt.Sprintf("已读取 %d / %d 场，缺失场次不会出现在推荐里。", coverage.Loaded, coverage.PublicTotal)})
	}

	lowSample := make([]string, 0, 5)
	unrated := 0
	missingTickets := make([]string, 0, 5)
	incomplete := make([]string, 0, 5)
	for _, show := range snapshot.Items {
		switch {
		case !show.Rating.Available || show.Rating.Count == 0:
			unrated++
		case show.Rating.Count < 5:
			if len(lowSample) < 5 {
				lowSample = append(lowSample, fmt.Sprintf("《%s》%d 个", show.Name, show.Rating.Count))
			}
		}
		if !show.TicketsKnown && len(missingTickets) < 5 {
			missingTickets = append(missingTickets, "《"+show.Name+"》")
		}
		if !show.Rating.Complete && len(incomplete) < 5 {
			incomplete = append(incomplete, "《"+show.Name+"》")
		}
	}
	if len(lowSample) > 0 {
		health = append(health, healthItem{"warn", "评分样本偏少", fmt.Sprintf("%d 场有效评分少于 5 个，排序中已靠后：%s", len(lowSample), strings.Join(lowSample, "、"))})
	}
	if unrated > 0 {
		health = append(health, healthItem{"warn", "没有观众评分", fmt.Sprintf("%d 场暂无可引用评分，不建议作为口碑推荐依据。", unrated)})
	}
	// Ticket analytics already counts shows with no ticket record at all, so the
	// overview and the ticket card cannot disagree.
	var ticketData *catalogAnalytics
	if data, err := h.analytics(ctx, catalog, false); err == nil {
		ticketData = data
	}
	if ticketData != nil && ticketData.Summary.ShowsNoTickets > 0 {
		health = append(health, healthItem{"warn", "票档缺失", fmt.Sprintf("%d 场没有任何票档记录，无法进入购票推荐。", ticketData.Summary.ShowsNoTickets)})
	} else if len(missingTickets) > 0 {
		health = append(health, healthItem{"warn", "票档未读取", fmt.Sprintf("%d 场票档缺失或读取失败：%s", len(missingTickets), strings.Join(missingTickets, "、"))})
	}
	if ticketData != nil && ticketData.Summary.MissingRules > 0 {
		health = append(health, healthItem{"warn", "退改规则缺失", fmt.Sprintf("%d / %d 个票档没有填写退改规则，详情页会提示到 Echo 确认。", ticketData.Summary.MissingRules, ticketData.Summary.Tiers)})
	}
	if ticketData != nil && ticketData.Summary.SuspiciousPrice > 0 {
		health = append(health, healthItem{"warn", "异常低价票档", fmt.Sprintf("%d 个票档价格低于 ¥10，可能是测试或占位数据。", ticketData.Summary.SuspiciousPrice)})
	}
	if len(incomplete) > 0 {
		health = append(health, healthItem{"warn", "评测统计不完整", fmt.Sprintf("%d 场评测未读完整，均分仅供参考：%s", len(incomplete), strings.Join(incomplete, "、"))})
	}
	if len(incomplete) == 0 && len(missingTickets) == 0 && unrated == 0 && len(lowSample) == 0 {
		health = append(health, healthItem{"ok", "评分与票档齐备", "所有公开演出都有评分与票档数据。"})
	}

	model := map[string]any{"enabled": false}
	if h.model != nil {
		config := h.model.Config()
		model = map[string]any{"enabled": h.model.Enabled(), "model": config.LargeModel, "thinking": config.EnableThinking}
	}
	_, version, promptUpdated := h.prompt.current()
	h.write(w, http.StatusOK, map[string]any{
		"coverage":   coverage,
		"health":     health,
		"model":      model,
		"prompt":     map[string]any{"version": version, "updated_at": promptUpdated},
		"stats":      h.stats.view(h.started),
		"cities":     len(snapshot.Cities),
		"categories": len(snapshot.Categories),
		"updated_at": coverage.UpdatedAt,
		"cache_note": "演出与评测缓存 30 秒；存在缺失时缩短为 5 秒。",
	})
}

func (h *Handler) adminPrompt(w http.ResponseWriter, r *http.Request) {
	text, version, updatedAt := h.prompt.current()
	h.write(w, http.StatusOK, map[string]any{
		"text":       text,
		"version":    version,
		"updated_at": updatedAt,
		"default":    defaultAssistantPrompt,
		"max_runes":  promptMaxRunes,
		"history":    h.prompt.historyCopy(),
	})
}

func (h *Handler) adminPromptSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input struct {
		Text    string `json:"text"`
		Restore bool   `json:"restore"`
		Version int    `json:"version"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "请求内容不是有效 JSON。"})
		return
	}
	// Editing the prompt can change what the assistant tells visitors, so the
	// request must repeat the admin token: a stolen idle tab is not enough.
	if !h.admin.matches(input.Confirm) {
		h.write(w, http.StatusForbidden, map[string]string{"message": "修改提示词需要再次输入管理口令确认。"})
		return
	}
	var (
		version   int
		updatedAt string
		err       error
		action    string
	)
	switch {
	case input.Version > 0:
		text, ok := h.prompt.versionText(input.Version)
		if !ok {
			h.write(w, http.StatusBadRequest, map[string]string{"message": fmt.Sprintf("找不到第 %d 版的记录，可能已被清理。", input.Version)})
			return
		}
		action = "rollback"
		version, updatedAt, err = h.prompt.save(text, action)
	case input.Restore:
		action = "restore"
		version, updatedAt, err = h.prompt.restore()
	default:
		text := strings.TrimSpace(input.Text)
		if len([]rune(text)) < 60 {
			h.write(w, http.StatusBadRequest, map[string]string{"message": "提示词过短（至少 60 字），容易丢失约束。"})
			return
		}
		if len([]rune(text)) > promptMaxRunes {
			h.write(w, http.StatusBadRequest, map[string]string{"message": fmt.Sprintf("提示词超过 %d 字上限。", promptMaxRunes)})
			return
		}
		action = "update"
		version, updatedAt, err = h.prompt.save(text, action)
	}
	if err != nil {
		h.write(w, http.StatusInternalServerError, map[string]string{"message": "保存失败：" + err.Error()})
		return
	}
	// Every accepted change is verified immediately: a prompt that breaks
	// planning or budget parsing is caught here, not by visitors.
	response := map[string]any{"version": version, "updated_at": updatedAt, "action": action}
	if _, rich := h.catalog.(richCatalog); rich {
		evalCtx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		started := time.Now()
		report := h.runEval(evalCtx)
		cancel()
		report.DurationMS = time.Since(started).Milliseconds()
		response["eval"] = report
	}
	h.write(w, http.StatusOK, response)
}

type evalCheck struct {
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Got   string `json:"got"`
}

type evalCaseResult struct {
	Name     string      `json:"name"`
	Question string      `json:"question"`
	Pass     bool        `json:"pass"`
	Checks   []evalCheck `json:"checks"`
}

type evalReport struct {
	RanAt      string           `json:"ran_at"`
	DurationMS int64            `json:"duration_ms"`
	Passed     int              `json:"passed"`
	Total      int              `json:"total"`
	Cases      []evalCaseResult `json:"cases"`
	Note       string           `json:"note"`
}

type probeInput struct {
	Message string
	Plan    *shoppingPlan
	Budget  float64
}

type probeResult struct {
	Answer       string
	Mode         string
	Intent       string
	Plan         shoppingPlan
	Filters      discoveryFilters
	Items        []comparisonEntry
	Alternatives []comparisonEntry
}

func (h *Handler) adminEval(w http.ResponseWriter, r *http.Request) {
	catalog, ok := h.catalog.(richCatalog)
	if !ok {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前数据源不支持助手巡检。"})
		return
	}
	// Without live data every case would fail for the same reason; say so instead.
	probeCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	_, probeErr := catalog.Catalog(probeCtx, false)
	cancel()
	if probeErr != nil {
		h.unavailable(w, probeErr)
		return
	}
	started := time.Now()
	report := h.runEval(r.Context())
	report.DurationMS = time.Since(started).Milliseconds()
	h.write(w, http.StatusOK, report)
}

// runEval replays fixed requirements against the real catalog with the model
// turned off, so a failure points at planning or data rather than prompt wording.
func (h *Handler) runEval(ctx context.Context) evalReport {
	probe := &Handler{catalog: h.catalog, prompt: h.prompt, stats: newAskStats(), admin: h.admin, started: h.started}
	report := evalReport{RanAt: time.Now().UTC().Format(time.RFC3339), Cases: []evalCaseResult{}, Note: "巡检使用真实演出与评测数据，关闭模型，只验证条件解析、预算折算、座位续接和空结果兜底。"}

	for _, test := range evalCases() {
		result := probe.runProbe(ctx, test.input)
		checks := test.verify(result)
		entry := evalCaseResult{Name: test.name, Question: test.input.Message, Pass: true, Checks: checks}
		for _, check := range checks {
			if !check.OK {
				entry.Pass = false
			}
		}
		if entry.Pass {
			report.Passed++
		}
		report.Total++
		report.Cases = append(report.Cases, entry)
	}
	for _, custom := range h.customCases.list() {
		result := probe.runProbe(ctx, probeInput{Message: custom.Message})
		checks := verifyCustomCase(result, custom.Expect)
		entry := evalCaseResult{Name: "自定义：" + custom.ID, Question: custom.Message, Pass: true, Checks: checks}
		for _, check := range checks {
			if !check.OK {
				entry.Pass = false
			}
		}
		if entry.Pass {
			report.Passed++
		}
		report.Total++
		report.Cases = append(report.Cases, entry)
	}
	return report
}

// verifyCustomCase checks the expectations an operator wrote, one "key=value"
// per entry, against the probe result. Unknown keys fail loudly instead of
// silently passing.
func verifyCustomCase(result probeResult, expect []string) []evalCheck {
	checks := []evalCheck{}
	if len(expect) == 0 {
		return append(checks, check("至少写一条期望（key=value）", false, "没有期望无法判断"))
	}
	truth := func(key string) (string, bool) {
		switch key {
		case "party_size":
			return strconv.Itoa(result.Plan.PartySize), true
		case "budget":
			return strconv.FormatFloat(result.Plan.Budget, 'f', -1, 64), true
		case "budget_mode":
			return result.Plan.BudgetMode, true
		case "city":
			return result.Plan.Filters.City, true
		case "category":
			return result.Plan.Filters.Category, true
		case "date_from":
			return result.Plan.Filters.DateFrom, true
		case "date_to":
			return result.Plan.Filters.DateTo, true
		case "seat_preference":
			return result.Plan.SeatPreference, true
		case "available_only":
			return strconv.FormatBool(result.Plan.Filters.AvailableOnly), true
		case "lifecycle":
			return result.Plan.Filters.Lifecycle, true
		case "sort":
			return result.Plan.Filters.Sort, true
		case "intent":
			return result.Intent, true
		case "min_items":
			return strconv.Itoa(len(result.Items)), true
		case "min_alternatives":
			return strconv.Itoa(len(result.Alternatives)), true
		}
		return "", false
	}
	for _, pair := range expect {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			checks = append(checks, check("期望格式 key=value", false, pair))
			continue
		}
		got, ok := truth(key)
		if !ok {
			checks = append(checks, check("期望键 "+key+" 不存在", false, "可用键见文档"))
			continue
		}
		checks = append(checks, check(key+" = "+value, got == value, got))
	}
	return checks
}

type evalCase struct {
	name   string
	input  probeInput
	verify func(probeResult) []evalCheck
}

func check(label string, ok bool, got string) evalCheck {
	if strings.TrimSpace(got) == "" {
		got = "（无）"
	}
	return evalCheck{Label: label, OK: ok, Got: got}
}

func evalCases() []evalCase {
	previous := shoppingPlan{PartySize: 2, Budget: 600, BudgetMode: "total", SeatPreference: "front", Filters: discoveryFilters{Lifecycle: "active", Sort: "reputation", Budget: 300}}
	return []evalCase{
		{
			name:  "人数与总预算折算",
			input: probeInput{Message: "周六，两个人，总预算 600 元，推荐能买票的演出"},
			verify: func(result probeResult) []evalCheck {
				checks := []evalCheck{
					check("人数解析为 2", result.Plan.PartySize == 2, fmt.Sprintf("%d 人", result.Plan.PartySize)),
					check("总预算 600 元", result.Plan.Budget == 600 && result.Plan.BudgetMode == "total", fmt.Sprintf("%.2f / %s", result.Plan.Budget, result.Plan.BudgetMode)),
					check("单张预算折算为 300 元", result.Filters.Budget == 300, fmt.Sprintf("%.2f", result.Filters.Budget)),
					check("周六解析为具体日期", result.Filters.DateFrom != "" && result.Filters.DateFrom == result.Filters.DateTo, "解析为 "+result.Filters.DateFrom),
					check("只推荐未结束且有票场次", result.Filters.Lifecycle == "active" && result.Filters.AvailableOnly, "状态 "+result.Filters.Lifecycle),
				}
				if len(result.Items) > 0 {
					within := true
					for _, item := range result.Items {
						for _, tier := range item.Tickets {
							if tier.Price*float64(result.Plan.PartySize) > 600 {
								within = false
							}
						}
					}
					checks = append(checks, check("2 张合计不超过 600 元", within, fmt.Sprintf("%d 场匹配", len(result.Items))))
				} else {
					labelled := false
					if len(result.Alternatives) > 0 {
						labelled = strings.Contains(strings.Join(result.Alternatives[0].Reasons, " "), "不在你要求的日期内")
					}
					checks = append(checks, check("无结果时给出带标注的备选", labelled, fmt.Sprintf("%d 条备选", len(result.Alternatives))))
				}
				return checks
			},
		},
		{
			name:  "城市与类型过滤",
			input: probeInput{Message: "上海话剧，500 元以内，能买票的"},
			verify: func(result probeResult) []evalCheck {
				checks := []evalCheck{
					check("城市解析为上海", result.Filters.City == "上海", "识别为 "+result.Filters.City),
					check("类型解析为话剧", result.Filters.Category == "话剧", "识别为 "+result.Filters.Category),
					check("预算 500 元", result.Filters.Budget == 500, fmt.Sprintf("%.2f", result.Filters.Budget)),
					check("只推荐可购场次", result.Filters.AvailableOnly && result.Filters.Lifecycle == "active", "状态 "+result.Filters.Lifecycle),
				}
				within := true
				for _, item := range result.Items {
					if !strings.Contains(item.Show.Area+item.Show.Address, "上海") || item.Show.TypeName != "话剧" {
						within = false
					}
					for _, tier := range item.Tickets {
						if tier.Price > 500 {
							within = false
						}
					}
				}
				checks = append(checks, check("结果都符合城市、类型与预算", within, fmt.Sprintf("%d 场匹配", len(result.Items))))
				if len(result.Items) == 0 {
					checks = append(checks, check("无结果时说明限制", strings.Contains(result.Answer, "没有找到可购演出"), "已说明"))
				}
				return checks
			},
		},
		{
			name:  "人均预算与三人",
			input: probeInput{Message: "人均 300，三个人"},
			verify: func(result probeResult) []evalCheck {
				return []evalCheck{
					check("人数解析为 3", result.Plan.PartySize == 3, fmt.Sprintf("%d 人", result.Plan.PartySize)),
					check("预算识别为人均", result.Plan.BudgetMode == "per_person" && result.Plan.Budget == 300, fmt.Sprintf("%.2f / %s", result.Plan.Budget, result.Plan.BudgetMode)),
					check("单张预算不折算", result.Filters.Budget == 300, fmt.Sprintf("%.2f", result.Filters.Budget)),
				}
			},
		},
		{
			name:  "座位偏好续接",
			input: probeInput{Message: "想坐近一点，预算改成 800", Plan: &previous},
			verify: func(result probeResult) []evalCheck {
				checks := []evalCheck{
					check("记住前排偏好", result.Plan.SeatPreference == "front", "偏好 "+result.Plan.SeatPreference),
					check("人数沿用上一轮", result.Plan.PartySize == 2, fmt.Sprintf("%d 人", result.Plan.PartySize)),
					check("预算按新说法更新", result.Plan.Budget == 800, fmt.Sprintf("%.2f", result.Plan.Budget)),
				}
				linked := len(result.Items) > 0
				for _, item := range result.Items {
					for _, tier := range item.Tickets {
						if !strings.Contains(tier.CheckoutURL, "seatPreference=front") || !strings.Contains(tier.CheckoutURL, "count=2") {
							linked = false
						}
						if !tier.HasSeats {
							linked = false
						}
					}
				}
				checks = append(checks, check("确认链接带入人数与座位偏好", linked, fmt.Sprintf("%d 场匹配", len(result.Items))))
				return checks
			},
		},
		{
			name:  "预算改为不限",
			input: probeInput{Message: "预算不限", Plan: &previous},
			verify: func(result probeResult) []evalCheck {
				return []evalCheck{
					check("预算清空", result.Plan.Budget == 0 && result.Filters.Budget == 0, fmt.Sprintf("%.2f", result.Plan.Budget)),
					check("人数与前排偏好保留", result.Plan.PartySize == 2 && result.Plan.SeatPreference == "front", fmt.Sprintf("%d 人 / %s", result.Plan.PartySize, result.Plan.SeatPreference)),
				}
			},
		},
		{
			name:  "口碑意图与样本排序",
			input: probeInput{Message: "有哪些口碑好的演出？"},
			verify: func(result probeResult) []evalCheck {
				checks := []evalCheck{
					check("识别为口碑意图", result.Intent == "reviews", "意图 "+result.Intent),
					check("按口碑排序", result.Filters.Sort == "reputation", "排序 "+result.Filters.Sort),
				}
				ordered := true
				for i := 1; i < len(result.Items); i++ {
					if sampleTier(result.Items[i-1].Show.Rating) > sampleTier(result.Items[i].Show.Rating) {
						ordered = false
					}
				}
				checks = append(checks, check("评分样本少的排在后面", ordered, fmt.Sprintf("%d 场", len(result.Items))))
				return checks
			},
		},
		{
			name:  "购票推荐不出现历史场次",
			input: probeInput{Message: "推荐能买票的演出，两个人，预算 600"},
			verify: func(result probeResult) []evalCheck {
				ended := []string{}
				for _, item := range result.Items {
					if item.Show.Lifecycle == "ended" {
						ended = append(ended, item.Show.Name)
					}
				}
				return []evalCheck{
					check("推荐里没有已结束场次", len(ended) == 0, fmt.Sprintf("已结束 %d 场 %s", len(ended), strings.Join(ended, "、"))),
					check("历史票档不作为可购票", len(result.Items) == 0 || len(ended) == 0, fmt.Sprintf("%d 场匹配", len(result.Items))),
				}
			},
		},
	}
}

func (h *Handler) runProbe(ctx context.Context, input probeInput) probeResult {
	body, _ := json.Marshal(map[string]any{"message": input.Message, "plan": input.Plan, "budget": input.Budget, "history": []any{}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/assist", strings.NewReader(string(body))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.assist(recorder, request)

	var parsed struct {
		Answer       string            `json:"answer"`
		Mode         string            `json:"mode"`
		Intent       string            `json:"intent"`
		Plan         shoppingPlan      `json:"plan"`
		Filters      discoveryFilters  `json:"filters"`
		Items        []comparisonEntry `json:"items"`
		Alternatives []comparisonEntry `json:"alternatives"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		return probeResult{Answer: "巡检响应无法解析：" + err.Error()}
	}
	return probeResult{Answer: parsed.Answer, Mode: parsed.Mode, Intent: parsed.Intent, Plan: parsed.Plan, Filters: parsed.Filters, Items: parsed.Items, Alternatives: parsed.Alternatives}
}
