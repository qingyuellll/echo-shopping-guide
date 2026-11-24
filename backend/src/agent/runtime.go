package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// 运行参数。步数与超时是成本与体验的平衡点：步数太多模型容易绕圈，
// 太少则「检索 → 详情 → 评测」这种正常链路都走不完。
const (
	defaultMaxSteps  = 4
	perStepTimeout   = 20 * time.Second
	totalBudget      = 75 * time.Second
	observationLimit = 1400 // 单条观察最多保留的字符数，防止上下文膨胀
	maxParseFailures = 2
	maxParallelTools = 3    // 一轮最多并行 3 个工具
	contextBudget    = 6000 // 决策上下文字数上限，超过就压缩
	digestSteps      = 12   // 压缩时保留的最近步骤数
)

// Runtime 是 Agent 的执行器：规划 + 工具编排 + 答案组织 + 自检 + 轨迹记录。
type Runtime struct {
	kit         Toolkit
	model       Model
	memory      *MemoryStore
	traces      *TraceStore
	sink        ActionSink      // 写操作审批队列；为 nil 时写工具不可用
	kb          *KnowledgeIndex // 常识库检索（向量 + 重排）；nil 时退回关键词
	checkpoints *CheckpointStore
	budget      time.Duration // 单次运行总预算，测试可调小以验证中断
	maxStep     int
}

// Option 用函数式选项扩展 Runtime，保持 New 的参数稳定。
type Option func(*Runtime)

// WithActionSink 注入写操作审批队列。不注入时 Agent 只能读。
func WithActionSink(sink ActionSink) Option {
	return func(r *Runtime) { r.sink = sink }
}

// WithCheckpoints 注入断点快照仓库。不注入时运行不可恢复，但其它功能不受影响。
func WithCheckpoints(store *CheckpointStore) Option {
	return func(r *Runtime) { r.checkpoints = store }
}

// WithKnowledge 注入常识检索索引（向量召回 + 重排）。
func WithKnowledge(index *KnowledgeIndex) Option {
	return func(r *Runtime) { r.kb = index }
}

// WithTotalBudget 覆盖单次运行总预算。测试用它把超时压到几十毫秒，
// 以便真实触发「中断 → 续跑」这条路径。
func WithTotalBudget(budget time.Duration) Option {
	return func(r *Runtime) {
		if budget > 0 {
			r.budget = budget
		}
	}
}

// WithMaxSteps 覆盖最大推理步数（测试用 1 就能验证截断逻辑）。
func WithMaxSteps(steps int) Option {
	return func(r *Runtime) {
		if steps > 0 {
			r.maxStep = steps
		}
	}
}

// New 构建 Agent 运行时。model 可以为 nil（等于禁用模型），此时走规则执行器。
func New(kit Toolkit, model Model, memory *MemoryStore, traces *TraceStore, options ...Option) *Runtime {
	runtime := &Runtime{kit: kit, model: model, memory: memory, traces: traces, budget: totalBudget, maxStep: defaultMaxSteps}
	if memory == nil {
		runtime.memory = NewMemoryStore(0, 0)
	}
	for _, option := range options {
		option(runtime)
	}
	return runtime
}

// session 是一次运行的上下文：统一负责「记录一步 + 实时推送」，
// 这样步骤序号天然连续，前端时间线不会跳号。
type session struct {
	mu      sync.Mutex
	result  *Result
	emit    func(Event) error
	names   map[int64]string // 工具观察里出现过的演出名，写操作提案用它补全标题
	ownerID string           // 写操作的归属（设备级），比会话活得久

	// 以下字段只为「断点续跑」而存在：重建一次中断的运行需要知道
	// 它属于谁、在问什么、走到第几轮、以及为什么停下。
	runID     string
	sessionID string
	query     string
	history   []llm.Message
	plan      Plan
	slots     Slots
	store     *CheckpointStore
	last      snapshotState
}

// snapshotState 是最近一次「中间态」的描述，供 checkpoint 读取。
type snapshotState struct {
	engine   string
	stage    string
	reason   string // timeout / canceled / model_error
	round    int
	failures int
	messages []llm.Message
}

func (s *session) step(step Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	step.Index = len(s.result.Steps) + 1
	step.At = now()
	s.result.Steps = append(s.result.Steps, step)
	_ = emitEvent(s.emit, Event{Type: "step", Step: &step})
}

func (s *session) chunk(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return emitEvent(s.emit, Event{Type: "chunk", Text: text})
}

// snapshot 记录一份可恢复的中间态。没注入 CheckpointStore 时是空操作，
// 所以这条逻辑不会影响正常路径的性能与行为。
func (s *session) snapshot(update snapshotState, found evidence) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if update.engine != "" {
		s.last.engine = update.engine
	}
	if update.stage != "" {
		s.last.stage = update.stage
	}
	if update.reason != "" {
		s.last.reason = update.reason
	}
	if update.messages != nil {
		s.last.messages = update.messages
	}
	s.last.round = update.round
	s.last.failures = update.failures

	last := s.last
	mode := "agent-rule"
	if last.engine == "react" {
		mode = "agent-react"
	}
	checkpoint := Checkpoint{
		RunID: s.runID, SessionID: s.sessionID, OwnerID: s.ownerID, Query: s.query,
		Engine: last.engine, Stage: last.stage, Status: "resumable",
		Plan: s.plan, Slots: s.slots, Mode: mode,
		Round: last.round, Failures: last.failures, Messages: last.messages,
		Steps:    append([]Step{}, s.result.Steps...),
		Evidence: found.snapshot(),
		Actions:  append([]PendingAction{}, s.result.Actions...),
		Reason:   last.reason, History: s.history,
	}
	store := s.store
	s.mu.Unlock()
	store.Save(checkpoint)
}

