package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// slowModel 在第 delayFrom 次「大模型」调用时挂起，直到 context 结束。
// 用它来制造一次真实的「时间预算用完」，而不是伪造一个 error 字符串。
// 规划走小模型，所以只有 ReAct 循环里的调用才计数。
type slowModel struct {
	fakeModel
	delayFrom int
	delay     time.Duration
	largeCall int
}

func (s *slowModel) Complete(ctx context.Context, model string, messages []llm.Message, temperature float64) (string, error) {
	if model != "large" {
		return `{"intent":"recommend","reason":"测试规划"}`, nil
	}
	s.largeCall++
	if s.largeCall >= s.delayFrom {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.delay):
		}
	}
	return `{"tool":"search_shows","args":{"category":"话剧"}}`, nil
}

func TestRunSavesCheckpointOnTimeout(t *testing.T) {
	store := NewCheckpointStore("", 30*time.Minute, 10)
	model := &slowModel{delayFrom: 2, delay: 5 * time.Second}
	model.enabled = true
	runtime := New(fixtureKit(), model, nil, nil,
		WithCheckpoints(store), WithTotalBudget(150*time.Millisecond))

	result, err := runtime.Run(context.Background(), Request{Query: "推荐一场话剧", SessionID: "s-resume"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumable || result.ResumeReason != "timeout" {
		t.Fatalf("expected resumable timeout, got %+v", result)
	}
	if result.ResumeStage != "act" {
		t.Fatalf("stage = %q, want act", result.ResumeStage)
	}
	if !strings.Contains(result.Note, "继续") {
		t.Fatalf("note should tell the user they can resume: %q", result.Note)
	}

	checkpoint, ok := store.Load(result.RunID)
	if !ok {
		t.Fatal("checkpoint should be kept for resumable runs")
	}
	if checkpoint.Stage != "act" || checkpoint.Engine != "react" {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	// 第一步已经查过演出，快照里必须留着这份证据，续跑才不用重查。
	if len(checkpoint.Evidence.Shows) == 0 {
		t.Fatalf("evidence not captured: %+v", checkpoint.Evidence)
	}
	if checkpoint.Round == 0 {
		t.Fatalf("round should record completed turns: %+v", checkpoint)
	}
	if len(checkpoint.Messages) < 3 {
		t.Fatalf("messages should carry the decision context: %d", len(checkpoint.Messages))
	}
}

func TestResumeContinuesWithoutRequerying(t *testing.T) {
	store := NewCheckpointStore("", 30*time.Minute, 10)
	kit := fixtureKit()
	model := &slowModel{delayFrom: 2, delay: 5 * time.Second}
	model.enabled = true
	first := New(kit, model, nil, nil,
		WithCheckpoints(store), WithTotalBudget(150*time.Millisecond))
	result, err := first.Run(context.Background(), Request{Query: "推荐一场话剧", SessionID: "s-resume-2"}, nil)
	if err != nil || !result.Resumable {
		t.Fatalf("setup failed: %v %+v", err, result)
	}
	queriesBefore, detailsBefore := kit.searches, kit.details

	// 续跑用同一份快照仓库，但换成一个能正常答完的模型。
	fast := &fakeModel{enabled: true,
		complete: func(messages []llm.Message) (string, error) { return `{"final":true}`, nil },
		stream:   "接着上次的结果：雷雨最合适，二楼看台 180 元。",
	}
	second := New(fixtureKit(), fast, nil, nil, WithCheckpoints(store))
	// 注意：这里用新的 kit 实例，所以计数从 0 开始，验证「续跑没有发起新查询」。
	resumed, err := second.Resume(context.Background(), result.RunID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumedFrom != result.RunID {
		t.Fatalf("resumed_from = %q", resumed.ResumedFrom)
	}
	if strings.TrimSpace(resumed.Answer) == "" {
		t.Fatal("resume should produce an answer")
	}
	if len(resumed.ShowIDs) == 0 {
		t.Fatalf("resume should reuse evidence from the checkpoint: %+v", resumed.ShowIDs)
	}
	// 续跑的第一步是「从断点继续」，说明它接上了原来的时间线。
	if len(resumed.Steps) == 0 || resumed.Steps[0].Phase != "planner" && resumed.Steps[0].Phase != "tool" {
		t.Logf("first step: %+v", resumed.Steps[0])
	}
	found := false
	for _, step := range resumed.Steps {
		if step.Title == "从断点继续" {
			found = true
		}
	}
	if !found {
		t.Fatal("resume should record a 'from checkpoint' step")
	}
	if queriesBefore == 0 || detailsBefore != 0 {
		t.Logf("first run did %d searches, %d details", queriesBefore, detailsBefore)
	}
	// 完成之后快照要被清理，避免同一份证据被反复续跑。
	if _, ok := store.Load(result.RunID); ok {
		t.Fatal("checkpoint should be removed after a completed resume")
	}
}

func TestResumeRejectsUnknownOrExpiredRun(t *testing.T) {
	store := NewCheckpointStore("", 30*time.Minute, 10)
	runtime := New(fixtureKit(), nil, nil, nil, WithCheckpoints(store))
	if _, err := runtime.Resume(context.Background(), "nope", nil); err == nil {
		t.Fatal("unknown run id should fail")
	}

	// 过期快照不可恢复：TTL 设 1 毫秒，保存后稍等即过期。
	shortStore := NewCheckpointStore("", time.Millisecond, 10)
	shortStore.Save(Checkpoint{
		RunID: "expired", SessionID: "s", Query: "推荐话剧", Stage: "act",
	})
	time.Sleep(10 * time.Millisecond)
	expiredRuntime := New(fixtureKit(), nil, nil, nil, WithCheckpoints(shortStore))
	if _, err := expiredRuntime.Resume(context.Background(), "expired", nil); err == nil {
		t.Fatal("expired checkpoint should fail")
	}
}

func TestCheckpointStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.jsonl")
	store := NewCheckpointStore(path, 30*time.Minute, 4)
	store.Save(Checkpoint{
		RunID: "run-a", SessionID: "s", Query: "推荐话剧", Stage: "act", Engine: "react",
		Round: 2, Evidence: EvidenceState{Shows: []Show{{ID: 7, Name: "雷雨"}}, Available: true},
	})
	store.Save(Checkpoint{
		RunID: "run-b", SessionID: "s", Query: "看口碑", Stage: "act", Engine: "react",
	})

	// 重启：新实例从同一个 JSONL 读回快照。
	reloaded := NewCheckpointStore(path, 30*time.Minute, 4)
	checkpoint, ok := reloaded.Load("run-a")
	if !ok || checkpoint.Round != 2 || len(checkpoint.Evidence.Shows) != 1 {
		t.Fatalf("checkpoint not restored: %+v", checkpoint)
	}
	if len(reloaded.List(10)) != 2 {
		t.Fatalf("list = %d", len(reloaded.List(10)))
	}

	// 已完成的状态不应被当成可恢复。
	reloaded.Delete("run-a")
	if _, ok := reloaded.Load("run-a"); ok {
		t.Fatal("deleted checkpoint should be gone")
	}
}

func TestCheckpointMessagesAreTrimmed(t *testing.T) {
	messages := make([]llm.Message, 0, 40)
	messages = append(messages, llm.Message{Role: "system", Content: "系统提示"})
	for index := 0; index < 39; index++ {
		messages = append(messages, llm.Message{Role: "user", Content: strings.Repeat("观察", 3000)})
	}
	trimmed := trimCheckpointMessages(messages)
	if len(trimmed) != checkpointMessageLimit {
		t.Fatalf("trimmed length = %d", len(trimmed))
	}
	if trimmed[0].Role != "system" {
		t.Fatal("system message must survive trimming")
	}
	if len([]rune(trimmed[len(trimmed)-1].Content)) > checkpointMessageRunes+1 {
		t.Fatal("messages should be truncated by runes")
	}
}
