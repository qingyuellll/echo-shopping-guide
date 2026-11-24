package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// ---------- 测试替身 ----------

type fakeKit struct {
	shows    []Show
	detail   Detail
	fail     bool
	searches int
	details  int
	reviews  int
}

func (f *fakeKit) Search(ctx context.Context, q SearchQuery) ([]Show, error) {
	f.searches++
	if f.fail {
		return nil, errors.New("upstream down")
	}
	return f.shows, nil
}

func (f *fakeKit) Detail(ctx context.Context, id int64) (Detail, error) {
	f.details++
	if f.fail {
		return Detail{}, errors.New("upstream down")
	}
	detail := f.detail
	detail.Show.ID = id
	return detail, nil
}

func (f *fakeKit) Compare(ctx context.Context, ids []int64) ([]Detail, error) {
	out := make([]Detail, 0, len(ids))
	for _, id := range ids {
		detail := f.detail
		detail.Show.ID = id
		out = append(out, detail)
	}
	return out, nil
}

func (f *fakeKit) Reviews(ctx context.Context, id int64, focus string, limit int) (Detail, error) {
	f.reviews++
	detail := f.detail
	detail.Show.ID = id
	detail.Tickets = nil
	return detail, nil
}

type fakeModel struct {
	enabled   bool
	complete  func(messages []llm.Message) (string, error)
	stream    string
	streamErr error
	calls     int
}

func (f *fakeModel) Enabled() bool      { return f.enabled }
func (f *fakeModel) SmallModel() string { return "small" }
func (f *fakeModel) LargeModel() string { return "large" }

func (f *fakeModel) Complete(ctx context.Context, model string, messages []llm.Message, temperature float64) (string, error) {
	f.calls++
	if f.complete == nil {
		return "", errors.New("no script")
	}
	return f.complete(messages)
}

func (f *fakeModel) Stream(ctx context.Context, model string, messages []llm.Message, temperature float64, onDelta func(string) error) error {
	if f.streamErr != nil {
		return f.streamErr
	}
	text := f.stream
	if text == "" {
		text = "默认回答"
	}
	// 按两段推，验证前端会收到多次 delta。
	for _, piece := range []string{text[:len(text)/2], text[len(text)/2:]} {
		if err := onDelta(piece); err != nil {
			return err
		}
	}
	return nil
}

func fixtureKit() *fakeKit {
	price := 280.0
	maxPrice := 380.0
	return &fakeKit{
		shows: []Show{{
			ID: 1, Name: "雷雨", TypeName: "话剧", Area: "上海市", Address: "人民大舞台",
			OpenTime: "2026-12-05 19:30", Lifecycle: "upcoming",
			Rating: 4.2, RatingCount: 12, MinPrice: &price, MaxPrice: &maxPrice, AvailableStock: 37,
		}},
		detail: Detail{
			Show: Show{ID: 1, Name: "雷雨", TypeName: "话剧", Area: "上海市", Address: "人民大舞台",
				OpenTime: "2026-12-05 19:30", Lifecycle: "upcoming", Rating: 4.2, RatingCount: 12, MinPrice: &price},
			Tickets: []Ticket{
				{ID: 11, Title: "一楼 A 区", Price: 380, Stock: 12, HasSeats: true, Rules: "演出前 3 天可退"},
				{ID: 12, Title: "二楼看台", Price: 180, Stock: 25, Rules: "不可退"},
			},
			Positive: []Quote{{ID: 101, Author: "观众甲", Score: 5, Content: "演员台词功底扎实，第三幕很震撼。"}},
			Critical: []Quote{{ID: 102, Author: "观众乙", Score: 3, Content: "剧情不错，但二楼座位间距小。"}},
			Cautions: []string{"有效评分少于 5 个，口碑样本较少"},
			Keywords: []string{"台词", "舞台"},
		},
	}
}

