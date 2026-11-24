package guide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/agent"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

// 本文件是导购与 Agent 之间的接缝：
// 解析请求 → 转成 Agent 的 Slots → 跑 Agent → 把结构化结果还原成票务卡片。

// AgentStreamHandler 暴露 SSE 版 Agent。main 会把它挂在 TimeoutHandler 之外，
// 否则被包装的 writer 无法逐块 Flush，用户就看不到步骤实时出现。
func (h *Handler) AgentStreamHandler() http.Handler {
	return http.HandlerFunc(h.agentStream)
}

// agentEmitter 把 Agent 事件翻译成 SSE 载荷。Run 与 Resume 共用，
// 续跑时前端看到的事件序列与首轮一致（step/delta/phase/action）。
func agentEmitter(send func(map[string]any) bool) func(agent.Event) error {
	return func(event agent.Event) error {
		switch event.Type {
		case "step":
			if event.Step == nil {
				return nil
			}
			if !send(map[string]any{"type": "step", "step": event.Step}) {
				return context.Canceled
			}
		case "chunk":
			if !send(map[string]any{"type": "delta", "source": "agent", "text": event.Text}) {
				return context.Canceled
			}
		case "phase":
			if !send(map[string]any{"type": "phase", "phase": event.Phase}) {
				return context.Canceled
			}
		case "action":
			if event.Action != nil && !send(map[string]any{"type": "action", "action": event.Action}) {
				return context.Canceled
			}
		}
		return nil
	}
}

// agentStream 是「看得见思考过程」的咨询入口：每一步工具调用都会即时推送。
func (h *Handler) agentStream(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeAssist(w, r)
	if !ok {
		return
	}
	if h.agent == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "Agent 未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	request, err := h.agentRequest(ctx, input)
	if err != nil {
		h.unavailable(w, err)
		return
	}

	// 拿不到 Flush 能力时退回普通 JSON 接口，保证老客户端也能用。
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		h.agentJSON(w, r, input)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	send := sseSender(w, controller)

	if !send(map[string]any{"type": "agent_start", "session_id": request.SessionID, "mode": h.agentMode()}) {
		return
	}

	result, runErr := h.agent.Run(ctx, request, agentEmitter(send))
	if runErr != nil {
		send(map[string]any{"type": "error", "message": "Agent 运行中断：" + runErr.Error()})
		return
	}
	h.sendAgentResult(ctx, send, input, result, request.SessionID, false)
}

// sseSender 封装「一行 data + Flush」；写失败返回 false，调用方据此停止推送。
func sseSender(w http.ResponseWriter, controller *http.ResponseController) func(map[string]any) bool {
	return func(payload map[string]any) bool {
		payload["ts"] = time.Now().UnixMilli()
		data, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return false
		}
		if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", data); writeErr != nil {
			return false
		}
		return controller.Flush() == nil
	}
}

