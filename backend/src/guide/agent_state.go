package guide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/agent"
)

// 本文件是 Agent 的「写侧」：审批队列 + 观演清单 + 长期偏好。
//
// 三个约束写在最前面，后面所有实现都是为了满足它们：
//  1. 提案与执行分离：模型只能 Propose，真正落库必须经过 Confirm；
//  2. 幂等：同一条动作只能确认一次，重复确认返回当前状态而不是再写一遍；
//  3. 可审计：提案、确认、拒绝、过期都留在历史里，问题可追溯。

const (
	actionTTL        = 10 * time.Minute // 提案有效期：过期作废，避免「昨晚的确认今天生效」
	actionHistoryMax = 200              // 历史保留条数
	pendingPerOwner  = 5                // 单个设备最多挂起的提案数
)

// watchItem 是观演清单里的一条。
type watchItem struct {
	ShowID   int64  `json:"show_id"`
	ShowName string `json:"show_name"`
	Note     string `json:"note,omitempty"`
	At       string `json:"at"`
}

// agentState 是落盘结构；整体一次写入，避免多文件之间的半更新状态。
type agentState struct {
	Actions   []agent.PendingAction        `json:"actions"`
	Watchlist map[string][]watchItem       `json:"watchlist"`
	Profile   map[string]map[string]string `json:"profile"`
}

// agentStateStore 是 agent.ActionSink 的实现，同时对外提供清单与偏好读取。
type agentStateStore struct {
	mu   sync.Mutex
	path string
	data agentState
}

func newAgentStateStore(path string) *agentStateStore {
	store := &agentStateStore{path: path}
	store.data = agentState{
		Actions:   []agent.PendingAction{},
		Watchlist: map[string][]watchItem{},
		Profile:   map[string]map[string]string{},
	}
	store.load()
	return store
}

// agentStateFilePath 默认放程序同目录，可用 GUIDE_AGENT_STATE 覆盖（测试用）。
func agentStateFilePath() string {
	if custom := strings.TrimSpace(os.Getenv("GUIDE_AGENT_STATE")); custom != "" {
		return custom
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "agent-state.json")
	}
	return "agent-state.json"
}

func (s *agentStateStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var parsed agentState
	if json.Unmarshal(data, &parsed) != nil {
		return
	}
	if parsed.Actions == nil {
		parsed.Actions = []agent.PendingAction{}
	}
	if parsed.Watchlist == nil {
		parsed.Watchlist = map[string][]watchItem{}
	}
	if parsed.Profile == nil {
		parsed.Profile = map[string]map[string]string{}
	}
	s.data = parsed
}

// persistLocked 原子写入：先写临时文件再 rename，避免进程被杀时留下半个 JSON。
func (s *agentStateStore) persistLocked() error {
	payload, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

// Propose 是 agent.ActionSink 的实现：给动作编号、去重、落盘。
//
// 去重规则：同一设备 + 同类型 + 同目标（演出或偏好键）已有 pending，
// 直接复用旧提案，避免用户连点两下生成两条一样的待确认。
func (s *agentStateStore) Propose(ctx context.Context, ownerID string, action agent.PendingAction) (agent.PendingAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return agent.PendingAction{}, err
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return agent.PendingAction{}, errors.New("缺少设备标识，无法保存写操作")
	}
	s.expireLocked()

	pending := 0
	for _, existing := range s.data.Actions {
		if existing.OwnerID != ownerID || existing.Status != agent.ActionPending {
			continue
		}
		if sameTarget(existing, action) {
			return existing, nil
		}
		pending++
	}
	if pending >= pendingPerOwner {
		return agent.PendingAction{}, errors.New("待确认的操作太多，请先处理已有提案")
	}

	action.ID = shortID()
	action.OwnerID = ownerID
	action.Status = agent.ActionPending
	action.CreatedAt = time.Now().UTC()
	s.data.Actions = append([]agent.PendingAction{action}, s.data.Actions...)
	s.trimLocked()
	if err := s.persistLocked(); err != nil {
		return agent.PendingAction{}, err
	}
	return action, nil
}

func sameTarget(existing agent.PendingAction, next agent.PendingAction) bool {
	if existing.Kind != next.Kind {
		return false
	}
	switch next.Kind {
	case agent.ActionWatchlist:
		return existing.ShowID == next.ShowID
	case agent.ActionPreference:
		return existing.Key == next.Key
	default:
		return false
	}
}

