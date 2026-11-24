package guide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/agent"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// sseEvents 把 SSE 响应体拆成事件列表，测试只关心「有哪些事件、内容对不对」。
func sseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	events := []map[string]any{}
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "data: ")), &event); err != nil {
			t.Fatalf("bad event %q: %v", block, err)
		}
		events = append(events, event)
	}
	return events
}

func newAgentTestHandler(t *testing.T, fixture *discoveryCatalog) (*Handler, http.Handler) {
	t.Helper()
	// 轨迹与状态写到临时目录，避免测试污染程序目录。
	t.Setenv("GUIDE_AGENT_TRACE", filepath.Join(t.TempDir(), "agent-runs.jsonl"))
	t.Setenv("GUIDE_AGENT_STATE", filepath.Join(t.TempDir(), "agent-state.json"))
	handler, _ := New(fixture, llm.Config{})
	return handler, handler.AgentStreamHandler()
}

func TestAgentStreamRunsToolsAndReturnsCards(t *testing.T) {
	fixture := richFixture()
	_, stream := newAgentTestHandler(t, fixture)

	body := `{"message":"推荐一场能买票的话剧","session_id":"t-agent-1","filters":{"category":"话剧"}}`
	response := httptest.NewRecorder()
	stream.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/stream", strings.NewReader(body)))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	events := sseEvents(t, response.Body.String())
	kinds := map[string]int{}
	var final map[string]any
	for _, event := range events {
		kind, _ := event["type"].(string)
		kinds[kind]++
		if kind == "final" {
			final = event
		}
	}
	if kinds["agent_start"] != 1 || kinds["step"] == 0 || kinds["meta"] != 1 || kinds["final"] != 1 {
		t.Fatalf("unexpected event mix: %v\n%s", kinds, response.Body.String())
	}
	if final == nil {
		t.Fatal("missing final event")
	}
	if mode, _ := final["mode"].(string); mode != "agent-rule" {
		t.Fatalf("mode = %v", final["mode"])
	}
	answer, _ := final["answer"].(string)
	if !strings.Contains(answer, "未来话剧") {
		t.Fatalf("answer should mention the upcoming show:\n%s", answer)
	}
	// 已结束的场次不能被推荐成可购。
	if strings.Contains(answer, "历史话剧") {
		t.Fatalf("ended show leaked into recommendation:\n%s", answer)
	}
	items, _ := final["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected recommendation cards: %s", response.Body.String())
	}

	// 步骤必须包含规划、工具、观察、自检四类，否则前端时间线不完整。
	phases := map[string]bool{}
	for _, event := range events {
		if event["type"] != "step" {
			continue
		}
		step, _ := event["step"].(map[string]any)
		if phase, ok := step["phase"].(string); ok {
			phases[phase] = true
		}
	}
	for _, phase := range []string{"planner", "tool", "observe", "check"} {
		if !phases[phase] {
			t.Fatalf("missing phase %s: %v", phase, phases)
		}
	}
}

func TestAgentSessionKeepsSlotsAcrossTurns(t *testing.T) {
	fixture := richFixture()
	_, stream := newAgentTestHandler(t, fixture)

	first := `{"message":"两个人 500 元想看话剧","session_id":"t-agent-2","filters":{"category":"话剧"}}`
	response := httptest.NewRecorder()
	stream.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/stream", strings.NewReader(first)))
	if response.Code != 200 {
		t.Fatalf("first turn failed: %s", response.Body.String())
	}

	// 第二轮不提预算与人数，服务端记忆应把 2 人 / 500 元带回来。
	second := `{"message":"那有没有便宜点的","session_id":"t-agent-2"}`
	response = httptest.NewRecorder()
	stream.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/stream", strings.NewReader(second)))
	if response.Code != 200 {
		t.Fatalf("second turn failed: %s", response.Body.String())
	}
	var final map[string]any
	for _, event := range sseEvents(t, response.Body.String()) {
		if event["type"] == "final" {
			final = event
		}
	}
	plan, _ := final["plan"].(map[string]any)
	if plan == nil || plan["party_size"].(float64) != 2 || plan["budget"].(float64) != 500 {
		t.Fatalf("slots not carried: %+v", plan)
	}
}

