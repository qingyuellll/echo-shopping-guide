package agent

import (
	"context"
	"strings"
	"time"
)

// 写操作采用「提案 → 人工确认 → 执行」三段式，而不是让模型直接落库。
//
// 这不是保守，是真实产品里的必要设计：
//   - 模型可能理解错（用户说「帮我加个提醒」，它可能挑错场次）；
//   - 写操作不可撤销，误写的代价远高于误答；
//   - 审批记录本身就是审计材料，出了问题能追溯。
//
// 所以 Agent 只负责「准备一个动作」，真正执行由用户在界面上点确认。

// 动作类型。新增类型要同时补 executeWrite（guide 侧）与前端卡片文案。
const (
	ActionWatchlist  = "watchlist"  // 加入观演清单（导购侧持久化）
	ActionPreference = "preference" // 保存长期偏好（影响后续检索）
)

// 动作状态机：pending → confirmed / rejected / expired。
const (
	ActionPending   = "pending"
	ActionConfirmed = "confirmed"
	ActionRejected  = "rejected"
	ActionExpired   = "expired"
)

// PendingAction 是一次等待用户确认的写操作。
// 它包含「改什么」的全部参数，确认时不需要再问模型，避免二次理解偏差。
type PendingAction struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id,omitempty"` // 稳定的设备/用户标识，跨会话有效
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Risk      string    `json:"risk"` // low / medium
	ShowID    int64     `json:"show_id,omitempty"`
	ShowName  string    `json:"show_name,omitempty"`
	Key       string    `json:"key,omitempty"`
	Value     string    `json:"value,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at,omitempty"`
	Result    string    `json:"result,omitempty"`
}

// ActionSink 由业务层实现：负责给提案编号、落盘、执行。
// Agent 只依赖这个窄接口，不关心存储是文件还是数据库。
type ActionSink interface {
	Propose(ctx context.Context, ownerID string, action PendingAction) (PendingAction, error)
}

// describeAction 把提案写成一句人能读的话，同时用于观察文本和前端卡片。
func describeAction(action PendingAction) string {
	if action.Summary != "" {
		return action.Summary
	}
	return action.Title
}

// writeToolHint 是写工具返回给模型的观察：明确告诉它「还没执行、需要用户点确认」，
// 避免模型在最终回答里说「已经帮你加好了」。
func writeToolHint(action PendingAction) string {
	return strings.Join([]string{
		"已生成待确认操作：" + describeAction(action) + "。",
		"操作编号 " + action.ID + "，当前状态 pending。",
		"注意：这一步没有执行。必须在回答里说明「已准备好，等你点确认」，不能说已经完成。",
	}, " ")
}