// sendAgentResult 收尾：推 meta/final，记统计。resumed 表示这是续跑结果，
// 前端用它把答案接在原来的对话后面。
func (h *Handler) sendAgentResult(ctx context.Context, send func(map[string]any) bool, input assistRequest, result agent.Result, sessionID string, resumed bool) {
	entries := h.entriesFor(ctx, result.ShowIDs)
	plan := planFromSlots(result.Slots)

	send(map[string]any{
		"type": "meta", "mode": result.Mode, "intent": result.Intent,
		"items": entries, "alternatives": []comparisonEntry{},
		"plan": plan, "session_id": sessionID, "updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	final := map[string]any{
		"type": "final", "answer": result.Answer, "mode": result.Mode, "intent": result.Intent,
		"note": result.Note, "run_id": result.RunID, "session_id": sessionID,
		"items": entries, "plan": plan, "steps": len(result.Steps), "elapsed_ms": result.Elapsed,
		"actions":   result.Actions,
		"resumable": result.Resumable, "resume_reason": result.ResumeReason, "resume_stage": result.ResumeStage,
	}
	if resumed {
		final["resumed_from"] = result.ResumedFrom
	}
	send(final)
	if h.stats != nil {
		h.stats.record(askRecord{
			At: time.Now().UTC().Format(time.RFC3339), Question: input.Message,
			Mode: result.Mode, Intent: result.Intent, LatencyMS: result.Elapsed,
			Items: len(entries), Alternatives: 0,
		})
	}
}

// agentJSON 是非流式版本，也是 SSE 不可用时的兜底。
func (h *Handler) agentJSON(w http.ResponseWriter, r *http.Request, input assistRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	request, err := h.agentRequest(ctx, input)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	result, runErr := h.agent.Run(ctx, request, nil)
	if runErr != nil {
		h.write(w, http.StatusBadGateway, map[string]string{"message": "Agent 运行中断，请稍后重试"})
		return
	}
	entries := h.entriesFor(ctx, result.ShowIDs)
	h.write(w, 200, map[string]any{
		"answer": result.Answer, "mode": result.Mode, "intent": result.Intent,
		"note": result.Note, "run_id": result.RunID, "session_id": request.SessionID,
		"items": entries, "alternatives": []comparisonEntry{},
		"plan": planFromSlots(result.Slots), "steps": result.Steps, "actions": result.Actions,
		"elapsed_ms": result.Elapsed, "updated_at": time.Now().UTC().Format(time.RFC3339),
		"resumable": result.Resumable, "resume_reason": result.ResumeReason, "resume_stage": result.ResumeStage,
	})
}

// agentRequest 把 HTTP 请求翻译成 Agent 的输入。
//
// 这里刻意复用 resolvePlan：人数、预算、日期、城市、类型的正则解析已经在
// 导购里被验证过一遍，Agent 不需要再写一套「理解数字」的逻辑。
func (h *Handler) agentRequest(ctx context.Context, input assistRequest) (agent.Request, error) {
	if h.agent == nil {
		return agent.Request{}, fmt.Errorf("agent disabled")
	}
	// 取一次快照（命中 30 秒缓存）：只为让 resolvePlan 能识别城市和类型词表。
	snapshot, err := h.snapshotForAgent(ctx)
	if err != nil {
		return agent.Request{}, err
	}
	plan := resolvePlan(input, snapshot, time.Now())

	// resolvePlan 在用户没提人数时会默认 1 人；这里区分「明确说了」和「默认值」，
	// 否则第二轮会把上一轮记住的「两个人」覆盖成 1 人。
	party := plan.PartySize
	if !peopleRE.MatchString(input.Message) {
		party = 0
	}
	slots := agent.Slots{
		Party:      party,
		Budget:     plan.Budget,
		BudgetMode: plan.BudgetMode,
		SeatPref:   plan.SeatPreference,
		City:       plan.Filters.City,
		Category:   plan.Filters.Category,
		DateFrom:   plan.Filters.DateFrom,
		DateTo:     plan.Filters.DateTo,
	}
	ids := make([]int64, 0, 4)
	for _, value := range append(append([]string{}, input.CompareIDs...), input.ShowID) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		id, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	slots.ShowIDs = ids

	intent := ""
	if len(ids) > 1 || strings.Contains(input.Message, "对比") || strings.Contains(input.Message, "比较") {
		intent = agent.IntentCompare
	}
	session := strings.TrimSpace(input.SessionID)
	if session == "" {
		session = "s-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	// 设备标识比会话活得久：偏好与写操作都挂在它下面。
	owner := strings.TrimSpace(input.ProfileID)
	if owner == "" {
		owner = session
	}
	// 长期偏好只补「用户没说」的字段，不能盖掉本轮明确条件。
	if h.agentState != nil {
		profile := h.agentState.Profile(owner)
		if slots.City == "" {
			slots.City = profile["city"]
		}
		if slots.Category == "" {
			slots.Category = profile["category"]
		}
		if slots.SeatPref == "" {
			slots.SeatPref = profile["seat"]
		}
		if slots.Budget == 0 && profile["budget"] != "" {
			if amount, parseErr := strconv.ParseFloat(profile["budget"], 64); parseErr == nil && amount > 0 {
				slots.Budget = amount
				slots.BudgetMode = "per_person"
			}
		}
	}
	showID := int64(0)
	if len(ids) == 1 {
		showID = ids[0]
	}
	return agent.Request{
		Query: input.Message, SessionID: session, OwnerID: owner, History: input.History,
		Intent: intent, Slots: slots, CompareIDs: ids, ShowID: showID,
	}, nil
}

// snapshotForAgent 取一次全量快照（命中 30 秒缓存），供需求解析识别城市与类型词表。
func (h *Handler) snapshotForAgent(ctx context.Context) (ticketing.CatalogSnapshot, error) {
	rich, ok := h.catalog.(richCatalog)
	if !ok {
		return ticketing.CatalogSnapshot{}, fmt.Errorf("当前数据源不支持 Agent 查询")
	}
	return rich.Catalog(ctx, false)
}

// entriesFor 把 Agent 选中的演出还原成前端认识的卡片结构（含票档、评测、注意事项）。
func (h *Handler) entriesFor(ctx context.Context, ids []int64) []comparisonEntry {
	entries := make([]comparisonEntry, 0, len(ids))
	rich, ok := h.catalog.(richCatalog)
	if !ok {
		return entries
	}
	snapshot, err := rich.Catalog(ctx, false)
	if err != nil {
		return entries
	}
	for _, id := range ids {
		if len(entries) >= 4 {
			break
		}
		for _, show := range snapshot.Items {
			if show.ID != id {
				continue
			}
			entries = append(entries, h.comparison(ctx, show, snapshot.Reviews[id], 0))
			break
		}
	}
	return entries
}

// planFromSlots 把 Agent 合并后的需求还原成前端的 shoppingPlan，供下一轮继承。
func planFromSlots(slots agent.Slots) shoppingPlan {
	plan := shoppingPlan{
		PartySize:      max(slots.Party, 1),
		Budget:         slots.Budget,
		BudgetMode:     slots.BudgetMode,
		SeatPreference: slots.SeatPref,
		Filters: discoveryFilters{
			City:     slots.City,
			Category: slots.Category,
			DateFrom: slots.DateFrom,
			DateTo:   slots.DateTo,
			Budget:   slots.PerTicketBudget(),
		},
	}
	if plan.BudgetMode == "" {
		plan.BudgetMode = "total"
	}
	return plan
}

func (h *Handler) agentMode() string {
	if h.model != nil && h.model.Enabled() {
		return "agent-react"
	}
	return "agent-rule"
}

// agentTraceFilePath 与提示词、统计放在同一目录，方便一并清理与备份。
func agentTraceFilePath() string {
	// 测试与多实例部署可用环境变量指定轨迹文件，避免写进程序目录。
	if custom := strings.TrimSpace(os.Getenv("GUIDE_AGENT_TRACE")); custom != "" {
		return custom
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "agent-runs.jsonl")
	}
	return "agent-runs.jsonl"
}

// ownerFrom 从请求里取设备标识；写操作与清单都以它为准。
func ownerFrom(r *http.Request) string {
	if value := strings.TrimSpace(r.URL.Query().Get("profile_id")); value != "" {
		return value
	}
	return strings.TrimSpace(r.URL.Query().Get("session_id"))
}

// agentActionConfirm 执行一条待确认的写操作。
func (h *Handler) agentActionConfirm(w http.ResponseWriter, r *http.Request) {
	h.decideAction(w, r, true)
}

// agentActionReject 取消一条待确认的写操作。
func (h *Handler) agentActionReject(w http.ResponseWriter, r *http.Request) {
	h.decideAction(w, r, false)
}

// decideAction 是审批的统一入口：幂等、区分「已处理」与「不存在」。
func (h *Handler) decideAction(w http.ResponseWriter, r *http.Request, approve bool) {
	if h.agentState == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "写操作未启用"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "缺少操作编号"})
		return
	}
	action, err := h.agentState.Decide(id, approve)
	switch {
	case errors.Is(err, errActionNotFound):
		h.write(w, http.StatusNotFound, map[string]string{"message": "没有找到这条操作，可能已过期"})
	case errors.Is(err, errAlreadyDecided):
		// 幂等：重复确认不报 500，返回当前状态让前端更新即可。
		h.write(w, http.StatusConflict, map[string]any{"message": "这条操作已经处理过了", "action": action})
	case err != nil:
		h.write(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
	default:
		h.write(w, http.StatusOK, map[string]any{"action": action, "watchlist": h.agentState.Watchlist(action.OwnerID)})
	}
}