// interruption 返回中断原因；空串表示这条链路正常走完了。
func (s *session) interruption() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last.reason
}

// lastRound 读取最近一次记录的轮次，供「只补成文」的快照沿用。
func (s *session) lastRound() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last.round
}

// Run 执行一次完整咨询。emit 用于实时推送步骤与文字（可为 nil）。
func (r *Runtime) Run(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()

	result := Result{RunID: newID(), Steps: []Step{}}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return result, fmt.Errorf("问题不能为空")
	}
	owner := strings.TrimSpace(req.OwnerID)
	if owner == "" {
		owner = req.SessionID
	}
	state := &session{
		result: &result, emit: emit, names: map[int64]string{}, ownerID: owner,
		runID: result.RunID, sessionID: req.SessionID, query: query,
		history: req.History, store: r.checkpoints,
	}

	// ① 召回记忆并合并本轮需求：本轮明确的字段优先，没提的沿用上一轮。
	previous, remembered := r.memory.Load(req.SessionID)
	slots := mergeSlots(previous.Slots, req.Slots)
	result.Slots = slots
	memoryNote := ""
	if remembered && previous.Turns > 0 {
		memoryNote = "（沿用上轮条件）"
	}

	// ② 规划：规则给底线，模型可细化；两者都产出可展示的 Plan。
	plan := r.plan(ctx, req, slots, previous.Intent, state)
	result.Plan = plan
	result.Intent = plan.Intent
	state.mu.Lock()
	state.plan, state.slots = plan, slots
	state.mu.Unlock()

	// ③ 执行：有模型走 ReAct，无模型走规则工具链，两条路径共用同一套工具。
	found := r.act(ctx, req, query, slots, plan, state)
	interrupted := state.interruption()

	// ④ 事实已齐，先记一份 stage=answer 的快照：成文阶段断流可以只补这一步。
	// 已经中断的运行不覆盖，否则会把「停在第二轮」改写成「停在成文」。
	if interrupted == "" {
		state.snapshot(snapshotState{stage: "answer", round: state.lastRound()}, found)
	}
	answer, note := r.finalize(ctx, req, query, slots, plan, found, memoryNote, state)

	result.Answer = answer
	result.Note = note
	result.ShowIDs = found.showIDs
	result.Elapsed = time.Since(started).Milliseconds()

	// ⑤ 中断处理：没跑完的运行保留快照，前端可按 run_id 继续。
	switch {
	case interrupted == "timeout" || interrupted == "canceled":
		result.Resumable = true
		result.ResumeReason = interrupted
		result.ResumeStage = "act"
		result.Note = joinNote(result.Note, interruptionNote(interrupted))
	case ctx.Err() != nil:
		// 工具都查到了，但预算在成文阶段用完：续跑只需要重新成文。
		result.Resumable = true
		result.ResumeReason = interruptionReason(ctx)
		result.ResumeStage = "answer"
		state.snapshot(snapshotState{stage: "answer", reason: result.ResumeReason}, found)
	}

	// ⑥ 落记忆与轨迹：记忆只存结构化需求，轨迹供管理端回看。
	r.memory.Save(req.SessionID, slots, plan.Intent)
	if r.traces != nil {
		r.traces.Add(Run{
			RunID: result.RunID, SessionID: req.SessionID, Query: query,
			Intent: plan.Intent, Mode: result.Mode, StartedAt: now(),
			ElapsedMS: result.Elapsed, Steps: result.Steps, Tools: found.tools,
			ShowIDs: found.showIDs, Slots: slots, Plan: plan,
			Answer: answer, Note: note, Resumable: result.Resumable,
		})
	}
	// 跑完的运行没有续跑价值，清掉快照避免堆积。
	if !result.Resumable {
		r.checkpoints.Delete(result.RunID)
	}
	return result, nil
}

// finalize 生成答案并自检：先算数据版（自检锚点），再决定是否让模型改写。
// Run 与 Resume 共用这一步，避免「续跑出来的答案少了一道自检」。
func (r *Runtime) finalize(ctx context.Context, req Request, query string, slots Slots, plan Plan, found evidence, memoryNote string, state *session) (string, string) {
	dataAnswer := buildAnswer(plan, slots, found, memoryNote)
	answer := dataAnswer
	note := ""

	if r.model != nil && r.model.Enabled() {
		_ = emitEvent(state.emit, Event{Type: "phase", Phase: "answer"})
		streamed, err := r.streamAnswer(ctx, req, query, slots, plan, found, dataAnswer, state)
		switch {
		case strings.TrimSpace(streamed) != "":
			// 即使中途断流也保留已经吐出的内容，不再重推整段答案。
			answer = strings.TrimSpace(streamed)
			state.result.Mode = "agent-react"
			if err != nil {
				note = "回答生成中断，以上是已生成的部分，可直接参考数据版结论。"
			}
		case err != nil:
			note = "模型暂时不可用，下面是按查到的数据整理的回答。"
		}
	}
	if state.result.Mode == "" {
		state.result.Mode = "agent-rule"
		for _, piece := range chunkText(answer, 80) {
			if err := state.chunk(piece); err != nil {
				break // 前端已断开，答案仍会落轨迹
			}
		}
	}

	// ⑤ 自检：不能被模型带出「已下单/已支付」这类越权表述，也不能空回答。
	return r.selfCheck(answer, dataAnswer, found, note, state)
}

