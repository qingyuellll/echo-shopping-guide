package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// Checkpoint 是一次运行的中途快照，不是日志：它要让「继续跑」成为可能。
//
// 日志回答「刚才发生了什么」，快照要回答「还差什么没做」——
// 所以除了步骤，还得存下决策上下文（messages）、已收集的事实（evidence）
// 和已经走到第几轮（Round）。
type Checkpoint struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	OwnerID   string `json:"owner_id,omitempty"`
	Query     string `json:"query"`
	Engine    string `json:"engine"` // react | rule
	Stage     string `json:"stage"`  // act | answer | done
	Status    string `json:"status"` // resumable | resumed | done

	Plan  Plan   `json:"plan"`
	Slots Slots  `json:"slots"`
	Mode  string `json:"mode,omitempty"`

	Round    int             `json:"round"`    // 已完成的决策轮，恢复时从下一轮开始
	Failures int             `json:"failures"` // 协议解析失败次数，恢复后继续累计
	Messages []llm.Message   `json:"messages"`
	History  []llm.Message   `json:"history,omitempty"` // 成文阶段要用的对话上下文
	Steps    []Step          `json:"steps"`
	Evidence EvidenceState   `json:"evidence"`
	Actions  []PendingAction `json:"actions,omitempty"`

	Answer string `json:"answer,omitempty"`
	Note   string `json:"note,omitempty"`
	Reason string `json:"reason,omitempty"` // 为什么停在这里：超时 / 断连 / 模型失败

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 快照里的消息要有上限，否则一次超时可能写出几百 KB 的观察原文。
const (
	checkpointMessageLimit = 24
	checkpointMessageRunes = 2000
)

// EvidenceState 是 evidence 的可序列化投影。
// 只保留「回答要用到的事实」，原始 observation 文本不进快照（它已在 messages 里）。
type EvidenceState struct {
	Shows     []Show         `json:"shows,omitempty"`
	Details   []Detail       `json:"details,omitempty"`
	Tips      []KnowledgeHit `json:"tips,omitempty"`
	Budget    *BudgetPlan    `json:"budget,omitempty"`
	Tools     []string       `json:"tools,omitempty"`
	ShowIDs   []int64        `json:"show_ids,omitempty"`
	Available bool           `json:"available"`
}

func (e evidence) snapshot() EvidenceState {
	state := EvidenceState{
		Tips: e.tips, Budget: e.budget,
		Tools: e.tools, ShowIDs: e.showIDs, Available: e.available,
	}
	seen := map[int64]bool{}
	for _, show := range e.shows {
		if show.ID > 0 && seen[show.ID] {
			continue
		}
		seen[show.ID] = true
		state.Shows = append(state.Shows, show)
	}
	// details 按演出 id 去重：同一场被 detail 与 compare 各查一次时只留一份。
	seenDetail := map[int64]bool{}
	for _, detail := range e.details {
		if detail.Show.ID > 0 && seenDetail[detail.Show.ID] {
			continue
		}
		seenDetail[detail.Show.ID] = true
		state.Details = append(state.Details, detail)
	}
	return state
}

func (s EvidenceState) restore() evidence {
	return evidence{
		shows: s.Shows, details: s.Details, tips: s.Tips, budget: s.Budget,
		tools: s.Tools, showIDs: s.ShowIDs, available: s.Available,
	}
}

// CheckpointStore 保存最近若干次可恢复的快照：内存索引 + JSONL 追加落盘。
// 追加写而不是整文件重写，是因为保存发生在每一步之后，重写整个文件会把 IO 放大到 O(n²)。
type CheckpointStore struct {
	mu    sync.Mutex
	items map[string]Checkpoint
	order []string // 最近的 run_id 在前
	limit int
	ttl   time.Duration
	path  string
}

