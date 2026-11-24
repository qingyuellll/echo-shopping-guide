package agent

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemoryStore 保存每个会话最近一次确认过的需求。
//
// 为什么放在服务端：前端只带最近 8 条消息，一旦本地历史被截断，
// 「刚才说的两个人、600 预算」就丢了。服务端只保存结构化的 Slots，
// 不保存对话原文——既省内存，也不把用户原话留在进程里。
type MemoryStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	limit int
	items map[string]*memoryEntry
}

type memoryEntry struct {
	slots     Slots
	intent    string
	updatedAt time.Time
	turns     int
}

// Memory 是「这个会话上次聊到哪」的快照：结构化需求、上一轮意图与轮次。
// 意图也要记：用户接着说「那有没有便宜点的」，只靠关键词会被判成宽泛浏览，
// 继承上一轮的 recommend 才符合人的理解。
type Memory struct {
	Slots  Slots
	Intent string
	Turns  int
}

// NewMemoryStore 构建会话记忆；ttl 为 0 时默认 30 分钟，limit 为 0 时默认 500 个会话。
func NewMemoryStore(ttl time.Duration, limit int) *MemoryStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if limit <= 0 {
		limit = 500
	}
	return &MemoryStore{ttl: ttl, limit: limit, items: map[string]*memoryEntry{}}
}

// Load 读取会话记忆，过期视为不存在。
func (m *MemoryStore) Load(session string) (Memory, bool) {
	session = strings.TrimSpace(session)
	if m == nil || session == "" {
		return Memory{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.items[session]
	if !ok || time.Since(entry.updatedAt) > m.ttl {
		delete(m.items, session)
		return Memory{}, false
	}
	return Memory{Slots: entry.slots, Intent: entry.intent, Turns: entry.turns}, true
}

// Save 写回会话记忆；超过容量时先清过期，再丢最旧的一条。
func (m *MemoryStore) Save(session string, slots Slots, intent string) {
	session = strings.TrimSpace(session)
	if m == nil || session == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.items[session]; ok {
		entry.slots = slots
		entry.intent = intent
		entry.updatedAt = time.Now()
		entry.turns++
		return
	}
	if len(m.items) >= m.limit {
		m.evictLocked()
	}
	m.items[session] = &memoryEntry{slots: slots, intent: intent, updatedAt: time.Now(), turns: 1}
}

// evictLocked 先清过期，再淘汰最久未更新的会话。调用方必须已持锁。
func (m *MemoryStore) evictLocked() {
	now := time.Now()
	for key, entry := range m.items {
		if now.Sub(entry.updatedAt) > m.ttl {
			delete(m.items, key)
		}
	}
	if len(m.items) < m.limit {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range m.items {
		if oldestKey == "" || entry.updatedAt.Before(oldest) {
			oldestKey, oldest = key, entry.updatedAt
		}
	}
	if oldestKey != "" {
		delete(m.items, oldestKey)
	}
}

// mergeSlots 把本轮新解析出的需求合并到历史需求上：本轮明确的字段优先，
// 本轮没提的字段沿用上一轮。这就是「多轮对话里默认记住上次条件」的实现。
func mergeSlots(previous Slots, next Slots) Slots {
	merged := previous
	if next.Party > 0 {
		merged.Party = next.Party
	}
	if next.Budget > 0 {
		merged.Budget = next.Budget
		merged.BudgetMode = next.BudgetMode
	} else if next.BudgetMode == "total" || next.BudgetMode == "per_person" {
		// 只给了口径没给金额（如「改成人均」），沿用旧金额但换口径。
		merged.BudgetMode = next.BudgetMode
	}
	if merged.BudgetMode == "" {
		merged.BudgetMode = "total"
	}
	if next.SeatPref != "" {
		merged.SeatPref = next.SeatPref
	}
	if value := strings.TrimSpace(next.City); value != "" {
		merged.City = value
	}
	if value := strings.TrimSpace(next.Category); value != "" {
		merged.Category = value
	}
	if next.DateFrom != "" {
		merged.DateFrom = next.DateFrom
		merged.DateTo = next.DateTo
	}
	if len(next.ShowIDs) > 0 {
		merged.ShowIDs = next.ShowIDs
		merged.ShowNames = next.ShowNames
	}
	if merged.Party < 1 {
		merged.Party = 1
	}
	return merged
}

// describeSlots 把需求写成一句人话，用于回答开头「已记住：…」。
func describeSlots(slots Slots) string {
	parts := []string{strconv.Itoa(max(slots.Party, 1)) + " 人"}
	if slots.Budget > 0 {
		kind := "总预算"
		if slots.BudgetMode == "per_person" {
			kind = "人均预算"
		}
		parts = append(parts, kind+" ¥"+formatMoney(slots.Budget))
	}
	if slots.City != "" {
		parts = append(parts, slots.City)
	}
	if slots.Category != "" {
		parts = append(parts, slots.Category)
	}
	if slots.DateFrom != "" {
		if slots.DateTo != "" && slots.DateTo != slots.DateFrom {
			parts = append(parts, slots.DateFrom+" 至 "+slots.DateTo)
		} else {
			parts = append(parts, slots.DateFrom)
		}
	}
	if slots.SeatPref != "" {
		parts = append(parts, "座位偏好："+seatLabel(slots.SeatPref))
	}
	return strings.Join(parts, " · ")
}

func seatLabel(value string) string {
	switch value {
	case "front":
		return "想坐前面"
	case "adjacent":
		return "需要连座"
	default:
		return value
	}
}

// formatMoney 去掉多余的小数位：600 → 600，599.5 → 599.5。
func formatMoney(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