func collectEvents() (func(Event) error, *[]Event) {
	events := []Event{}
	return func(event Event) error {
		events = append(events, event)
		return nil
	}, &events
}

// ---------- 纯函数 ----------

func TestPlanBudgetHandlesBoundaries(t *testing.T) {
	cases := []struct {
		party int
		price float64
		total float64
		cost  float64
		over  bool
	}{
		{2, 180, 500, 360, false},
		{3, 200, 500, 600, true},
		{0, 180, 0, 180, false}, // 人数缺失按 1 人
		{2, 250, 500, 500, false},
	}
	for _, test := range cases {
		plan := PlanBudget(test.party, test.price, test.total)
		if plan.TicketCost != test.cost || plan.OverBudget != test.over {
			t.Fatalf("%+v => cost=%.2f over=%v", test, plan.TicketCost, plan.OverBudget)
		}
	}
}

func TestSearchKnowledgeRanksByTopic(t *testing.T) {
	hits := SearchKnowledge("带孩子看儿童剧有什么身高要求", 3)
	if len(hits) == 0 || !strings.Contains(hits[0].Title, "儿童") {
		t.Fatalf("expected 儿童 entry first, got %+v", hits)
	}
	if got := SearchKnowledge("完全不相关的天文物理", 3); len(got) != 0 {
		t.Fatalf("expected no hits, got %+v", got)
	}
}

func TestInferIntent(t *testing.T) {
	cases := map[string]string{
		"周六两个人600元想看话剧": IntentRecommend,
		"这两场哪个更好":       IntentCompare,
		"有什么避雷的评测吗":     IntentReviews,
		"迟到还能进场吗":       IntentVenue,
		"随便看看":          IntentExplore,
	}
	for query, want := range cases {
		if got := inferIntent(query); got != want {
			t.Fatalf("%q => %s want %s", query, got, want)
		}
	}
}

// ---------- 规则执行器 ----------