// agentActions 返回当前设备的提案历史，用于刷新后恢复待确认卡片。
func (h *Handler) agentActions(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r)
	if h.agentState == nil || owner == "" {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h.write(w, http.StatusOK, map[string]any{"items": h.agentState.ListActions(owner, limit)})
}

// watchlist 返回已确认的观演清单（服务端持久化，跨会话有效）。
func (h *Handler) watchlist(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r)
	if h.agentState == nil || owner == "" {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	h.write(w, http.StatusOK, map[string]any{"items": h.agentState.Watchlist(owner)})
}

// adminAgentBench 用固定用例集回放当前 Agent，产出意图/工具/耗时指标。
// 改提示词、加工具、换模型之后跑一次，就能看出有没有退化。
func (h *Handler) adminAgentBench(w http.ResponseWriter, r *http.Request) {
	if h.agent == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "Agent 未启用"})
		return
	}
	cases, err := agent.LoadBenchCases(benchCasesPath())
	if err != nil {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "用例文件无法解析：" + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	report := agent.Benchmark(ctx, h.agent, cases, agent.Slots{})
	h.write(w, http.StatusOK, map[string]any{"report": report, "mode": h.agentMode(), "cases": len(cases)})
}

// benchCasesPath 允许用 GUIDE_AGENT_BENCH 指向自定义用例文件。
func benchCasesPath() string {
	if custom := strings.TrimSpace(os.Getenv("GUIDE_AGENT_BENCH")); custom != "" {
		return custom
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "agent-bench.json")
	}
	return "agent-bench.json"
}

