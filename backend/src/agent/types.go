// Package agent 是演出导购的多步推理层。
//
// 它把一次咨询拆成五段：规划 → 调用工具 → 观察 → 组织答案 → 自检。
// 每一段都产出可展示、可追踪的 Step，前端能看到「它想了什么、查了什么」。
//
// 三条硬规则：
//   - 工具只读：Agent 不直接碰数据源，所有查询都必须经过业务层实现的 Toolkit；
//   - 没有模型也能跑：模型缺席时退化为「规则驱动的工具脚本」，离线可演示、可回归；
//   - 答案只能复述观察：自检发现越权表述（声称已下单/已支付）就回退到数据版答案。
package agent

import (
	"context"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// 意图取值。规划器与工具策略都围绕这几个值分支，新增意图必须同时补策略与工具。
const (
	IntentRecommend = "recommend" // 找演出/选票
	IntentCompare   = "compare"   // 多场对比
	IntentReviews   = "reviews"   // 只看口碑与原文
	IntentVenue     = "venue"     // 场馆、交通、观演常识
	IntentPolicy    = "policy"    // 退改、秒杀、入场规则
	IntentExplore   = "explore"   // 宽泛浏览

	// 写意图：只生成待确认动作，不直接落库。
	IntentWatchlist  = "watchlist"  // 加入观演清单
	IntentPreference = "preference" // 记住长期偏好
)

// Model 是 Agent 对模型的最小依赖，*llm.Client 直接满足；
// 测试用脚本化的假模型替换它，不联网也能验证 ReAct 与自检。
type Model interface {
	Enabled() bool
	SmallModel() string
	LargeModel() string
	Complete(ctx context.Context, model string, messages []llm.Message, temperature float64) (string, error)
	Stream(ctx context.Context, model string, messages []llm.Message, temperature float64, onDelta func(string) error) error
}

// Slots 是一次咨询里「用户到底要什么」的结构化结果。
// 它同时承担跨轮记忆：上一轮说过的预算、人数、城市，下一轮可以被继承或覆盖。
type Slots struct {
	Party      int      `json:"party"`
	Budget     float64  `json:"budget"`      // 预算数值
	BudgetMode string   `json:"budget_mode"` // total 总预算 / per_person 人均
	SeatPref   string   `json:"seat_preference"`
	City       string   `json:"city"`
	Category   string   `json:"category"`
	DateFrom   string   `json:"date_from"`
	DateTo     string   `json:"date_to"`
	ShowIDs    []int64  `json:"show_ids,omitempty"` // 对话中提到/选中的演出
	ShowNames  []string `json:"show_names,omitempty"`
}

// PerTicketBudget 把总预算折算成单张预算；人数缺失时按 1 人处理。
func (s Slots) PerTicketBudget() float64 {
	if s.Budget <= 0 {
		return 0
	}
	if s.BudgetMode == "per_person" {
		return s.Budget
	}
	party := s.Party
	if party < 1 {
		party = 1
	}
	return s.Budget / float64(party)
}

// TotalBudget 把预算还原成总预算，用于展示与合计校验。
func (s Slots) TotalBudget() float64 {
	if s.Budget <= 0 {
		return 0
	}
	if s.BudgetMode == "per_person" {
		return s.Budget * float64(max(s.Party, 1))
	}
	return s.Budget
}

// SearchQuery 是 search_shows 工具的参数，字段名与前端筛选一一对应。
type SearchQuery struct {
	Query         string  `json:"query,omitempty"`
	City          string  `json:"city,omitempty"`
	Category      string  `json:"category,omitempty"`
	DateFrom      string  `json:"date_from,omitempty"`
	DateTo        string  `json:"date_to,omitempty"`
	Budget        float64 `json:"budget,omitempty"` // 单张上限，0 表示不限
	MinScore      float64 `json:"min_score,omitempty"`
	AvailableOnly bool    `json:"available_only,omitempty"`
	IncludeEnded  bool    `json:"include_ended,omitempty"`
	Limit         int     `json:"limit,omitempty"`
}

// Show 是工具返回的演出摘要；只放回答真正需要的字段，避免把整包 JSON 塞进模型上下文。
type Show struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	TypeName       string   `json:"type_name"`
	Area           string   `json:"area"`
	Address        string   `json:"address"`
	OpenTime       string   `json:"open_time"`
	Lifecycle      string   `json:"lifecycle"`
	Rating         float64  `json:"rating"`
	RatingCount    int      `json:"rating_count"`
	LowSample      bool     `json:"low_sample"`
	MinPrice       *float64 `json:"min_price"`
	MaxPrice       *float64 `json:"max_price"`
	AvailableStock int      `json:"available_stock"`
	HasSeckill     bool     `json:"has_seckill"`
}

// Ticket 是票档；与 Show 是 1:N，回答里最多展示三条。
type Ticket struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Price         float64 `json:"price"`
	Stock         int     `json:"stock"`
	SoldOut       bool    `json:"sold_out"`
	Seckill       bool    `json:"seckill"`
	SeckillActive bool    `json:"seckill_active"`
	HasSeats      bool    `json:"has_seats"`
	Rules         string  `json:"rules"`
}