func TestAgentEndpointFallsBackToJSONWithoutFlush(t *testing.T) {
	fixture := richFixture()
	handler, _ := newAgentTestHandler(t, fixture)
	_, mux := New(fixture, llm.Config{})

	// 普通 JSON 版本（非 SSE）：给不支持流式的客户端用。
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent", strings.NewReader(`{"message":"推荐话剧","filters":{"category":"话剧"}}`)))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Answer string           `json:"answer"`
		Mode   string           `json:"mode"`
		Steps  []map[string]any `json:"steps"`
		Items  []any            `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Answer == "" || payload.Mode != "agent-rule" || len(payload.Steps) == 0 || len(payload.Items) == 0 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if handler.agent == nil {
		t.Fatal("handler should carry an agent runtime")
	}
}

func TestAgentConfigAdvertisesTools(t *testing.T) {
	fixture := richFixture()
	handler, _ := newAgentTestHandler(t, fixture)
	_, mux := New(fixture, llm.Config{})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/ticketing/config", nil))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var payload struct {
		Agent struct {
			Enabled bool     `json:"enabled"`
			Mode    string   `json:"mode"`
			Tools   []string `json:"tools"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Agent.Enabled || payload.Agent.Mode != "agent-rule" || len(payload.Agent.Tools) < 5 {
		t.Fatalf("config should advertise agent: %+v", payload.Agent)
	}
	_ = handler
}

func TestAgentWithoutRichCatalogIsUnavailable(t *testing.T) {
	t.Setenv("GUIDE_AGENT_TRACE", filepath.Join(t.TempDir(), "runs.jsonl"))
	handler, mux := New(&testCatalog{}, llm.Config{})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent", strings.NewReader(`{"message":"推荐话剧"}`)))
	if response.Code != 503 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private upstream") || !strings.Contains(response.Body.String(), "Echo") {
		t.Fatalf("message should be actionable: %s", response.Body.String())
	}
	_ = handler
}