// plan 产出本轮的执行计划。规则版永远可用；模型只负责「理解得更细」。
func (r *Runtime) plan(ctx context.Context, req Request, slots Slots, previousIntent string, state *session) Plan {
	started := time.Now()
	intent := strings.TrimSpace(req.Intent)
	fromRule := true
	if intent == "" {
		intent = inferIntent(req.Query)
		// 「那有没有便宜点的」这类补充句自己看不出意图，继承上一轮更接近人的理解。
		if intent == IntentExplore && previousIntent != "" {
			intent = previousIntent
		}
	}
	plan := Plan{Intent: intent, Tools: toolsFor(intent), FromRule: fromRule, Reason: "按关键词与前端筛选预判"}

	if r.model != nil && r.model.Enabled() {
		messages := []llm.Message{
			{Role: "system", Content: plannerPrompt},
			{Role: "user", Content: fmt.Sprintf("当前条件：%s\n用户问题：%s\n只输出 JSON：{\"intent\":\"recommend|compare|reviews|venue|policy|explore\",\"focus\":\"关注点\",\"reason\":\"一句话依据\"}", describeSlots(slots), req.Query)},
		}
		modelCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		raw, err := r.model.Complete(modelCtx, r.model.SmallModel(), messages, 0.1)
		cancel()
		if err == nil {
			var parsed struct {
				Intent string `json:"intent"`
				Focus  string `json:"focus"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(extractJSONObject(raw)), &parsed) == nil {
				if normalized := normalizeIntent(parsed.Intent); normalized != "" {
					plan.Intent = normalized
					plan.Tools = toolsFor(normalized)
					plan.FromRule = false
				}
				plan.Focus = strings.TrimSpace(parsed.Focus)
				if reason := strings.TrimSpace(parsed.Reason); reason != "" {
					plan.Reason = reason
				}
			}
		}
	}
	state.step(planStep(plan, started, len(plan.Tools)))
	return plan
}

// evidence 汇总本轮工具产出，是答案、自检与轨迹的共同依据。
type evidence struct {
	results   []toolResult
	shows     []Show
	details   []Detail
	tips      []KnowledgeHit
	actions   []PendingAction
	budget    *BudgetPlan
	tools     []string
	showIDs   []int64
	available bool
}

// act 选择执行器并把工具结果收拢成 evidence。
func (r *Runtime) act(ctx context.Context, req Request, query string, slots Slots, plan Plan, state *session) evidence {
	if r.model != nil && r.model.Enabled() {
		found := r.react(ctx, req, query, slots, plan, state)
		if found.available {
			return found
		}
		// ReAct 失败或一步没走通时，用规则链兜底，保证用户总能拿到数据。
		state.step(Step{Phase: "observe", Title: "转向规则执行", Detail: "模型没有给出可用的工具调用，改用固定链路。", OK: true})
		return r.ruleChain(ctx, query, slots, plan, state)
	}
	return r.ruleChain(ctx, query, slots, plan, state)
}

// ruleChain 是无模型时的确定性执行器：按意图固定工具顺序，离线可演示、可回归测试。
func (r *Runtime) ruleChain(ctx context.Context, query string, slots Slots, plan Plan, state *session) evidence {
	result := evidence{available: true}
	search := func() []Show {
		res := r.call(ctx, ToolSearchShows, nil, slots, query, state)
		result.absorb(res)
		return res.Shows
	}

	switch plan.Intent {
	case IntentCompare:
		ids := slots.ShowIDs
		if len(ids) < 2 {
			ids = idsOf(search(), 2)
		}
		if len(ids) >= 2 {
			result.absorb(r.call(ctx, ToolCompareShows, map[string]any{"show_ids": ids}, slots, query, state))
		}
	case IntentReviews:
		shows := search()
		for _, id := range idsOf(shows, 2) {
			result.absorb(r.call(ctx, ToolListReviews, map[string]any{"show_id": id, "focus": plan.Focus}, slots, query, state))
		}
	case IntentVenue:
		shows := search()
		topic := query
		if len(shows) > 0 && strings.TrimSpace(shows[0].Address) != "" {
			topic = query + " " + shows[0].Address
		}
		result.absorb(r.call(ctx, ToolVenueTips, map[string]any{"topic": topic}, slots, query, state))
	case IntentPolicy:
		shows := search()
		if len(shows) > 0 {
			result.absorb(r.call(ctx, ToolShowDetail, map[string]any{"show_id": shows[0].ID}, slots, query, state))
		}
	case IntentExplore:
		search()
	case IntentWatchlist:
		// 「记一下这场」：先确认是哪一场，再准备一条待确认动作。
		shows := search()
		if len(shows) > 0 {
			result.absorb(r.call(ctx, ToolAddWatchlist, map[string]any{"show_id": shows[0].ID}, slots, query, state))
		}
	case IntentPreference:
		// 「以后都看上海的」：把本轮明确的条件存成长期偏好。
		saved := false
		if slots.City != "" {
			result.absorb(r.call(ctx, ToolSavePreference, map[string]any{"key": "city", "value": slots.City}, slots, query, state))
			saved = true
		}
		if slots.Category != "" {
			result.absorb(r.call(ctx, ToolSavePreference, map[string]any{"key": "category", "value": slots.Category}, slots, query, state))
			saved = true
		}
		if budget := slots.PerTicketBudget(); budget > 0 {
			result.absorb(r.call(ctx, ToolSavePreference, map[string]any{"key": "budget", "value": strconv.FormatFloat(budget, 'f', -1, 64)}, slots, query, state))
			saved = true
		}
		if slots.SeatPref != "" {
			result.absorb(r.call(ctx, ToolSavePreference, map[string]any{"key": "seat", "value": slots.SeatPref}, slots, query, state))
			saved = true
		}
		if !saved {
			state.step(Step{Phase: "observe", Title: "没有可保存的偏好", Detail: "用户没有说出城市、类型、预算或座位偏好", OK: false})
		}
	default: // recommend
		shows := search()
		if len(shows) > 0 {
			detail := r.call(ctx, ToolShowDetail, map[string]any{"show_id": shows[0].ID}, slots, query, state)
			result.absorb(detail)
			result.absorb(r.call(ctx, ToolListReviews, map[string]any{"show_id": shows[0].ID, "focus": plan.Focus}, slots, query, state))
			if price := cheapestTicket(detail); price > 0 {
				result.absorb(r.call(ctx, ToolBudgetPlan, map[string]any{"party": max(slots.Party, 1), "ticket_price": price, "total_budget": slots.TotalBudget()}, slots, query, state))
			}
		}
	}
	return result
}

// react 是模型驱动的执行器：模型看工具说明书自己决定调什么，
// 后端只做三件事——校验白名单、执行、把观察喂回去。
func (r *Runtime) react(ctx context.Context, req Request, query string, slots Slots, plan Plan, state *session) evidence {
	messages := []llm.Message{
		{Role: "system", Content: reactPrompt(plan)},
		{Role: "user", Content: reactUserPrompt(query, slots)},
	}
	found := evidence{}
	state.snapshot(snapshotState{engine: "react", stage: "act", messages: messages}, found)
	return r.reactLoop(ctx, query, slots, plan, state, messages, 0, 0, &found)
}

// reactLoop 是循环本体。Run 从第 0 轮开始，Resume 从中断那一轮之后开始，
// 两处共用同一份实现，续跑就不会走成另一条逻辑。
//
// 每一轮都写一份快照，代价是一次几十字节的追加写；换来的是「断在哪都能接上」。
func (r *Runtime) reactLoop(ctx context.Context, query string, slots Slots, plan Plan, state *session, messages []llm.Message, round int, failures int, found *evidence) evidence {
	for step := round + 1; step <= r.maxStep; step++ {
		messages = r.compact(messages, state)
		state.snapshot(snapshotState{engine: "react", stage: "act", round: step - 1, failures: failures, messages: messages}, *found)

		stepCtx, cancel := context.WithTimeout(ctx, perStepTimeout)
		raw, err := r.model.Complete(stepCtx, r.model.LargeModel(), messages, 0.1)
		cancel()
		if err != nil {
			reason := "model_error"
			if ctx.Err() != nil {
				reason = interruptionReason(ctx)
			}
			state.step(Step{Phase: "observe", Title: "模型调用失败", Detail: err.Error(), OK: false})
			// 只有「预算用完/被取消」才值得续跑；模型自身报错重试大概率同样失败。
			state.snapshot(snapshotState{engine: "react", round: step - 1, failures: failures, messages: messages, reason: reason}, *found)
			return *found
		}
		action, err := parseAction(raw)
		if err != nil {
			failures++
			messages = append(messages,
				llm.Message{Role: "assistant", Content: raw},
				llm.Message{Role: "user", Content: `格式不对。需要工具时输出 {"tool":"工具名","args":{...}}；信息足够时输出 {"final":true}。不要输出解释性文字。`},
			)
			if failures >= maxParseFailures {
				state.snapshot(snapshotState{engine: "react", round: step, failures: failures, messages: messages}, *found)
				return *found
			}
			continue
		}
		failures = 0
		if action.Final {
			state.snapshot(snapshotState{engine: "react", round: step, messages: messages}, *found)
			break
		}
		if len(action.Tools) > 0 {
			batch := r.callBatch(ctx, action.Tools, plan, slots, query, state)
			for _, observation := range batch {
				found.absorb(observation)
			}
			summary := make([]string, 0, len(batch))
			for index, observation := range batch {
				summary = append(summary, fmt.Sprintf("#%d %s：%s", index+1, observation.Tool, truncateRunes(observation.Observation, observationLimit/2)))
			}
			messages = append(messages,
				llm.Message{Role: "assistant", Content: raw},
				llm.Message{Role: "user", Content: "并行工具结果：\n" + strings.Join(summary, "\n")},
			)
			state.snapshot(snapshotState{engine: "react", round: step, messages: messages}, *found)
			continue
		}
		if !toolAllowed(plan.Intent, action.Tool) {
			messages = append(messages,
				llm.Message{Role: "assistant", Content: raw},
				llm.Message{Role: "user", Content: fmt.Sprintf("工具 %q 不在当前意图的可用列表中，请从说明书里重新选择，或直接 final。", action.Tool)},
			)
			state.snapshot(snapshotState{engine: "react", round: step, messages: messages}, *found)
			continue
		}
		observation := r.call(ctx, action.Tool, action.Args, slots, query, state)
		found.absorb(observation)
		messages = append(messages,
			llm.Message{Role: "assistant", Content: raw},
			llm.Message{Role: "user", Content: "工具结果：\n" + truncateRunes(observation.Observation, observationLimit)},
		)
		state.snapshot(snapshotState{engine: "react", round: step, messages: messages}, *found)
	}
	return *found
}

// callBatch 并行执行一组工具：先按「工具 + 参数」去重（模型常把同一调用写两遍），
// 再并发执行，结果按请求顺序返回，保证喂回模型的顺序稳定、可复现。
func (r *Runtime) callBatch(ctx context.Context, calls []toolCall, plan Plan, slots Slots, query string, state *session) []toolResult {
	seen := map[string]bool{}
	unique := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		tool := strings.TrimSpace(call.Tool)
		if tool == "" {
			continue
		}
		if !toolAllowed(plan.Intent, tool) {
			unique = append(unique, toolCall{Tool: tool}) // 交给调用处按未知/越权处理
			continue
		}
		raw, _ := json.Marshal(call.Args)
		key := tool + string(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, toolCall{Tool: tool, Args: call.Args})
	}
	results := make([]toolResult, len(unique))
	var wait sync.WaitGroup
	for index, call := range unique {
		if !toolAllowed(plan.Intent, call.Tool) {
			results[index] = toolResult{Tool: call.Tool, OK: false, Summary: "工具不可用", Observation: "错误：工具 " + call.Tool + " 不在当前意图的可用列表中"}
			continue
		}
		wait.Add(1)
		go func(index int, call toolCall) {
			defer wait.Done()
			results[index] = r.call(ctx, call.Tool, call.Args, slots, query, state)
		}(index, call)
	}
	wait.Wait()
	return results
}

// compact 是上下文压缩：决策轮之前如果消息太长，就把早期的工具观察换成一份摘要。
// 刻意做成确定性规则（不是再调一次模型），这样省钱、可测、不会改坏事实。
func (r *Runtime) compact(messages []llm.Message, state *session) []llm.Message {
	total := 0
	for _, message := range messages {
		total += len([]rune(message.Content))
	}
	if total <= contextBudget || len(messages) <= 3 {
		return messages
	}
	digest := make([]string, 0, digestSteps)
	state.mu.Lock()
	steps := state.result.Steps
	start := max(0, len(steps)-digestSteps)
	for _, step := range steps[start:] {
		if step.Phase != "tool" && step.Phase != "observe" {
			continue
		}
		line := step.Phase + "｜" + step.Title
		if step.Detail != "" {
			line += "｜" + truncateRunes(step.Detail, 60)
		}
		digest = append(digest, line)
	}
	state.mu.Unlock()

	compacted := []llm.Message{
		messages[0],
		messages[1],
		{Role: "assistant", Content: "（为控制上下文，较早的工具结果已压缩为摘要）\n" + strings.Join(digest, "\n")},
		{Role: "user", Content: "请基于以上摘要继续。如果摘要不足以判断，请重新调用工具查证，不要猜测。"},
	}
	after := 0
	for _, message := range compacted {
		after += len([]rune(message.Content))
	}
	state.step(Step{Phase: "observe", Title: "压缩上下文",
		Detail: fmt.Sprintf("消息 %d 字 → %d 字，保留最近 %d 步", total, after, len(digest)),
		OK:     true})
	return compacted
}

// call 执行单个工具并记录两步（tool + observe），让前端能实时看到进度。
func (r *Runtime) call(ctx context.Context, tool string, args map[string]any, slots Slots, query string, state *session) toolResult {
	if IsWriteTool(tool) {
		return r.propose(ctx, tool, args, slots, query, state)
	}
	raw, _ := json.Marshal(args)
	started := time.Now()
	toolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result := executeToolWith(toolCtx, r.kit, r.kb, tool, raw, slots, query)
	cancel()
	elapsed := time.Since(started).Milliseconds()

	state.step(Step{
		Phase: "tool", Tool: tool, OK: result.OK, MS: elapsed,
		Title:  toolTitle(tool, args),
		Detail: toolArgsText(tool, args),
	})
	state.step(Step{
		Phase: "observe", Tool: tool, OK: result.OK, MS: elapsed,
		Title:  result.Summary,
		Detail: truncateRunes(strings.ReplaceAll(result.Observation, "\n", " "), 240),
	})
	// 记住演出名：写操作提案里要显示《雷雨》而不是「演出 #1」。
	for _, show := range result.Shows {
		if show.ID > 0 && show.Name != "" {
			state.names[show.ID] = show.Name
		}
	}
	for _, detail := range result.Details {
		if detail.Show.ID > 0 && detail.Show.Name != "" {
			state.names[detail.Show.ID] = detail.Show.Name
		}
	}
	return result
}

// propose 是写工具的唯一出口：只生成待确认动作，绝不直接落库。
// 返回给模型的观察会明确写「没有执行」，防止它在回答里说「已经加好了」。
func (r *Runtime) propose(ctx context.Context, tool string, args map[string]any, slots Slots, query string, state *session) toolResult {
	started := time.Now()
	raw, _ := json.Marshal(args)
	result := executeToolWith(ctx, r.kit, r.kb, tool, raw, slots, query)
	elapsed := time.Since(started).Milliseconds()
	if !result.OK || result.Pending == nil {
		state.step(Step{Phase: "tool", Tool: tool, OK: false, MS: elapsed, Title: toolTitle(tool, args), Detail: toolArgsText(tool, args)})
		state.step(Step{Phase: "observe", Tool: tool, OK: false, MS: elapsed, Title: result.Summary, Detail: result.Observation})
		return result
	}
	if r.sink == nil {
		result.OK = false
		result.Summary = "写操作未启用"
		result.Observation = "错误：当前部署没有开启写操作审批，只能给出建议。"
		state.step(Step{Phase: "observe", Tool: tool, OK: false, MS: elapsed, Title: result.Summary})
		return result
	}

	action := *result.Pending
	if action.ShowID > 0 && action.ShowName == "" {
		if name := state.names[action.ShowID]; name != "" {
			action.ShowName = name
			// 提案标题里已经写死了「《演出 #1》」，拿到真名后整体替换，避免出现双书名号。
			token := "演出 #" + strconv.FormatInt(action.ShowID, 10)
			action.Summary = strings.ReplaceAll(action.Summary, "《"+token+"》", "《"+name+"》")
			action.Summary = strings.ReplaceAll(action.Summary, token, name)
		}
	}
	saved, err := r.sink.Propose(ctx, state.ownerID, action)
	if err != nil {
		result.OK = false
		result.Summary = "写操作提案失败"
		result.Observation = "错误：" + err.Error()
		state.step(Step{Phase: "observe", Tool: tool, OK: false, MS: elapsed, Title: result.Summary, Detail: result.Observation})
		return result
	}

	result.OK = true
	result.Pending = &saved
	result.Summary = "已准备：" + saved.Title + "（待确认 " + saved.ID + "）"
	result.Observation = writeToolHint(saved)
	state.result.Actions = append(state.result.Actions, saved)
	state.step(Step{Phase: "tool", Tool: tool, OK: true, MS: elapsed, Title: toolTitle(tool, args), Detail: toolArgsText(tool, args)})
	state.step(Step{Phase: "observe", Tool: tool, OK: true, MS: elapsed, Title: result.Summary, Detail: saved.Summary})
	_ = emitEvent(state.emit, Event{Type: "action", Action: &saved})
	return result
}

// absorb 把工具结果并入 evidence，并去重演出 id。
func (e *evidence) absorb(result toolResult) {
	e.results = append(e.results, result)
	if result.Tool != "" {
		e.tools = append(e.tools, result.Tool)
	}
	e.shows = append(e.shows, result.Shows...)
	e.details = append(e.details, result.Details...)
	e.tips = append(e.tips, result.Tips...)
	if result.Budget != nil {
		e.budget = result.Budget
	}
	if result.Pending != nil && result.Pending.Status == ActionPending {
		e.actions = append(e.actions, *result.Pending)
	}
	for _, show := range result.Shows {
		e.addID(show.ID)
	}
	for _, detail := range result.Details {
		e.addID(detail.Show.ID)
		e.shows = append(e.shows, detail.Show)
	}
	if len(result.Shows) > 0 || len(result.Details) > 0 || len(result.Tips) > 0 || result.Budget != nil {
		e.available = true
	}
}

func (e *evidence) addID(id int64) {
	if id <= 0 || len(e.showIDs) >= 4 {
		return
	}
	for _, existing := range e.showIDs {
		if existing == id {
			return
		}
	}
	e.showIDs = append(e.showIDs, id)
}

// streamAnswer 把事实交给模型流式改写；失败时由调用方回退到数据版答案。
func (r *Runtime) streamAnswer(ctx context.Context, req Request, query string, slots Slots, plan Plan, found evidence, dataAnswer string, state *session) (string, error) {
	facts := buildFacts(plan, slots, found)
	messages := []llm.Message{{Role: "system", Content: answerPrompt}}
	history := req.History
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	for _, message := range history {
		if (message.Role == "user" || message.Role == "assistant") && len([]rune(message.Content)) <= 1200 {
			messages = append(messages, message)
		}
	}
	messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf(
		"用户问题：%s\n当前条件：%s\n工具查到的事实（唯一数据来源）：\n%s\n参考的数据版回答（可改写措辞，不得新增事实）：\n%s",
		query, describeSlots(slots), facts, dataAnswer)})

	var builder strings.Builder
	// 流式回调里直接推给前端，不做二次缓冲：用户能看到逐字出现。
	writer := func(delta string) error {
		builder.WriteString(delta)
		return state.chunk(delta)
	}
	err := r.model.Stream(ctx, r.model.LargeModel(), messages, 0.3, writer)
	return builder.String(), err
}

// selfCheck 是最后一道闸门：越权表述、空答案都会被打回数据版。
func (r *Runtime) selfCheck(answer string, dataAnswer string, found evidence, note string, state *session) (string, string) {
	started := time.Now()
	problem := ""
	switch {
	case strings.TrimSpace(answer) == "":
		problem = "回答为空"
	case containsForbidden(answer):
		problem = "出现「已下单/已支付/已锁座」等越权表述"
	}
	if problem == "" {
		state.step(Step{Phase: "check", Title: "自检通过", OK: true,
			Detail: fmt.Sprintf("引用 %d 场演出、%d 次工具结果", len(found.showIDs), len(found.tools)),
			MS:     time.Since(started).Milliseconds()})
		return answer, note
	}
	state.step(Step{Phase: "check", Title: "自检未通过，回退数据版", Detail: problem, OK: false, MS: time.Since(started).Milliseconds()})
	state.result.Mode = "agent-rule"
	if note != "" {
		note += " "
	}
	note += "模型的表述越过了权限边界，已改为按数据整理的回答。"
	return dataAnswer, note
}

func containsForbidden(answer string) bool {
	for _, phrase := range []string{"已下单", "已支付", "支付成功", "已锁座", "已出票", "已经帮你买"} {
		if strings.Contains(answer, phrase) {
			return true
		}
	}
	return false
}

// action 是 ReAct 的合法输出：调一个工具、并行调一组工具，或结束。
// 允许并行是因为「检索 + 查常识」这类动作互不依赖，串行只是白等。
type action struct {
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args"`
	Tools []toolCall     `json:"tools"`
	Final bool           `json:"final"`
}

// toolCall 是并行批次里的单个调用。
type toolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

func parseAction(raw string) (action, error) {
	var parsed action
	content := extractJSONObject(raw)
	if content == "" {
		return parsed, fmt.Errorf("没有 JSON")
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return parsed, err
	}
	if strings.EqualFold(strings.TrimSpace(parsed.Tool), "final") {
		parsed.Final = true
		parsed.Tool = ""
	}
	if !parsed.Final && strings.TrimSpace(parsed.Tool) == "" && len(parsed.Tools) == 0 {
		return parsed, fmt.Errorf("tool 为空")
	}
	if len(parsed.Tools) > maxParallelTools {
		parsed.Tools = parsed.Tools[:maxParallelTools]
	}
	return parsed, nil
}

// extractJSONObject 从可能夹带解释文字的输出里抠出第一个完整 JSON 对象。
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for index := start; index < len(text); index++ {
		switch text[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : index+1]
			}
		}
	}
	return ""
}