// Quote 是评测原文；引用作者与编号，避免「模型编一条观众评价」。
type Quote struct {
	ID      int64   `json:"id"`
	Author  string  `json:"author"`
	Score   float64 `json:"score"`
	Content string  `json:"content"`
}

// Detail 是 show_detail / list_reviews / compare_shows 的共同返回结构。
type Detail struct {
	Show        Show     `json:"show"`
	Tickets     []Ticket `json:"tickets,omitempty"`
	Positive    []Quote  `json:"positive,omitempty"`
	Critical    []Quote  `json:"critical,omitempty"`
	Reasons     []string `json:"reasons,omitempty"`
	Cautions    []string `json:"cautions,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Purchasable bool     `json:"purchasable"`
}

// KnowledgeHit 是本地观演常识库的一条命中记录。
type KnowledgeHit struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Score   int    `json:"score"`
	// Aliases 是同一件事的口语说法（「轮椅」「拍照」「带孩子」）。
	// 它只参与检索，不出现在回答正文里：用户怎么问，决定能不能被搜到。
	Aliases []string `json:"aliases,omitempty"`
}

// BudgetPlan 是预算计算器的输出。金额一律由代码算，模型只负责解释。
type BudgetPlan struct {
	Party       int     `json:"party"`
	TotalBudget float64 `json:"total_budget"`
	TicketPrice float64 `json:"ticket_price"`
	TicketCost  float64 `json:"ticket_cost"`
	Remaining   float64 `json:"remaining"`
	OverBudget  bool    `json:"over_budget"`
}

// Toolkit 是 Agent 能对票务域执行的动作集合，由导购业务层实现。
// Agent 只认识这些语义化操作，不认识 HTTP、SQL 或 catalog 结构。
type Toolkit interface {
	Search(ctx context.Context, q SearchQuery) ([]Show, error)
	Detail(ctx context.Context, id int64) (Detail, error)
	Compare(ctx context.Context, ids []int64) ([]Detail, error)
	Reviews(ctx context.Context, id int64, focus string, limit int) (Detail, error)
}

// Step 是一步可展示的推理记录。前端按 Phase 分组渲染成时间线。
type Step struct {
	Index  int    `json:"index"`
	Phase  string `json:"phase"` // planner | tool | observe | answer | check
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Tool   string `json:"tool,omitempty"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
	At     string `json:"at"`
}

// Event 是运行过程中实时推给前端的事件：step 用于展示思考过程，chunk 用于逐字输出。
type Event struct {
	Type   string         `json:"type"`
	Step   *Step          `json:"step,omitempty"`
	Text   string         `json:"text,omitempty"`
	Phase  string         `json:"phase,omitempty"`
	Action *PendingAction `json:"action,omitempty"`
}

// Request 是一次 Agent 调用的全部输入。
type Request struct {
	Query      string
	SessionID  string
	OwnerID    string // 长期记忆与写操作的归属（设备级），为空时退回 SessionID
	History    []llm.Message
	Intent     string  // 业务层用规则预判的意图，为空时由规划器推断
	Slots      Slots   // 业务层解析好的结构化需求（可为零值）
	CompareIDs []int64 // 前端勾选的对比演出
	ShowID     int64   // 正在看的演出
}

// Result 是一次 Agent 调用的产出。Answer 可能已经通过 chunk 事件流式发过。
type Result struct {
	RunID   string          `json:"run_id"`
	Mode    string          `json:"mode"` // agent-rule（无模型）/ agent-react（有模型）
	Intent  string          `json:"intent"`
	Answer  string          `json:"answer"`
	Note    string          `json:"note,omitempty"`
	ShowIDs []int64         `json:"show_ids"`
	Steps   []Step          `json:"steps"`
	Actions []PendingAction `json:"actions,omitempty"`
	Slots   Slots           `json:"slots"`
	Plan    Plan            `json:"plan"`
	Elapsed int64           `json:"elapsed_ms"`

	// 断点续跑：Resumable 为真时，前端可以用同一个 run_id 调 resume，
	// 从已查到的事实继续，而不是把整条链路重跑一遍。
	Resumable    bool   `json:"resumable"`
	ResumeReason string `json:"resume_reason,omitempty"` // timeout | canceled | model_error
	ResumeStage  string `json:"resume_stage,omitempty"`  // act | answer
	ResumedFrom  string `json:"resumed_from,omitempty"`  // 本次结果是接着哪一次运行跑出来的
}

// Plan 是规划器的输出，也是 Trace 里最值得回看的一段。
type Plan struct {
	Intent   string   `json:"intent"`
	Focus    string   `json:"focus,omitempty"`
	Tools    []string `json:"tools"`
	Reason   string   `json:"reason,omitempty"`
	FromRule bool     `json:"from_rule"`
}