func TestRuleChainRunsToolsAndBuildsAnswer(t *testing.T) {
	kit := fixtureKit()
	runtime := New(kit, nil, nil, nil)
	emit, events := collectEvents()

	result, err := runtime.Run(context.Background(), Request{
		Query:     "两个人 500 元以内想看话剧",
		SessionID: "s-1",
		Slots:     Slots{Party: 2, Budget: 500, BudgetMode: "total", Category: "话剧"},
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "agent-rule" || result.Intent != IntentRecommend {
		t.Fatalf("mode=%s intent=%s", result.Mode, result.Intent)
	}
	if kit.searches == 0 || kit.details == 0 || kit.reviews == 0 {
		t.Fatalf("expected search/detail/reviews calls, got %+v", kit)
	}
	for _, want := range []string{"《雷雨》", "门票合计", "观众原话 #101", "不能代下单"} {
		if !strings.Contains(result.Answer, want) {
			t.Fatalf("answer missing %q:\n%s", want, result.Answer)
		}
	}
	if len(result.ShowIDs) == 0 || result.ShowIDs[0] != 1 {
		t.Fatalf("show ids = %v", result.ShowIDs)
	}

	phases := map[string]bool{}
	chunks := 0
	for _, event := range *events {
		if event.Type == "step" && event.Step != nil {
			phases[event.Step.Phase] = true
		}
		if event.Type == "chunk" {
			chunks++
		}
	}
	for _, phase := range []string{"planner", "tool", "observe", "check"} {
		if !phases[phase] {
			t.Fatalf("missing step phase %s (got %v)", phase, phases)
		}
	}
	if chunks < 2 {
		t.Fatalf("expected chunked streaming, got %d chunks", chunks)
	}
	// 步骤序号必须连续，前端时间线依赖它。
	for index, step := range result.Steps {
		if step.Index != index+1 {
			t.Fatalf("step index = %d want %d", step.Index, index+1)
		}
	}
}

func TestRuleChainHandlesUpstreamFailure(t *testing.T) {
	kit := fixtureKit()
	kit.fail = true
	runtime := New(kit, nil, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "推荐话剧", SessionID: "s-2", Intent: IntentRecommend}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "没有找到") && !strings.Contains(result.Answer, "错误") {
		t.Fatalf("answer should be actionable, got:\n%s", result.Answer)
	}
}

// ---------- 多轮记忆 ----------

func TestMemoryCarriesSlotsAcrossTurns(t *testing.T) {
	memory := NewMemoryStore(time.Minute, 10)
	runtime := New(fixtureKit(), nil, memory, nil)

	first, err := runtime.Run(context.Background(), Request{
		Query: "两个人 600 元", SessionID: "s-3",
		Slots: Slots{Party: 2, Budget: 600, BudgetMode: "total", City: "上海"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Slots.Budget != 600 || first.Slots.Party != 2 {
		t.Fatalf("first slots = %+v", first.Slots)
	}

	// 第二轮没提预算、人数、城市，只补了类型，应该继承上一轮。
	second, err := runtime.Run(context.Background(), Request{
		Query: "那有话剧吗", SessionID: "s-3", Slots: Slots{Category: "话剧"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Slots.Budget != 600 || second.Slots.Party != 2 || second.Slots.City != "上海" {
		t.Fatalf("memory not carried: %+v", second.Slots)
	}
	if !strings.Contains(second.Answer, "沿用上轮") {
		t.Fatalf("answer should mention memory: %s", second.Answer)
	}

	// 第三轮显式覆盖：改成 2 人 300，城市改北京。
	third, err := runtime.Run(context.Background(), Request{
		Query: "改成 2 人 300，北京", SessionID: "s-3",
		Slots: Slots{Party: 2, Budget: 300, BudgetMode: "total", City: "北京"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Slots.Budget != 300 || third.Slots.City != "北京" {
		t.Fatalf("override failed: %+v", third.Slots)
	}
}

// ---------- ReAct ----------

func TestReactRespectsAllowlistAndMaxSteps(t *testing.T) {
	kit := fixtureKit()
	// 模型一直想调工具，运行时应被最大步数截断，而不是无限循环。
	model := &fakeModel{enabled: true, complete: func(messages []llm.Message) (string, error) {
		last := messages[len(messages)-1].Content
		if strings.Contains(last, "只输出 JSON：{\"intent\"") {
			return `{"intent":"recommend","focus":"预算","reason":"用户在挑演出"}`, nil
		}
		return `{"tool":"search_shows","args":{"category":"话剧"}}`, nil
	}}
	runtime := New(kit, model, nil, nil, WithMaxSteps(2))
	result, err := runtime.Run(context.Background(), Request{Query: "推荐话剧", SessionID: "s-4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kit.searches != 2 {
		t.Fatalf("expected 2 tool calls (max steps), got %d", kit.searches)
	}
	if result.Plan.FromRule {
		t.Fatal("planner should come from model")
	}
}

func TestReactRejectsToolsOutsideIntent(t *testing.T) {
	kit := fixtureKit()
	// compare 意图下模型却想调 budget_plan，应被白名单挡住并收到纠偏提示。
	step := 0
	model := &fakeModel{enabled: true, complete: func(messages []llm.Message) (string, error) {
		step++
		if step == 1 {
			return `{"intent":"compare","focus":"价格","reason":"两场对比"}`, nil
		}
		if step == 2 {
			return `{"tool":"budget_plan","args":{"party":2,"ticket_price":100}}`, nil
		}
		return `{"final":true}`, nil
	}}
	runtime := New(kit, model, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "这两场对比一下", SessionID: "s-5", Intent: IntentCompare}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Plan.Tools {
		if tool == ToolBudgetPlan {
			t.Fatal("budget_plan should not be allowed for compare")
		}
	}
	if result.Intent != IntentCompare {
		t.Fatalf("intent = %s", result.Intent)
	}
}

func TestReactFallsBackWhenModelKeepsBreakingProtocol(t *testing.T) {
	kit := fixtureKit()
	model := &fakeModel{
		enabled: true,
		// 决策输出不是 JSON，连续失败后应回退规则链；成文也失败，才会用数据版答案。
		complete:  func(messages []llm.Message) (string, error) { return "我觉得应该先看看演出列表", nil },
		streamErr: errors.New("model offline"),
	}
	runtime := New(kit, model, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "推荐话剧", SessionID: "s-6"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kit.searches == 0 {
		t.Fatal("expected rule chain fallback to run search")
	}
	if result.Mode != "agent-rule" {
		t.Fatalf("mode = %s", result.Mode)
	}
}

// ---------- 自检 ----------

func TestSelfCheckBlocksPurchaseClaims(t *testing.T) {
	kit := fixtureKit()
	model := &fakeModel{enabled: true,
		stream: "已经帮你买好了，已支付 360 元，座位也锁好了。",
		complete: func(messages []llm.Message) (string, error) {
			return `{"intent":"recommend","reason":"挑演出"}`, nil
		},
	}
	runtime := New(kit, model, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "两个人 500 元", SessionID: "s-7", Slots: Slots{Party: 2, Budget: 500, BudgetMode: "total"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Answer, "已支付") {
		t.Fatalf("guardrail failed, answer kept the claim:\n%s", result.Answer)
	}
	if !strings.Contains(result.Note, "权限边界") {
		t.Fatalf("expected note about guardrail, got %q", result.Note)
	}
	if result.Mode != "agent-rule" {
		t.Fatalf("mode should fall back, got %s", result.Mode)
	}
}

// ---------- 轨迹 ----------

func TestTraceStoreRecordsRun(t *testing.T) {
	traces := NewTraceStore("", 5)
	runtime := New(fixtureKit(), nil, nil, traces)
	result, err := runtime.Run(context.Background(), Request{Query: "推荐话剧", SessionID: "s-8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := traces.List(10)
	if len(list) != 1 || list[0].RunID != result.RunID {
		t.Fatalf("trace not recorded: %+v", list)
	}
	if len(list[0].Tools) == 0 || len(list[0].Steps) == 0 {
		t.Fatalf("trace missing tools/steps: %+v", list[0])
	}
	if _, ok := traces.Get(result.RunID); !ok {
		t.Fatal("Get should find the run")
	}
}

func TestExecuteToolRejectsBadArguments(t *testing.T) {
	kit := fixtureKit()
	result := executeTool(context.Background(), kit, ToolShowDetail, []byte(`{"show_id":"abc"}`), Slots{}, "")
	if result.OK {
		t.Fatal("invalid id should fail")
	}
	if !strings.Contains(result.Observation, "正整数") {
		t.Fatalf("observation should explain: %s", result.Observation)
	}
	unknown := executeTool(context.Background(), kit, "drop_database", nil, Slots{}, "")
	if unknown.OK || !strings.Contains(unknown.Observation, "没有名为") {
		t.Fatalf("unknown tool should be rejected: %+v", unknown)
	}
}

func TestAnswerIncludesEndedWarning(t *testing.T) {
	kit := fixtureKit()
	kit.detail.Show.Lifecycle = "ended"
	runtime := New(kit, nil, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "看看雷雨", SessionID: "s-9", Intent: IntentReviews}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "已结束") {
		t.Fatalf("ended show must be flagged: %s", result.Answer)
	}
}

// ---------- 写操作审批 ----------

type fakeSink struct {
	proposed []PendingAction
	fail     bool
}

func (f *fakeSink) Propose(ctx context.Context, ownerID string, action PendingAction) (PendingAction, error) {
	if f.fail {
		return PendingAction{}, errors.New("store offline")
	}
	action.ID = "act-0001"
	action.OwnerID = ownerID
	action.Status = ActionPending
	action.CreatedAt = time.Now()
	f.proposed = append(f.proposed, action)
	return action, nil
}

func TestWriteToolIsProposedNotExecuted(t *testing.T) {
	kit := fixtureKit()
	sink := &fakeSink{}
	step := 0
	model := &fakeModel{enabled: true,
		// 成文交给数据版答案，方便断言「待确认」段落。
		streamErr: errors.New("model offline"),
		complete: func(messages []llm.Message) (string, error) {
			step++
			switch step {
			case 1:
				return `{"intent":"recommend","focus":"收藏","reason":"用户想记一下"}`, nil
			case 2:
				return `{"tool":"search_shows","args":{"category":"话剧"}}`, nil
			case 3:
				return `{"tool":"add_watchlist","args":{"show_id":1,"note":"周末想看"}}`, nil
			default:
				return `{"final":true}`, nil
			}
		}}
	runtime := New(kit, model, nil, nil, WithActionSink(sink))
	result, err := runtime.Run(context.Background(), Request{Query: "帮我把雷雨记一下", SessionID: "s-w1", OwnerID: "owner-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 写工具不能触发任何真实查询/写入，只能产生一条提案。
	if len(sink.proposed) != 1 || sink.proposed[0].Kind != ActionWatchlist || sink.proposed[0].ShowID != 1 {
		t.Fatalf("expected one watchlist proposal, got %+v", sink.proposed)
	}
	if sink.proposed[0].OwnerID != "owner-1" {
		t.Fatalf("owner not carried: %+v", sink.proposed[0])
	}
	if sink.proposed[0].ShowName != "雷雨" {
		t.Fatalf("show name should come from earlier observations, got %q", sink.proposed[0].ShowName)
	}
	if len(result.Actions) != 1 || result.Actions[0].Status != ActionPending {
		t.Fatalf("result actions = %+v", result.Actions)
	}
	if !strings.Contains(result.Answer, "需要你确认") || !strings.Contains(result.Answer, "当前未执行") {
		t.Fatalf("answer must make the pending state explicit://n%s", result.Answer)
	}
}

func TestWriteToolWithoutSinkIsRejected(t *testing.T) {
	step := 0
	model := &fakeModel{enabled: true, complete: func(messages []llm.Message) (string, error) {
		step++
		if step == 1 {
			return `{"intent":"recommend","reason":"偏好"}`, nil
		}
		if step == 2 {
			return `{"tool":"save_preference","args":{"key":"city","value":"上海"}}`, nil
		}
		return `{"final":true}`, nil
	}}
	runtime := New(fixtureKit(), model, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "以后都看上海的", SessionID: "s-w2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 0 {
		t.Fatalf("no sink should mean no actions: %+v", result.Actions)
	}
	if !strings.Contains(result.Answer, "没有找到") && !strings.Contains(result.Answer, "已记住") {
		t.Logf("answer=%s", result.Answer)
	}
}

func TestPreferenceToolValidatesKeyAndValue(t *testing.T) {
	kit := fixtureKit()
	slots := Slots{}
	cases := []struct {
		args map[string]any
		ok   bool
	}{
		{map[string]any{"key": "city", "value": "上海"}, true},
		{map[string]any{"key": "budget", "value": "500"}, true},
		{map[string]any{"key": "seat", "value": "front"}, true},
		{map[string]any{"key": "seat", "value": "靠窗"}, false},
		{map[string]any{"key": "nickname", "value": "小明"}, false},
		{map[string]any{"key": "budget", "value": "abc"}, false},
	}
	for _, test := range cases {
		raw, _ := json.Marshal(test.args)
		result := executeTool(context.Background(), kit, ToolSavePreference, raw, slots, "")
		if result.OK != test.ok || (test.ok && result.Pending == nil) {
			t.Fatalf("%v => ok=%v pending=%+v", test.args, result.OK, result.Pending)
		}
	}
}

// ---------- 并行工具 ----------

func TestParallelBatchDedupesAndRuns(t *testing.T) {
	kit := fixtureKit()
	step := 0
	model := &fakeModel{enabled: true, complete: func(messages []llm.Message) (string, error) {
		step++
		switch step {
		case 1:
			return `{"intent":"recommend","reason":"并行查询"}`, nil
		case 2:
			return `{"tools":[{"tool":"search_shows","args":{"category":"话剧"}},{"tool":"search_shows","args":{"category":"话剧"}},{"tool":"show_detail","args":{"show_id":1}}]}`, nil
		default:
			return `{"final":true}`, nil
		}
	}}
	runtime := New(kit, model, nil, nil)
	result, err := runtime.Run(context.Background(), Request{Query: "推荐话剧并看票档", SessionID: "s-p1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 两个完全相同的调用被去重，只执行一次。
	if kit.searches != 1 {
		t.Fatalf("duplicate calls should collapse, searches=%d", kit.searches)
	}
	if kit.details != 1 {
		t.Fatalf("detail should run once, got %d", kit.details)
	}
	if len(result.ShowIDs) == 0 {
		t.Fatal("parallel results should feed evidence")
	}
}

func TestParseActionSupportsBatch(t *testing.T) {
	action, err := parseAction(`{"tools":[{"tool":"search_shows","args":{"city":"上海"}},{"tool":"venue_tips","args":{"topic":"儿童"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(action.Tools) != 2 || action.Tools[0].Tool != ToolSearchShows || action.Tools[1].Tool != ToolVenueTips {
		t.Fatalf("unexpected batch: %+v", action)
	}
	// 超过上限要截断，防止模型一次写十条把成本拉爆。
	var many []string
	for index := 0; index < 6; index++ {
		many = append(many, `{"tool":"search_shows","args":{}}`)
	}
	action, err = parseAction(`{"tools":[` + strings.Join(many, ",") + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(action.Tools) != maxParallelTools {
		t.Fatalf("batch should be capped at %d, got %d", maxParallelTools, len(action.Tools))
	}
}

// ---------- 上下文压缩 ----------

func TestCompactMessagesShrinksContext(t *testing.T) {
	runtime := New(fixtureKit(), nil, nil, nil)
	state := &session{result: &Result{}, emit: nil, names: map[int64]string{}}
	state.result.Steps = []Step{
		{Phase: "tool", Title: "检索符合条件的演出", Detail: "检索到 2 场"},
		{Phase: "observe", Title: "检索到 2 场符合条件的演出", Detail: strings.Repeat("演出信息", 40)},
		{Phase: "tool", Title: "读取演出 #1 的票档"},
	}
	long := strings.Repeat("很长的观察内容", 500)
	messages := []llm.Message{
		{Role: "system", Content: "系统提示"},
		{Role: "user", Content: "用户问题"},
		{Role: "assistant", Content: `{"tool":"search_shows"}`},
		{Role: "user", Content: "工具结果：" + long},
		{Role: "assistant", Content: `{"tool":"show_detail","args":{"show_id":1}}`},
		{Role: "user", Content: "工具结果：" + long},
	}
	compacted := runtime.compact(messages, state)
	if len(compacted) != 4 {
		t.Fatalf("expected 4 messages after compaction, got %d", len(compacted))
	}
	if !strings.Contains(compacted[2].Content, "压缩") || !strings.Contains(compacted[2].Content, "检索符合条件的演出") {
		t.Fatalf("digest missing tool summary://n%s", compacted[2].Content)
	}
	found := false
	for _, step := range state.result.Steps {
		if step.Title == "压缩上下文" {
			found = true
		}
	}
	if !found {
		t.Fatal("compaction should record a step")
	}
}

// ---------- 跑分 ----------

func TestBenchmarkReportsMetrics(t *testing.T) {
	runtime := New(fixtureKit(), nil, nil, nil)
	report := Benchmark(context.Background(), runtime, DefaultBenchCases(), Slots{Party: 2, Budget: 500, BudgetMode: "total"})
	if report.Cases != len(DefaultBenchCases()) {
		t.Fatalf("cases = %d", report.Cases)
	}
	// 没有审批队列时「写操作」用例应被跳过，而不是算失败。
	if report.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", report.Skipped)
	}
	if report.Passed < 6 {
		t.Fatalf("passed = %d, failures=%v", report.Passed, report.Failures)
	}
	if report.ToolHitRate != 1 {
		t.Fatalf("tool hit rate = %.2f, failures=%v", report.ToolHitRate, report.Failures)
	}
	if report.IntentRate < 0.8 {
		t.Fatalf("intent rate = %.2f, failures=%v", report.IntentRate, report.Failures)
	}
	if report.AvgSteps <= 0 || report.AvgMS < 0 {
		t.Fatalf("metrics not filled: %+v", report)
	}
}

func TestBenchmarkWithSinkCoversWriteCase(t *testing.T) {
	sink := &fakeSink{}
	step := 0
	model := &fakeModel{enabled: true,
		complete: func(messages []llm.Message) (string, error) {
			step++
			if step == 1 {
				return `{"intent":"recommend","reason":"想记一下"}`, nil
			}
			if step == 2 {
				return `{"tool":"add_watchlist","args":{"show_id":1}}`, nil
			}
			return `{"final":true}`, nil
		}}
	runtime := New(fixtureKit(), model, nil, nil, WithActionSink(sink))
	cases := []BenchCase{{Name: "写操作", Query: "把雷雨记到清单", ExpectIntent: IntentRecommend, ExpectTool: ToolAddWatchlist, WantPending: 1}}
	report := Benchmark(context.Background(), runtime, cases, Slots{})
	if report.Skipped != 0 || report.Passed != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestLoadBenchCasesFallsBackToDefaults(t *testing.T) {
	cases, err := LoadBenchCases("definitely-not-exist.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != len(DefaultBenchCases()) {
		t.Fatalf("fallback should return defaults, got %d", len(cases))
	}
	file := t.TempDir() + "/cases.json"
	if err := os.WriteFile(file, []byte(`[{"name":"自定义","query":"推荐话剧","expect_intent":"recommend"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err = LoadBenchCases(file)
	if err != nil || len(cases) != 1 || cases[0].Name != "自定义" {
		t.Fatalf("custom cases failed: %v %+v", err, cases)
	}
}

// ---------- 无模型也能演示写操作 ----------

func TestRuleModeProposesWatchlist(t *testing.T) {
	sink := &fakeSink{}
	runtime := New(fixtureKit(), nil, nil, nil, WithActionSink(sink))
	result, err := runtime.Run(context.Background(), Request{
		Query: "把雷雨记到我的观演清单", SessionID: "s-rule-w", OwnerID: "owner-rule",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent != IntentWatchlist {
		t.Fatalf("intent = %s", result.Intent)
	}
	if len(sink.proposed) != 1 || sink.proposed[0].Kind != ActionWatchlist {
		t.Fatalf("expected watchlist proposal, got %+v", sink.proposed)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("result actions = %+v", result.Actions)
	}
	if !strings.Contains(result.Answer, "需要你确认") {
		t.Fatalf("answer must ask for confirmation://n%s", result.Answer)
	}
}

func TestRuleModeProposesPreference(t *testing.T) {
	sink := &fakeSink{}
	runtime := New(fixtureKit(), nil, nil, nil, WithActionSink(sink))
	result, err := runtime.Run(context.Background(), Request{
		Query: "以后都看上海的", SessionID: "s-rule-p", OwnerID: "owner-rule",
		Slots: Slots{Party: 1, City: "上海"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent != IntentPreference {
		t.Fatalf("intent = %s", result.Intent)
	}
	if len(sink.proposed) != 1 || sink.proposed[0].Kind != ActionPreference || sink.proposed[0].Key != "city" {
		t.Fatalf("expected city preference proposal, got %+v", sink.proposed)
	}
}
