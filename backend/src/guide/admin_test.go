package guide

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
	"github.com/LYP-leo/echo-shopping-guide/backend/src/ticketing"
)

func adminFixture(t *testing.T, token string) *Handler {
	t.Helper()
	h, _ := New(richFixture(), llm.Config{})
	h.prompt = &promptStore{path: filepath.Join(t.TempDir(), "guide-prompt.json"), text: defaultAssistantPrompt, version: 1}
	h.admin = adminSettings{token: token}
	h.manageOn = token != ""
	return h
}

func adminRequest(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-Guide-Admin-Token", token)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, request)
	return out
}

func TestAdminRequiresConfiguredTokenAndValidHeader(t *testing.T) {
	disabled := adminFixture(t, "")
	if out := adminRequest(t, disabled.AdminHandler(), "GET", "/api/v1/guide-admin/overview", "anything", ""); out.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin without token returned %d: %s", out.Code, out.Body.String())
	}

	h := adminFixture(t, "s3cret")
	admin := h.AdminHandler()
	var session struct{ Enabled, Authenticated bool }
	out := adminRequest(t, admin, "GET", "/api/v1/guide-admin/session", "", "")
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &session) != nil || !session.Enabled || session.Authenticated {
		t.Fatalf("session gate wrong: %s", out.Body.String())
	}
	if out := adminRequest(t, admin, "GET", "/api/v1/guide-admin/overview", "wrong", ""); out.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token returned %d", out.Code)
	}
	out = adminRequest(t, admin, "GET", "/api/v1/guide-admin/overview", "s3cret", "")
	var overview struct {
		Health []healthItem
		Stats  askStatsView
		Model  map[string]any
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &overview) != nil || len(overview.Health) == 0 || overview.Stats.Total != 0 {
		t.Fatalf("overview failed: %s", out.Body.String())
	}
	if _, leaked := overview.Model["api_key"]; leaked {
		t.Fatal("model credentials must never reach the console")
	}
}

func TestAdminPromptSavesVersionsAndRestoresDefault(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":"太短","confirm":"token"}`); out.Code != http.StatusBadRequest {
		t.Fatalf("short prompt accepted: %s", out.Body.String())
	}
	custom := strings.Repeat("只根据服务端事实回答，不虚构演出、票价与余票。", 6)
	var saved struct{ Version int }
	out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":`+strconvQuote(custom)+`,"confirm":"token"}`)
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &saved) != nil || saved.Version != 2 {
		t.Fatalf("prompt save failed: %s", out.Body.String())
	}
	if h.prompt.system() != custom {
		t.Fatal("assistant did not pick up the saved prompt")
	}
	var restored struct{ Version int }
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"restore":true,"confirm":"token"}`); out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &restored) != nil || restored.Version != 3 {
		t.Fatalf("restore failed: %s", out.Body.String())
	}
	if h.prompt.system() != defaultAssistantPrompt {
		t.Fatal("restore did not return the shipped prompt")
	}
}

func TestAdminPromptRequiresSecondConfirmation(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	custom := strings.Repeat("只根据服务端事实回答，不虚构演出、票价与余票。", 6)
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":`+strconvQuote(custom)+`}`); out.Code != http.StatusForbidden {
		t.Fatalf("save without confirm returned %d", out.Code)
	}
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":`+strconvQuote(custom)+`,"confirm":"wrong"}`); out.Code != http.StatusForbidden {
		t.Fatalf("save with wrong confirm returned %d", out.Code)
	}
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"restore":true}`); out.Code != http.StatusForbidden {
		t.Fatalf("restore without confirm returned %d", out.Code)
	}
	if h.prompt.version != 1 {
		t.Fatalf("rejected attempts must not bump the version: %d", h.prompt.version)
	}
}

func TestAdminPromptHistorySupportsRollback(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	first := strings.Repeat("第一版提示词，只根据服务端事实回答，不虚构数据。", 4)
	second := strings.Repeat("第二版提示词，调整了语气与样本提醒方式。", 4)
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":`+strconvQuote(first)+`,"confirm":"token"}`); out.Code != 200 {
		t.Fatalf("first save failed: %s", out.Body.String())
	}
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"text":`+strconvQuote(second)+`,"confirm":"token"}`); out.Code != 200 {
		t.Fatalf("second save failed: %s", out.Body.String())
	}
	var doc struct {
		History []struct {
			Version int    `json:"version"`
			Action  string `json:"action"`
			Text    string `json:"text"`
		} `json:"history"`
	}
	if out := adminRequest(t, admin, "GET", "/api/v1/guide-admin/prompt", "token", ""); out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &doc) != nil {
		t.Fatalf("prompt read failed: %s", out.Body.String())
	}
	if len(doc.History) < 2 || doc.History[0].Action != "update" || doc.History[0].Version != 3 {
		t.Fatalf("history wrong: %+v", doc.History)
	}
	// Roll back to v2 (the first custom text); rollback itself becomes v4.
	var rolled struct {
		Version int
		Action  string
	}
	out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"version":2,"confirm":"token"}`)
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &rolled) != nil || rolled.Version != 4 || rolled.Action != "rollback" {
		t.Fatalf("rollback failed: %s", out.Body.String())
	}
	if h.prompt.system() != first {
		t.Fatal("rollback did not restore the requested version text")
	}
	if out := adminRequest(t, admin, "PUT", "/api/v1/guide-admin/prompt", "token", `{"version":99,"confirm":"token"}`); out.Code != http.StatusBadRequest {
		t.Fatalf("unknown version accepted: %d", out.Code)
	}
}

