package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Run 是一次 Agent 运行的完整轨迹，也是管理端「Agent 轨迹」页面的数据源。
// 它记录的是「为什么这么答」：意图、工具链、每一步耗时、是否触发自检回退。
type Run struct {
	RunID     string   `json:"run_id"`
	SessionID string   `json:"session_id"`
	Query     string   `json:"query"`
	Intent    string   `json:"intent"`
	Mode      string   `json:"mode"`
	StartedAt string   `json:"started_at"`
	ElapsedMS int64    `json:"elapsed_ms"`
	Steps     []Step   `json:"steps"`
	Tools     []string `json:"tools"`
	ShowIDs   []int64  `json:"show_ids"`
	Slots     Slots    `json:"slots"`
	Plan      Plan     `json:"plan"`
	Answer    string   `json:"answer"`
	Note      string   `json:"note,omitempty"`
	Error     string   `json:"error,omitempty"`
	// 断点续跑：哪些运行留下了快照、哪一次是接着别人跑的。
	Resumable   bool   `json:"resumable,omitempty"`
	ResumedFrom string `json:"resumed_from,omitempty"`
}

// TraceStore 保存最近若干次运行：内存环形缓冲供页面读取，可选 JSONL 落盘供重启后排查。
type TraceStore struct {
	mu    sync.RWMutex
	runs  []Run
	limit int
	path  string
}

// NewTraceStore 构建轨迹仓库；path 为空表示只存内存（测试用）。
func NewTraceStore(path string, limit int) *TraceStore {
	if limit <= 0 {
		limit = 100
	}
	store := &TraceStore{limit: limit, path: strings.TrimSpace(path)}
	if store.path != "" {
		store.load()
	}
	return store
}

// Add 写入一次运行：内存里放最前面（最新），同时追加一行 JSONL。
func (s *TraceStore) Add(run Run) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.runs = append([]Run{run}, s.runs...)
	if len(s.runs) > s.limit {
		s.runs = s.runs[:s.limit]
	}
	path := s.path
	s.mu.Unlock()
	if path != "" {
		persistRun(path, run)
	}
}

// List 返回最近 limit 条；不含完整答案，避免管理端首屏过大。
func (s *TraceStore) List(limit int) []Run {
	if s == nil {
		return nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := min(limit, len(s.runs))
	out := make([]Run, 0, count)
	for _, run := range s.runs[:count] {
		trimmed := run
		if len([]rune(trimmed.Answer)) > 200 {
			trimmed.Answer = string([]rune(trimmed.Answer)[:200]) + "…"
		}
		out = append(out, trimmed)
	}
	return out
}

// Get 按 run_id 取完整轨迹。
func (s *TraceStore) Get(runID string) (Run, bool) {
	if s == nil {
		return Run{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, run := range s.runs {
		if run.RunID == runID {
			return run, true
		}
	}
	return Run{}, false
}

// load 从 JSONL 恢复最近轨迹；文件损坏的行直接跳过，不影响启动。
func (s *TraceStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	runs := make([]Run, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var run Run
		if json.Unmarshal([]byte(line), &run) != nil || run.RunID == "" {
			continue
		}
		runs = append(runs, run)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(runs) - 1; i >= 0 && len(s.runs) < s.limit; i-- {
		s.runs = append(s.runs, runs[i])
	}
}

func persistRun(path string, run Run) {
	payload, err := json.Marshal(run)
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

// NewID 生成 16 位十六进制 id；业务层生成动作编号时复用同一套规则。
func NewID() string { return newID() }

// newID 生成 16 位十六进制 id；失败时退化成时间戳，保证流程不中断。
func newID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
}

// now 统一时间格式，便于 Trace 展示与排序。
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}