func inferIntent(query string) string {
	lower := strings.ToLower(query)
	switch {
	case containsAny(lower, "对比", "比较", "哪个更好", "选哪个", "vs"):
		return IntentCompare
	case containsAny(lower, "评测", "评分", "口碑", "评价", "避雷", "好看吗", "值得看"):
		return IntentReviews
	case containsAny(lower, "退改", "能退", "退款", "秒杀规则", "入场", "迟到", "儿童", "身高", "停车", "地铁", "交通", "要带什么"):
		return IntentVenue
	case containsAny(lower, "记一下", "记住", "收藏", "加个提醒", "观演清单"):
		return IntentWatchlist
	case containsAny(lower, "以后都", "以后只", "我一般", "通常都", "默认看"):
		return IntentPreference
	case containsAny(lower, "推荐", "找", "想看", "有什么", "适合", "预算"):
		return IntentRecommend
	default:
		return IntentExplore
	}
}

func normalizeIntent(intent string) string {
	switch value := strings.ToLower(strings.TrimSpace(intent)); value {
	case IntentRecommend, IntentCompare, IntentReviews, IntentVenue, IntentPolicy, IntentExplore, IntentWatchlist, IntentPreference:
		return value
	default:
		return ""
	}
}

func containsAny(text string, keywords ...string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func idsOf(shows []Show, limit int) []int64 {
	ids := make([]int64, 0, limit)
	for _, show := range shows {
		if show.ID > 0 && len(ids) < limit {
			ids = append(ids, show.ID)
		}
	}
	return ids
}

func cheapestTicket(result toolResult) float64 {
	price := 0.0
	for _, detail := range result.Details {
		for _, ticket := range detail.Tickets {
			if ticket.SoldOut || ticket.Price <= 0 {
				continue
			}
			if price == 0 || ticket.Price < price {
				price = ticket.Price
			}
		}
	}
	return price
}

func toolTitle(tool string, args map[string]any) string {
	switch tool {
	case ToolSearchShows:
		return "检索符合条件的演出"
	case ToolShowDetail:
		return fmt.Sprintf("读取演出 #%v 的票档", args["show_id"])
	case ToolListReviews:
		return fmt.Sprintf("摘录演出 #%v 的观众评测", args["show_id"])
	case ToolCompareShows:
		return "并排对比候选演出"
	case ToolVenueTips:
		return "检索观演常识"
	case ToolBudgetPlan:
		return "计算门票合计与剩余预算"
	default:
		return tool
	}
}

func toolArgsText(tool string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncateRunes(string(raw), 120)
}

func planStep(plan Plan, started time.Time, toolCount int) Step {
	source := "规则规划"
	if !plan.FromRule {
		source = "模型规划"
	}
	return Step{
		Phase: "planner", OK: true, MS: time.Since(started).Milliseconds(),
		Title:  fmt.Sprintf("%s：%s", source, intentLabel(plan.Intent)),
		Detail: fmt.Sprintf("%s；可用工具 %d 个", strings.TrimSpace(plan.Reason), toolCount),
	}
}

func intentLabel(intent string) string {
	switch intent {
	case IntentRecommend:
		return "找演出/选票"
	case IntentCompare:
		return "多场对比"
	case IntentReviews:
		return "看口碑与评测"
	case IntentVenue:
		return "场馆与观演常识"
	case IntentPolicy:
		return "退改与规则"
	case IntentWatchlist:
		return "加入观演清单（待确认）"
	case IntentPreference:
		return "记住长期偏好（待确认）"
	default:
		return "宽泛浏览"
	}
}

// chunkText 按 rune 切片，中文不会被截成半个字。
func chunkText(text string, size int) []string {
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}
	pieces := make([]string, 0, len(runes)/size+1)
	for index := 0; index < len(runes); index += size {
		end := min(index+size, len(runes))
		pieces = append(pieces, string(runes[index:end]))
	}
	return pieces
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func emitEvent(emit func(Event) error, event Event) error {
	if emit == nil {
		return nil
	}
	return emit(event)
}

// Resume 从快照继续一次中断的运行：已经查过的事实不重查，只补没走完的部分。
//
// 这也是断点续跑和「重试」的区别：重试从头再来，续跑复用已有证据。
func (r *Runtime) Resume(ctx context.Context, runID string, emit func(Event) error) (Result, error) {
	started := time.Now()
	if r.checkpoints == nil {
		return Result{}, fmt.Errorf("当前部署没有开启断点续跑")
	}
	checkpoint, ok := r.checkpoints.Load(strings.TrimSpace(runID))
	if !ok {
		return Result{}, fmt.Errorf("这次运行无法恢复：快照已过期或已完成")
	}
	ctx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()

	result := Result{
		RunID: checkpoint.RunID, Steps: append([]Step{}, checkpoint.Steps...),
		Slots: checkpoint.Slots, Plan: checkpoint.Plan, Intent: checkpoint.Plan.Intent,
		Actions: checkpoint.Actions, ResumedFrom: checkpoint.RunID,
	}
	state := &session{
		result: &result, emit: emit, names: map[int64]string{},
		ownerID: checkpoint.OwnerID, runID: checkpoint.RunID, sessionID: checkpoint.SessionID,
		query: checkpoint.Query, history: checkpoint.History, plan: checkpoint.Plan,
		slots: checkpoint.Slots, store: r.checkpoints,
	}
	found := checkpoint.Evidence.restore()
	// 演出名要还原，否则续跑时写操作提案又变回「演出 #1」。
	for _, show := range found.shows {
		if show.ID > 0 && show.Name != "" {
			state.names[show.ID] = show.Name
		}
	}
	state.step(Step{
		Phase: "observe", OK: true, Title: "从断点继续",
		Detail: fmt.Sprintf("沿用第 %d 轮之前查到的 %d 次工具结果，不重复查询", checkpoint.Round, len(found.tools)),
	})

	// 原来卡在 ReAct 循环里：把没走完的轮次接着跑完。
	if checkpoint.Engine == "react" && checkpoint.Stage == "act" && r.model != nil && r.model.Enabled() {
		messages := checkpoint.Messages
		if len(messages) == 0 {
			messages = []llm.Message{
				{Role: "system", Content: reactPrompt(checkpoint.Plan)},
				{Role: "user", Content: reactUserPrompt(checkpoint.Query, checkpoint.Slots)},
			}
		}
		found = r.reactLoop(ctx, checkpoint.Query, checkpoint.Slots, checkpoint.Plan, state, messages, checkpoint.Round, checkpoint.Failures, &found)
	}

	req := Request{
		Query: checkpoint.Query, SessionID: checkpoint.SessionID,
		OwnerID: checkpoint.OwnerID, History: checkpoint.History,
		Slots: checkpoint.Slots,
	}
	answer, note := r.finalize(ctx, req, checkpoint.Query, checkpoint.Slots, checkpoint.Plan, found, "", state)
	result.Answer = answer
	result.Note = note
	result.ShowIDs = found.showIDs
	result.Elapsed = time.Since(started).Milliseconds()

	// 续跑也可能再次超时，所以只在真的跑完时才清快照。
	if ctx.Err() == nil {
		r.checkpoints.Delete(result.RunID)
	} else {
		result.Resumable = true
		result.ResumeReason = interruptionReason(ctx)
		result.ResumeStage = "answer"
		result.Note = joinNote(result.Note, interruptionNote(result.ResumeReason))
	}

	r.memory.Save(checkpoint.SessionID, checkpoint.Slots, checkpoint.Plan.Intent)
	if r.traces != nil {
		r.traces.Add(Run{
			RunID: result.RunID, SessionID: checkpoint.SessionID, Query: checkpoint.Query,
			Intent: checkpoint.Plan.Intent, Mode: result.Mode, StartedAt: now(),
			ElapsedMS: result.Elapsed, Steps: result.Steps, Tools: found.tools,
			ShowIDs: found.showIDs, Slots: checkpoint.Slots, Plan: checkpoint.Plan,
			Answer: answer, Note: note, Resumable: result.Resumable,
			ResumedFrom: checkpoint.RunID,
		})
	}
	return result, nil
}

// interruptionReason 把 context 的结束原因翻译成可展示的词。
func interruptionReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}

func interruptionNote(reason string) string {
	switch reason {
	case "timeout":
		return "这次推理用完了时间预算，已查到的结果保留；可以继续上一次推理，不必重新查。"
	case "canceled":
		return "请求被中断，已查到的结果保留；可以继续上一次推理。"
	default:
		return ""
	}
}

func joinNote(base string, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + " " + extra
}