// NewCheckpointStore 构建快照仓库；path 为空表示只存内存（测试用）。
func NewCheckpointStore(path string, ttl time.Duration, limit int) *CheckpointStore {
	if limit <= 0 {
		limit = 50
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	store := &CheckpointStore{
		items: map[string]Checkpoint{}, limit: limit, ttl: ttl,
		path: strings.TrimSpace(path),
	}
	if store.path != "" {
		store.load()
	}
	return store
}

// Save 覆盖写同一 run 的快照。快照数量按 limit 淘汰最旧的，过期由 Load 兜底。
func (s *CheckpointStore) Save(checkpoint Checkpoint) {
	if s == nil || checkpoint.RunID == "" {
		return
	}
	checkpoint.UpdatedAt = time.Now()
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = checkpoint.UpdatedAt
	}
	checkpoint.Messages = trimCheckpointMessages(checkpoint.Messages)

	s.mu.Lock()
	_, existed := s.items[checkpoint.RunID]
	s.items[checkpoint.RunID] = checkpoint
	if !existed {
		s.order = append([]string{checkpoint.RunID}, s.order...)
	}
	// 超出容量：连同 map 一起丢最旧的，避免只删索引留下内存泄漏。
	if len(s.order) > s.limit {
		for _, runID := range s.order[s.limit:] {
			delete(s.items, runID)
		}
		s.order = s.order[:s.limit]
	}
	path := s.path
	s.mu.Unlock()

	if path != "" {
		persistCheckpoint(path, checkpoint)
	}
}

// Load 取一条仍可用的快照。
func (s *CheckpointStore) Load(runID string) (Checkpoint, bool) {
	if s == nil {
		return Checkpoint{}, false
	}
	runID = strings.TrimSpace(runID)
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.items[runID]
	if !ok {
		return Checkpoint{}, false
	}
	if time.Since(checkpoint.UpdatedAt) > s.ttl {
		delete(s.items, runID)
		return Checkpoint{}, false
	}
	return checkpoint, true
}

// List 返回最近 limit 条快照（不含完整答案，供管理端列表用）。
func (s *CheckpointStore) List(limit int) []Checkpoint {
	if s == nil {
		return nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Checkpoint, 0, limit)
	for _, runID := range s.order {
		checkpoint, ok := s.items[runID]
		if !ok || time.Since(checkpoint.UpdatedAt) > s.ttl {
			continue
		}
		if len([]rune(checkpoint.Answer)) > 160 {
			checkpoint.Answer = string([]rune(checkpoint.Answer)[:160]) + "…"
		}
		out = append(out, checkpoint)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Delete 在运行完成后清理快照：能续跑的只有「没跑完」的那些。
func (s *CheckpointStore) Delete(runID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[runID]; !ok {
		return
	}
	delete(s.items, runID)
	for index, id := range s.order {
		if id == runID {
			s.order = append(s.order[:index], s.order[index+1:]...)
			break
		}
	}
}

// load 从 JSONL 恢复：同一 run 的后续行覆盖前面，所以按行顺序读即可。
func (s *CheckpointStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	latest := map[string]Checkpoint{}
	order := make([]string, 0, 16)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var checkpoint Checkpoint
		if json.Unmarshal([]byte(line), &checkpoint) != nil || checkpoint.RunID == "" {
			continue
		}
		if _, seen := latest[checkpoint.RunID]; !seen {
			order = append(order, checkpoint.RunID)
		}
		latest[checkpoint.RunID] = checkpoint
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := len(order) - 1; index >= 0 && len(s.items) < s.limit; index-- {
		checkpoint := latest[order[index]]
		if time.Since(checkpoint.UpdatedAt) > s.ttl || checkpoint.Status == "done" {
			continue
		}
		s.items[checkpoint.RunID] = checkpoint
		s.order = append(s.order, checkpoint.RunID)
	}
}

func persistCheckpoint(path string, checkpoint Checkpoint) {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = file.WriteString(string(payload) + "\n")
	_ = file.Close()
}

// trimCheckpointMessages 保留前两条（system + 首轮 user）和最近若干条，
// 中间被压缩掉的观察不需要进快照——它们已经在 compact 的摘要里。
func trimCheckpointMessages(messages []llm.Message) []llm.Message {
	trimmed := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		trimmed = append(trimmed, llm.Message{
			Role:    message.Role,
			Content: truncateRunes(message.Content, checkpointMessageRunes),
		})
	}
	if len(trimmed) <= checkpointMessageLimit {
		return trimmed
	}
	head := append([]llm.Message{}, trimmed[:2]...)
	tail := append([]llm.Message{}, trimmed[len(trimmed)-(checkpointMessageLimit-2):]...)
	return append(head, tail...)
}