func TestAdminEvalRunsEveryCaseWithoutModel(t *testing.T) {
	h := adminFixture(t, "token")
	out := adminRequest(t, h.AdminHandler(), "POST", "/api/v1/guide-admin/eval", "token", "")
	if out.Code != 200 {
		t.Fatalf("eval failed: %s", out.Body.String())
	}
	var report evalReport
	if err := json.Unmarshal(out.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Total != len(evalCases()) || len(report.Cases) != report.Total || report.DurationMS < 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, entry := range report.Cases {
		if len(entry.Checks) == 0 {
			t.Fatalf("case %q has no checks", entry.Name)
		}
		for _, check := range entry.Checks {
			if check.Got == "" {
				t.Fatalf("case %q check %q has no evidence", entry.Name, check.Label)
			}
			if strings.Contains(strings.ToLower(check.Got), "s3cret") {
				t.Fatal("admin token leaked into evaluation output")
			}
		}
	}
}

func TestAssistStatsFeedTheConsole(t *testing.T) {
	h := adminFixture(t, "token")
	request := httptest.NewRequest("POST", "/api/v1/ticketing/assist", strings.NewReader(`{"message":"两个人，总预算 600 元，推荐能买票的演出"}`))
	request.Header.Set("Content-Type", "application/json")
	h.assist(httptest.NewRecorder(), request)
	view := h.stats.view(h.started)
	if view.Total != 1 || view.Catalog != 1 || len(view.Recent) != 1 || view.Recent[0].Question == "" || len(view.Recent[0].Question) > 200 {
		t.Fatalf("unexpected stats: %+v", view)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAdminCatalogAggregatesCitiesAndTicketTypes(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	if out := adminRequest(t, admin, "GET", "/api/v1/guide-admin/catalog", "", ""); out.Code != http.StatusUnauthorized {
		t.Fatalf("catalog without token returned %d", out.Code)
	}
	out := adminRequest(t, admin, "GET", "/api/v1/guide-admin/catalog", "token", "")
	if out.Code != 200 {
		t.Fatalf("catalog failed: %s", out.Body.String())
	}
	var data catalogAnalytics
	if err := json.Unmarshal(out.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	cities := map[string]cityRow{}
	for _, row := range data.Cities {
		cities[row.City] = row
	}
	if cities["上海"].Shows != 2 || cities["北京"].Shows != 1 {
		t.Fatalf("city split wrong: %+v", data.Cities)
	}
	if cities["上海"].Rated != 2 || cities["上海"].RatingCount != 11 || cities["北京"].RatingCount != 0 {
		t.Fatalf("city ratings wrong: %+v", cities["上海"])
	}
	if data.Summary.Tiers != 9 || data.Summary.SoldOut != 3 || data.Summary.Purchasable != 4 || data.Summary.Stock != 56 {
		t.Fatalf("ticket summary wrong: %+v", data.Summary)
	}
	if len(data.Tiers) != 9 || data.Tiers[0].Show == "" || data.Tiers[0].City == "" {
		t.Fatalf("tier rows missing context: %+v", data.Tiers[0])
	}
	totalBand := 0
	for _, band := range data.Bands {
		totalBand += band.Count
	}
	if totalBand != 9 {
		t.Fatalf("price bands lost tiers: %+v", data.Bands)
	}
	if data.Summary.MissingRules == 0 {
		t.Fatal("empty ticket rules should be reported as a data gap")
	}
}

func TestCityOfSkipsDistrictValues(t *testing.T) {
	show := ticketing.Show{Area: "黄浦区", Address: "上海大剧院"}
	if got := cityOf(show, []string{"上海", "黄浦区"}); got != "上海" {
		t.Fatalf("district won the city match: %q", got)
	}
	if got := cityOf(ticketing.Show{Area: "未知", Address: "某剧场"}, []string{"上海"}); got != "未标注城市" {
		t.Fatalf("unmatched show should be labelled, got %q", got)
	}
}

func TestAdminAskRequiresTokenAndValidatesMessage(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	if out := adminRequest(t, admin, "POST", "/api/v1/guide-admin/ask", "", `{"message":"哪些演出没有票档"}`); out.Code != http.StatusUnauthorized {
		t.Fatalf("ask without token returned %d", out.Code)
	}
	for _, body := range []string{`{}`, `{"message":""}`, `{"message":"` + strings.Repeat("长", 301) + `"}`} {
		if out := adminRequest(t, admin, "POST", "/api/v1/guide-admin/ask", "token", body); out.Code != http.StatusBadRequest {
			t.Fatalf("invalid message accepted: %s", out.Body.String())
		}
	}
}

func TestAdminAskDataIntentListsRealGaps(t *testing.T) {
	price := 280.0
	snapshot := ticketing.CatalogSnapshot{Items: []ticketing.Show{
		{ID: 1, Name: "有票档的话剧", Address: "上海大剧院", MinPrice: &price, Rating: ticketing.RatingSummary{Average: 4.6, Count: 12, Available: true, Complete: true}},
		{ID: 2, Name: "没有票档的音乐会", Address: "北京音乐厅", Rating: ticketing.RatingSummary{Available: true, Complete: true}},
	}}
	data := &catalogAnalytics{
		Tiers:   []tierRow{{Show: "有票档的话剧", Title: "普通票", Price: 280, RulesMissing: true}},
		Summary: ticketSummary{Tiers: 1, MissingRules: 1},
		Cities:  []cityRow{{City: "上海", Shows: 1, Active: 1, Tiers: 1, RatingCount: 12, Average: 4.6}},
		Bands:   priceBands(),
	}
	answer, rows, _ := answerDataIntent("tickets_missing", "哪些演出没有票档", snapshot, data)
	if !strings.Contains(answer, "1 场") || len(rows) != 1 || rows[0].Title != "没有票档的音乐会" {
		t.Fatalf("gap report wrong: %s %+v", answer, rows)
	}
	if !strings.Contains(strings.Join(rows[0].Tags, ","), "无法购票") {
		t.Fatalf("missing ticket gap tag: %+v", rows[0])
	}

	answer, rows, _ = answerDataIntent("rules_missing", "哪些票档缺退改规则", snapshot, data)
	if !strings.Contains(answer, "退改规则") || len(rows) != 1 || !strings.Contains(rows[0].Value, "1 / 1") {
		t.Fatalf("rules report wrong: %s %+v", answer, rows)
	}

	answer, _, _ = answerDataIntent("cities", "城市分布怎么样", snapshot, data)
	if !strings.Contains(answer, "1 个城市") {
		t.Fatalf("city report wrong: %s", answer)
	}
}

func TestAdminAskSimulatesVisitorRequestWithPlanner(t *testing.T) {
	h := adminFixture(t, "token")
	out := adminRequest(t, h.AdminHandler(), "POST", "/api/v1/guide-admin/ask", "token", `{"message":"周六，两个人，总预算 600 元，推荐能买票的演出"}`)
	if out.Code != 200 {
		t.Fatalf("ask failed: %s", out.Body.String())
	}
	var data adminAskResponse
	if err := json.Unmarshal(out.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Kind != "guide" || data.Mode != "catalog" || data.Plan == nil || data.Plan.PartySize != 2 || data.Plan.Budget != 600 {
		t.Fatalf("planner not reused: %+v", data)
	}
	if data.Filters == nil || data.Filters.Budget != 300 || data.Filters.DateFrom == "" {
		t.Fatalf("filters not reported: %+v", data.Filters)
	}
	if len(data.Flags) == 0 || !strings.Contains(strings.Join(data.Flags, ";"), "没有调用模型") {
		t.Fatalf("model usage not disclosed: %+v", data.Flags)
	}
	if data.LatencyMS < 0 {
		t.Fatal("latency missing")
	}
}

func TestAskRowSurfacesDataTags(t *testing.T) {
	entry := comparisonEntry{
		Show:    ticketing.Show{Name: "无评分演出", Area: "黄浦区", Address: "上海大剧院", Lifecycle: "upcoming", Rating: ticketing.RatingSummary{Available: true, Complete: true}, OpenTime: "2099-01-01 20:00"},
		Tickets: []ticketing.TicketTier{{Title: "普通票", Price: 120, Rules: ""}},
	}
	row := askRow(entry)
	tags := strings.Join(row.Tags, ",")
	for _, want := range []string{"无评分", "缺退改规则"} {
		if !strings.Contains(tags, want) {
			t.Fatalf("missing tag %q: %s", want, tags)
		}
	}
	if !strings.Contains(row.Sub, "2099-01-01") {
		t.Fatalf("date missing from row: %s", row.Sub)
	}
}

func TestReviewKeywordsMergeSimilarRuns(t *testing.T) {
	reviews := make([]ticketing.Review, 0, 4)
	for i := 0; i < 4; i++ {
		reviews = append(reviews, ticketing.Review{ID: int64(i + 1), Author: "观众", Score: 4, Content: "舞台效果很好，音响效果也不错，整体体验超预期。"})
	}
	entries := []comparisonEntry{{Show: ticketing.Show{ID: 7}, Reviews: ticketing.ReviewReport{Items: reviews}}}
	keywords := extractKeywords(entries, 5)
	words := keywords[7]
	if len(words) == 0 {
		t.Fatal("no keywords extracted from repeated opinions")
	}
	for _, word := range words {
		if strings.Contains(word, "的") || strings.Contains(word, "很") {
			t.Fatalf("stopword leaked into keywords: %q", words)
		}
	}
	// 音响效果 / 效果很好 / 好效果 this kind of overlapping runs must collapse.
	for i, word := range words {
		for j, other := range words {
			if i != j && strings.Contains(word, other) && word != other {
				t.Fatalf("similar runs not merged: %q vs %q (%v)", word, other, words)
			}
		}
	}
	related := relatedQuotes(entries, "音响效果怎么样", 2)
	if len(related[7]) == 0 || !strings.Contains(related[7][0].Excerpt, "音响") {
		t.Fatalf("query-related quote missing: %+v", related[7])
	}
}

func TestAdminCustomCaseAddRunDelete(t *testing.T) {
	h := adminFixture(t, "token")
	admin := h.AdminHandler()
	out := adminRequest(t, admin, "POST", "/api/v1/guide-admin/cases", "token", `{"message":"两个人，总预算 600 元，推荐能买票的演出","expect":["party_size=2","budget=300","bogus=1"]}`)
	if out.Code != http.StatusBadRequest {
		t.Fatalf("bogus expectation accepted: %s", out.Body.String())
	}
	out = adminRequest(t, admin, "POST", "/api/v1/guide-admin/cases", "token", `{"message":"两个人，总预算 600 元，推荐能买票的演出","expect":["party_size=2","budget=300"]}`)
	if out.Code != 200 {
		t.Fatalf("case add failed: %s", out.Body.String())
	}
	var added customCase
	if json.Unmarshal(out.Body.Bytes(), &added) != nil || added.ID == "" {
		t.Fatalf("case id missing: %s", out.Body.String())
	}
	report := h.runEval(context.Background())
	var found bool
	for _, entry := range report.Cases {
		if strings.HasPrefix(entry.Name, "自定义：") {
			found = true
		}
	}
	if !found {
		t.Fatal("custom case did not run inside eval")
	}
	if out := adminRequest(t, admin, "DELETE", "/api/v1/guide-admin/cases/"+added.ID, "token", ""); out.Code != 200 {
		t.Fatalf("case delete failed: %s", out.Body.String())
	}
	if out := adminRequest(t, admin, "DELETE", "/api/v1/guide-admin/cases/"+added.ID, "token", ""); out.Code != http.StatusNotFound {
		t.Fatalf("double delete should 404: %d", out.Code)
	}
}

func TestAdminTokenHashOnlyMatches(t *testing.T) {
	digest := sha256.Sum256([]byte("rotated-token"))
	settings := adminSettings{hashes: [][]byte{digest[:]}}
	if !settings.enabled() || !settings.matches("rotated-token") {
		t.Fatal("hash-only token should authenticate")
	}
	if settings.matches("rotated-token ") == true && !settings.matches(" rotated-token ") {
		t.Fatal("candidate trimming inconsistent")
	}
	if settings.matches("wrong") {
		t.Fatal("wrong token accepted")
	}
}

func TestStatsFileSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guide-stats.jsonl")
	stats := newAskStatsWithFile(path)
	stats.record(askRecord{At: "t1", Question: "问题一", Mode: "catalog", LatencyMS: 10, Items: 1})
	stats.record(askRecord{At: "t2", Question: "问题二", Mode: "ai", LatencyMS: 20, Items: 2})
	reloaded := newAskStatsWithFile(path)
	if reloaded.total != 2 || reloaded.ai != 1 || reloaded.catalog != 1 || len(reloaded.recent) != 2 {
		t.Fatalf("stats did not survive restart: %+v", reloaded.view("now"))
	}
}

func TestDraftCSVQuotesAndBOM(t *testing.T) {
	draft := draftShow{Name: "测试「引用」演出", Tiers: []draftTier{{Title: "普通票", Price: 180, Stock: 20}}}
	csv := draftCSV(draft)
	if !strings.HasPrefix(csv, "\uFEFF") {
		t.Fatal("CSV missing UTF-8 BOM for Excel")
	}
	if !strings.Contains(csv, "\"测试「引用」演出\"") {
		t.Fatal("cells are not quoted")
	}
	if !strings.Contains(csv, "180.00") {
		t.Fatal("price missing from CSV")
	}
}