func TestAdminAgentTracesRequireTokenAndReturnSteps(t *testing.T) {
	fixture := richFixture()
	t.Setenv("GUIDE_ADMIN_TOKEN", "test-token")
	tracePath := filepath.Join(t.TempDir(), "agent-runs.jsonl")
	t.Setenv("GUIDE_AGENT_TRACE", tracePath)
	handler, _ := New(fixture, llm.Config{})

	// 先跑一次，产生轨迹。
	stream := handler.AgentStreamHandler()
	stream.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/stream", strings.NewReader(`{"message":"推荐话剧","session_id":"t-admin"}`)))

	admin := handler.AdminHandler()
	unauthorized := httptest.NewRecorder()
	admin.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/runs", nil))
	if unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusForbidden {
		t.Fatalf("expected auth failure, got %d %s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/runs", nil)
	request.Header.Set("X-Guide-Admin-Token", "test-token")
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var list struct {
		Items []struct {
			RunID string           `json:"run_id"`
			Tools []string         `json:"tools"`
			Steps []map[string]any `json:"steps"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 || len(list.Items[0].Tools) == 0 || len(list.Items[0].Steps) == 0 {
		t.Fatalf("trace list should contain tool calls and steps: %+v", list)
	}

	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/runs/"+list.Items[0].RunID, nil)
	detailRequest.Header.Set("X-Guide-Admin-Token", "test-token")
	detail := httptest.NewRecorder()
	admin.ServeHTTP(detail, detailRequest)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), list.Items[0].RunID) {
		t.Fatalf("detail failed: %d %s", detail.Code, detail.Body.String())
	}

	// 轨迹文件应真的落盘，重启后可回看。
	if data, err := os.ReadFile(tracePath); err != nil || !strings.Contains(string(data), list.Items[0].RunID) {
		t.Fatalf("trace file not persisted: %v", err)
	}
}

// ---------- 写操作：审批、幂等、清单 ----------

func newWriteTestHandler(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	t.Setenv("GUIDE_AGENT_TRACE", filepath.Join(t.TempDir(), "agent-runs.jsonl"))
	t.Setenv("GUIDE_AGENT_STATE", filepath.Join(t.TempDir(), "agent-state.json"))
	handler, mux := New(richFixture(), llm.Config{})
	return handler, mux
}

func TestAgentWriteActionConfirmFlow(t *testing.T) {
	handler, mux := newWriteTestHandler(t)
	action, err := handler.agentState.Propose(context.Background(), "owner-9", agent.PendingAction{
		Kind: agent.ActionWatchlist, ShowID: 2, ShowName: "未来话剧",
		Summary: "把《未来话剧》加入观演清单", Risk: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if action.Status != agent.ActionPending || action.ID == "" {
		t.Fatalf("proposal should be pending with id: %+v", action)
	}

	// 确认前：清单为空，动作仍是 pending。
	if items := handler.agentState.Watchlist("owner-9"); len(items) != 0 {
		t.Fatalf("watchlist should be empty before confirm: %+v", items)
	}

	confirm := httptest.NewRecorder()
	mux.ServeHTTP(confirm, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/actions/"+action.ID+"/confirm", nil))
	if confirm.Code != 200 {
		t.Fatalf("confirm failed: %d %s", confirm.Code, confirm.Body.String())
	}
	var payload struct {
		Action struct {
			Status string `json:"status"`
			Result string `json:"result"`
		} `json:"action"`
		Watchlist []struct {
			ShowID int64 `json:"show_id"`
		} `json:"watchlist"`
	}
	if err := json.Unmarshal(confirm.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Action.Status != agent.ActionConfirmed || payload.Action.Result == "" {
		t.Fatalf("action not confirmed: %+v", payload.Action)
	}
	if len(payload.Watchlist) != 1 || payload.Watchlist[0].ShowID != 2 {
		t.Fatalf("watchlist not written: %+v", payload.Watchlist)
	}

	// 幂等：再确认一次返回 409 + 当前状态，不重复写入。
	again := httptest.NewRecorder()
	mux.ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/actions/"+action.ID+"/confirm", nil))
	if again.Code != http.StatusConflict {
		t.Fatalf("second confirm should be 409, got %d", again.Code)
	}
	if items := handler.agentState.Watchlist("owner-9"); len(items) != 1 {
		t.Fatalf("duplicate confirm must not duplicate rows: %+v", items)
	}

	// 拒绝另一条提案：状态 rejected，清单不变。
	second, err := handler.agentState.Propose(context.Background(), "owner-9", agent.PendingAction{
		Kind: agent.ActionWatchlist, ShowID: 1, Summary: "把《历史话剧》加入观演清单",
	})
	if err != nil {
		t.Fatal(err)
	}
	reject := httptest.NewRecorder()
	mux.ServeHTTP(reject, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/actions/"+second.ID+"/reject", nil))
	if reject.Code != 200 || !strings.Contains(reject.Body.String(), agent.ActionRejected) {
		t.Fatalf("reject failed: %d %s", reject.Code, reject.Body.String())
	}
	if items := handler.agentState.Watchlist("owner-9"); len(items) != 1 {
		t.Fatalf("rejected action must not write: %+v", items)
	}

	// 不存在的编号：404，而不是 500。
	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/actions/nope/confirm", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing action should be 404, got %d", missing.Code)
	}

	// 清单接口按设备返回。
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/ticketing/watchlist?session_id=owner-9", nil))
	if list.Code != 200 || !strings.Contains(list.Body.String(), "未来话剧") {
		t.Fatalf("watchlist endpoint failed: %s", list.Body.String())
	}
}

func TestAgentActionDeduplicatesPending(t *testing.T) {
	handler, _ := newWriteTestHandler(t)
	first, err := handler.agentState.Propose(context.Background(), "owner-10", agent.PendingAction{
		Kind: agent.ActionWatchlist, ShowID: 2, Summary: "加入观演清单",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := handler.agentState.Propose(context.Background(), "owner-10", agent.PendingAction{
		Kind: agent.ActionWatchlist, ShowID: 2, Summary: "加入观演清单",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same target should reuse pending action: %s vs %s", first.ID, second.ID)
	}
}

func TestAgentPreferenceFeedsNextQuery(t *testing.T) {
	handler, _ := newWriteTestHandler(t)
	action, err := handler.agentState.Propose(context.Background(), "owner-11", agent.PendingAction{
		Kind: agent.ActionPreference, Key: "city", Value: "上海", Summary: "记住常驻城市：上海",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.agentState.Decide(action.ID, true); err != nil {
		t.Fatal(err)
	}

	// 用户没说城市时，长期偏好补进来。
	request, err := handler.agentRequest(context.Background(), assistRequest{Message: "推荐一场话剧", ProfileID: "owner-11"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Slots.City != "上海" {
		t.Fatalf("profile city not applied: %+v", request.Slots)
	}

	// 用户明确说北京时，本轮条件优先于偏好。
	override, err := handler.agentRequest(context.Background(), assistRequest{Message: "北京的话剧", ProfileID: "owner-11"})
	if err != nil {
		t.Fatal(err)
	}
	if override.Slots.City != "北京" {
		t.Fatalf("explicit city should win: %+v", override.Slots)
	}
}

func TestAdminAgentActionsRequireToken(t *testing.T) {
	t.Setenv("GUIDE_ADMIN_TOKEN", "act-token")
	handler, _ := newWriteTestHandler(t)
	if _, err := handler.agentState.Propose(context.Background(), "owner-12", agent.PendingAction{
		Kind: agent.ActionPreference, Key: "category", Value: "话剧", Summary: "记住偏好：话剧",
	}); err != nil {
		t.Fatal(err)
	}
	admin := handler.AdminHandler()

	unauthorized := httptest.NewRecorder()
	admin.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/actions", nil))
	if unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusForbidden {
		t.Fatalf("expected auth failure, got %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/actions", nil)
	request.Header.Set("X-Guide-Admin-Token", "act-token")
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "记住偏好") {
		t.Fatalf("admin actions failed: %d %s", response.Code, response.Body.String())
	}
}

// ---------- 断点续跑与 RAG 检索评估 ----------

func newResumeTestHandler(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	t.Setenv("GUIDE_AGENT_TRACE", filepath.Join(t.TempDir(), "agent-runs.jsonl"))
	t.Setenv("GUIDE_AGENT_STATE", filepath.Join(t.TempDir(), "agent-state.json"))
	t.Setenv("GUIDE_AGENT_CHECKPOINTS", filepath.Join(t.TempDir(), "agent-checkpoints.jsonl"))
	handler, mux := New(richFixture(), llm.Config{})
	return handler, mux
}

// resumableSnapshot 造一个「工具事实已齐、停在成文前」的快照。
// 刻意不依赖真实超时：这里要验证的是续跑链路本身，中断时机由 agent 包的单测覆盖。
func resumableSnapshot() agent.Checkpoint {
	price := 180.0
	return agent.Checkpoint{
		RunID: "run-resume-1", SessionID: "sess-r1", OwnerID: "owner-r1",
		Query: "推荐一场话剧", Engine: "rule", Stage: "answer", Status: "resumable",
		Plan:  agent.Plan{Intent: agent.IntentRecommend, Tools: []string{agent.ToolSearchShows}},
		Slots: agent.Slots{Party: 2}, Round: 2,
		Evidence: agent.EvidenceState{
			Shows:     []agent.Show{{ID: 1, Name: "雷雨", TypeName: "话剧", Area: "上海市", Rating: 4.5, RatingCount: 6, MinPrice: &price}},
			Tools:     []string{agent.ToolSearchShows},
			ShowIDs:   []int64{1},
			Available: true,
		},
	}
}

func TestAgentResumeEndpointReturnsResultAndClearsSnapshot(t *testing.T) {
	handler, mux := newResumeTestHandler(t)
	handler.agentCheckpoints.Save(resumableSnapshot())

	// 快照列表能按设备看到这次中断的运行。
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/ticketing/agent/checkpoints?profile_id=owner-r1", nil)
	listResponse := httptest.NewRecorder()
	mux.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != 200 || !strings.Contains(listResponse.Body.String(), "run-resume-1") {
		t.Fatalf("checkpoint list failed: %d %s", listResponse.Code, listResponse.Body.String())
	}
	// 别的设备看不到。
	otherRequest := httptest.NewRequest(http.MethodGet, "/api/v1/ticketing/agent/checkpoints?profile_id=someone-else", nil)
	otherResponse := httptest.NewRecorder()
	mux.ServeHTTP(otherResponse, otherRequest)
	if strings.Contains(otherResponse.Body.String(), "run-resume-1") {
		t.Fatalf("checkpoints must be scoped by owner: %s", otherResponse.Body.String())
	}

	// 续跑：不重新查询，直接用快照里的事实成文。
	resume := httptest.NewRecorder()
	mux.ServeHTTP(resume, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/resume/run-resume-1", nil))
	if resume.Code != 200 {
		t.Fatalf("resume failed: %d %s", resume.Code, resume.Body.String())
	}
	var payload struct {
		Answer      string           `json:"answer"`
		ResumedFrom string           `json:"resumed_from"`
		Items       []map[string]any `json:"items"`
		Mode        string           `json:"mode"`
	}
	if err := json.Unmarshal(resume.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ResumedFrom != "run-resume-1" || payload.Answer == "" {
		t.Fatalf("resume result incomplete: %+v", payload)
	}
	if !strings.Contains(payload.Answer, "雷雨") {
		t.Fatalf("resumed answer should reuse evidence, got: %s", payload.Answer)
	}
	if len(payload.Items) == 0 {
		t.Fatal("resumed result should carry show cards")
	}

	// 跑完即清：同一条 run_id 不能再续，避免「跑完之后又覆盖一遍答案」。
	again := httptest.NewRecorder()
	mux.ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/resume/run-resume-1", nil))
	if again.Code != http.StatusNotFound {
		t.Fatalf("second resume should be 404, got %d", again.Code)
	}

	afterList := httptest.NewRecorder()
	mux.ServeHTTP(afterList, listRequest)
	if strings.Contains(afterList.Body.String(), "run-resume-1") {
		t.Fatal("completed checkpoint must be removed from list")
	}
}

func TestAdminAgentCheckpointsAndRagEvalRequireToken(t *testing.T) {
	t.Setenv("GUIDE_ADMIN_TOKEN", "rag-token")
	handler, _ := newResumeTestHandler(t)
	handler.agentCheckpoints.Save(resumableSnapshot())
	admin := handler.AdminHandler()

	unauthorized := httptest.NewRecorder()
	admin.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/checkpoints", nil))
	if unauthorized.Code == http.StatusOK {
		t.Fatalf("checkpoints must require token, got %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/guide-admin/agent/checkpoints", nil)
	request.Header.Set("X-Guide-Admin-Token", "rag-token")
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "run-resume-1") {
		t.Fatalf("admin checkpoints failed: %d %s", response.Code, response.Body.String())
	}

	eval := httptest.NewRecorder()
	evalRequest := httptest.NewRequest(http.MethodPost, "/api/v1/guide-admin/agent/rag-eval", nil)
	evalRequest.Header.Set("X-Guide-Admin-Token", "rag-token")
	admin.ServeHTTP(eval, evalRequest)
	if eval.Code != 200 {
		t.Fatalf("rag eval failed: %d %s", eval.Code, eval.Body.String())
	}
	var payload struct {
		Entries int `json:"entries"`
		Report  struct {
			Embedder string `json:"embedder"`
			Results  []struct {
				Strategy string  `json:"strategy"`
				Recall   float64 `json:"recall_at_k"`
			} `json:"results"`
		} `json:"report"`
	}
	if err := json.Unmarshal(eval.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Entries != 12 {
		t.Fatalf("knowledge entries = %d, want 12", payload.Entries)
	}
	// 部署无论有没有模型 Key，都应给出 keyword 基线 + hybrid 对照。
	strategies := map[string]float64{}
	for _, item := range payload.Report.Results {
		strategies[item.Strategy] = item.Recall
	}
	if _, ok := strategies["keyword"]; !ok {
		t.Fatalf("missing keyword baseline: %+v", payload.Report.Results)
	}
	if _, ok := strategies["hybrid+rerank"]; !ok {
		t.Fatalf("missing hybrid strategy: %+v", payload.Report.Results)
	}
	if strategies["hybrid+rerank"] < strategies["keyword"] {
		t.Fatalf("hybrid should not be worse than keyword baseline: %+v", strategies)
	}
}

func TestAgentResumeStreamEmitsEvents(t *testing.T) {
	handler, _ := newResumeTestHandler(t)
	handler.agentCheckpoints.Save(resumableSnapshot())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ticketing/agent/resume/run-resume-1/stream?run_id=run-resume-1", nil)
	handler.AgentResumeStreamHandler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"step"`) || !strings.Contains(body, `"type":"final"`) {
		t.Fatalf("resume stream should emit step and final events:/n%s", body[:min(600, len(body))])
	}
	if !strings.Contains(body, "resumed_from") {
		t.Fatal("final event should mark resumed_from")
	}
}