// Decide 执行或拒绝一条提案。幂等：已决策过的动作返回当前状态 + errAlreadyDecided。
func (s *agentStateStore) Decide(id string, approve bool) (agent.PendingAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	for index := range s.data.Actions {
		action := &s.data.Actions[index]
		if action.ID != id {
			continue
		}
		if action.Status != agent.ActionPending {
			return *action, errAlreadyDecided
		}
		if !approve {
			action.Status = agent.ActionRejected
			action.DecidedAt = time.Now().UTC()
			action.Result = "用户取消"
			_ = s.persistLocked()
			return *action, nil
		}
		result, err := s.applyLocked(*action)
		if err != nil {
			return *action, err
		}
		action.Status = agent.ActionConfirmed
		action.DecidedAt = time.Now().UTC()
		action.Result = result
		_ = s.persistLocked()
		return *action, nil
	}
	return agent.PendingAction{}, errActionNotFound
}

// applyLocked 是唯一真正写数据的地方，只接受白名单动作类型。
func (s *agentStateStore) applyLocked(action agent.PendingAction) (string, error) {
	switch action.Kind {
	case agent.ActionWatchlist:
		items := s.data.Watchlist[action.OwnerID]
		for _, item := range items {
			if item.ShowID == action.ShowID {
				return "这场演出已经在观演清单里了", nil
			}
		}
		s.data.Watchlist[action.OwnerID] = append(items, watchItem{
			ShowID: action.ShowID, ShowName: action.ShowName, Note: action.Value,
			At: time.Now().UTC().Format(time.RFC3339),
		})
		name := action.ShowName
		if name == "" {
			name = fmt.Sprintf("演出 #%d", action.ShowID)
		}
		return "已把《" + name + "》加入观演清单", nil
	case agent.ActionPreference:
		if s.data.Profile[action.OwnerID] == nil {
			s.data.Profile[action.OwnerID] = map[string]string{}
		}
		s.data.Profile[action.OwnerID][action.Key] = action.Value
		return "已记住：" + action.Key + " = " + action.Value, nil
	default:
		return "", errors.New("不支持的动作类型：" + action.Kind)
	}
}

// expireLocked 把超时未确认的提案标记为过期；不用后台定时器，读时顺手清理。
func (s *agentStateStore) expireLocked() {
	now := time.Now()
	for index := range s.data.Actions {
		action := &s.data.Actions[index]
		if action.Status == agent.ActionPending && now.Sub(action.CreatedAt) > actionTTL {
			action.Status = agent.ActionExpired
			action.DecidedAt = now.UTC()
			action.Result = "超时未确认，已失效"
		}
	}
}

func (s *agentStateStore) trimLocked() {
	if len(s.data.Actions) > actionHistoryMax {
		s.data.Actions = s.data.Actions[:actionHistoryMax]
	}
}

// ListActions 返回某设备的提案历史（最新在前），供前端恢复待确认卡片。
func (s *agentStateStore) ListActions(ownerID string, limit int) []agent.PendingAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	out := make([]agent.PendingAction, 0, limit)
	for _, action := range s.data.Actions {
		if action.OwnerID != ownerID {
			continue
		}
		out = append(out, action)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// AllActions 供管理端审计：不带设备过滤，只看最近发生的事。
func (s *agentStateStore) AllActions(limit int) []agent.PendingAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if len(s.data.Actions) < limit {
		limit = len(s.data.Actions)
	}
	out := make([]agent.PendingAction, limit)
	copy(out, s.data.Actions[:limit])
	return out
}

// Watchlist 返回某设备已确认的观演清单。
func (s *agentStateStore) Watchlist(ownerID string) []watchItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := append([]watchItem{}, s.data.Watchlist[ownerID]...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].At > items[j].At })
	return items
}

// Profile 返回某设备的长期偏好，用于下一轮检索的默认值。
func (s *agentStateStore) Profile(ownerID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for key, value := range s.data.Profile[ownerID] {
		out[key] = value
	}
	return out
}

var (
	errAlreadyDecided = errors.New("这条操作已经处理过了")
	errActionNotFound = errors.New("没有找到这条操作")
)

// shortID 生成 8 位动作编号，够用且方便用户在界面上读。
func shortID() string {
	return agent.NewID()[:8]
}