// adminAgentActions 是管理端审计视图：所有设备的提案与决策结果。
func (h *Handler) adminAgentActions(w http.ResponseWriter, r *http.Request) {
	if h.agentState == nil {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h.write(w, http.StatusOK, map[string]any{"items": h.agentState.AllActions(limit)})
}

// adminAgentRuns 返回最近的 Agent 运行摘要，用于管理端「Agent 轨迹」列表。
func (h *Handler) adminAgentRuns(w http.ResponseWriter, r *http.Request) {
	if h.agentTraces == nil {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h.write(w, http.StatusOK, map[string]any{"items": h.agentTraces.List(limit), "mode": h.agentMode()})
}

// adminAgentRun 返回单次运行的完整步骤，用于排查「它为什么这么答」。
func (h *Handler) adminAgentRun(w http.ResponseWriter, r *http.Request) {
	if h.agentTraces == nil {
		h.write(w, http.StatusNotFound, map[string]string{"message": "没有找到这次运行"})
		return
	}
	run, ok := h.agentTraces.Get(strings.TrimSpace(r.PathValue("id")))
	if !ok {
		h.write(w, http.StatusNotFound, map[string]string{"message": "没有找到这次运行"})
		return
	}
	h.write(w, http.StatusOK, run)
}

// ---------- 断点续跑 ----------

// AgentResumeStreamHandler 暴露 SSE 版续跑入口，与首轮咨询一样挂在超时包装之外。
func (h *Handler) AgentResumeStreamHandler() http.Handler {
	return http.HandlerFunc(h.agentResumeStream)
}

// agentResume 从快照继续一次没跑完的推理：已查到的事实不重查，只补没走完的部分。
func (h *Handler) agentResume(w http.ResponseWriter, r *http.Request) {
	result, runID, sessionID, ok := h.runResume(w, r, nil)
	if !ok {
		return
	}
	entries := h.entriesFor(r.Context(), result.ShowIDs)
	h.write(w, 200, map[string]any{
		"answer": result.Answer, "mode": result.Mode, "intent": result.Intent,
		"note": result.Note, "run_id": result.RunID, "session_id": sessionID,
		"items": entries, "alternatives": []comparisonEntry{},
		"plan": planFromSlots(result.Slots), "steps": result.Steps, "actions": result.Actions,
		"elapsed_ms": result.Elapsed, "updated_at": time.Now().UTC().Format(time.RFC3339),
		"resumed_from": runID,
	})
}

// agentResumeStream 是续跑的流式版本：前端能看到「从断点继续」之后的每一步。
func (h *Handler) agentResumeStream(w http.ResponseWriter, r *http.Request) {
	if h.agent == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "Agent 未启用"})
		return
	}
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		runID = strings.TrimSpace(r.URL.Query().Get("run_id"))
	}
	if runID == "" {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "缺少 run_id"})
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		h.agentResume(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	send := sseSender(w, controller)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	send(map[string]any{"type": "agent_start", "resume_of": runID, "mode": h.agentMode()})
	result, runID, sessionID, ok := h.runResume(w, r, agentEmitter(send))
	if !ok {
		return
	}
	// 统计沿用快照里的原始问题；快照可能已随完成被清理，取不到就退回 run_id。
	question := runID
	if checkpoint, found := h.agentCheckpoints.Load(runID); found {
		question = checkpoint.Query
	}
	h.sendAgentResult(ctx, send, assistRequest{Message: question}, result, sessionID, true)
}

// runResume 是两种续跑入口的公共部分：校验、执行、统一错误码。
// 错误一律 404/503 而不是 500：快照过期是正常业务状态，不是服务故障。
func (h *Handler) runResume(w http.ResponseWriter, r *http.Request, emit func(agent.Event) error) (agent.Result, string, string, bool) {
	if h.agent == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "Agent 未启用"})
		return agent.Result{}, "", "", false
	}
	if h.agentCheckpoints == nil {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前部署没有开启断点续跑"})
		return agent.Result{}, "", "", false
	}
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		runID = strings.TrimSpace(r.URL.Query().Get("run_id"))
	}
	if runID == "" {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "缺少 run_id"})
		return agent.Result{}, "", "", false
	}
	// Resume 成功后会清掉快照，先取出会话信息给前端对齐消息归属。
	sessionID := runID
	if checkpoint, found := h.agentCheckpoints.Load(runID); found {
		sessionID = checkpoint.SessionID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	result, err := h.agent.Resume(ctx, runID, emit)
	if err != nil {
		h.write(w, http.StatusNotFound, map[string]string{"message": err.Error()})
		return agent.Result{}, "", "", false
	}
	return result, runID, sessionID, true
}

// agentCheckpointsList 返回当前设备还能继续的运行，刷新页面后仍可恢复。
func (h *Handler) agentCheckpointsList(w http.ResponseWriter, r *http.Request) {
	if h.agentCheckpoints == nil {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	owner := ownerFrom(r)
	items := make([]map[string]any, 0, 8)
	for _, checkpoint := range h.agentCheckpoints.List(20) {
		// 快照按设备隔离：别人中断的运行不该出现在你的「继续」列表里。
		if owner != "" && checkpoint.OwnerID != owner {
			continue
		}
		items = append(items, map[string]any{
			"run_id": checkpoint.RunID, "query": checkpoint.Query,
			"stage": checkpoint.Stage, "reason": checkpoint.Reason,
			"round": checkpoint.Round, "tools": len(checkpoint.Evidence.Tools),
			"steps": len(checkpoint.Steps), "updated_at": checkpoint.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	h.write(w, http.StatusOK, map[string]any{"items": items})
}

// agentCheckpointsFilePath 快照落盘位置；重启后未完成的运行仍然可以继续。
func agentCheckpointsFilePath() string {
	if custom := strings.TrimSpace(os.Getenv("GUIDE_AGENT_CHECKPOINTS")); custom != "" {
		return custom
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "agent-checkpoints.jsonl")
	}
	return "agent-checkpoints.jsonl"
}

// adminAgentCheckpoints 是管理端审计视图：所有设备的可恢复快照。
func (h *Handler) adminAgentCheckpoints(w http.ResponseWriter, r *http.Request) {
	if h.agentCheckpoints == nil {
		h.write(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h.write(w, http.StatusOK, map[string]any{"items": h.agentCheckpoints.List(limit)})
}

// adminAgentRagEval 在固定标注集上对比三种检索策略的召回质量。
// 改常识库、换 embedding 模型之后跑一次，退化会直接体现在 recall/MRR 上。
func (h *Handler) adminAgentRagEval(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	k, _ := strconv.Atoi(r.URL.Query().Get("k"))
	report := agent.EvaluateRAG(ctx, h.kb, agent.DefaultRagCases(), k)
	h.write(w, http.StatusOK, map[string]any{
		"report":  report,
		"entries": h.kb.Size(),
	})
}
