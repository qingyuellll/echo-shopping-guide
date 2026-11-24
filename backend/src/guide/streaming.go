package guide

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/LYP-leo/echo-shopping-guide/backend/src/llm"
)

// decodeAssist shares the plain endpoint's request validation.
func decodeAssist(w http.ResponseWriter, r *http.Request) (assistRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var input assistRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.Message) == "" || len([]rune(input.Message)) > 1500 || math.IsNaN(input.Budget) || math.IsInf(input.Budget, 0) || input.Budget < 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "请输入 1–1500 字的选票问题及有效预算"})
		return input, false
	}
	return input, true
}

// modelMessages assembles the prompt sent to the large model: the editable
// system prompt, the trimmed history, and one user message that carries the
// server-side facts as the only source of truth.
func (h *Handler) modelMessages(input assistRequest, intent string, plan shoppingPlan, filters discoveryFilters, entries []comparisonEntry, alternatives []comparisonEntry) []llm.Message {
	facts, _ := json.Marshal(map[string]any{"items": entries, "alternatives": alternatives})
	messages := []llm.Message{{Role: "system", Content: h.prompt.system()}}
	history := input.History
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	for _, message := range history {
		if (message.Role == "user" || message.Role == "assistant") && len([]rune(message.Content)) <= 3000 {
			messages = append(messages, message)
		}
	}
	messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf("意图：%s；预算上限：%.2f元。\n需求：%s（单张预算已按人数折算；座位偏好尚未落实）。\n事实与原文依据：%s\n用户问题：%s", intent, filters.Budget, planDescription(plan), facts, input.Message)})
	return messages
}

// chunkAnswer splits a data-mode answer into typewriter-sized pieces so the
// visitor starts reading immediately instead of waiting for the model.
func chunkAnswer(text string, size int) []string {
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}
	pieces := []string{}
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		pieces = append(pieces, string(runes[i:end]))
	}
	return pieces
}

// assistStream answers over Server-Sent Events: the real-data answer (with
// recommendations) arrives first, then the model polishes the answer in
// place. If the model is slow or fails, the visitor has already been reading
// the data answer — the wait never blocks the content.
func (h *Handler) assistStream(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeAssist(w, r)
	if !ok {
		return
	}
	catalog, rich := h.catalog.(richCatalog)
	if !rich {
		h.write(w, http.StatusServiceUnavailable, map[string]string{"message": "当前数据源不支持助手咨询。"})
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	comp, code, message := h.computeAssist(ctx, input, catalog)
	if code != 0 {
		h.write(w, code, map[string]string{"message": message})
		return
	}
	// The outer http.TimeoutHandler wraps the writer, so flushing must go
	// through ResponseController, which unwraps it. If flushing is unsupported
	// (old client), fall back to the buffered endpoint.
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		h.assistRich(w, r, input, catalog)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(payload map[string]any) bool {
		payload["ts"] = time.Now().UnixMilli()
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		if controller.Flush() != nil {
			return false
		}
		return true
	}
	payloadBase := func(mode string) map[string]any {
		return map[string]any{"mode": mode, "intent": comp.Intent, "items": comp.Entries, "alternatives": comp.Alternatives, "filters": comp.Filters, "plan": comp.Plan, "coverage": comp.Snapshot.Coverage, "updated_at": comp.Snapshot.Coverage.UpdatedAt}
	}
	meta := payloadBase("catalog")
	meta["type"] = "meta"
	if !send(meta) {
		return
	}
	for _, piece := range chunkAnswer(comp.CatalogAnswer, 80) {
		if !send(map[string]any{"type": "delta", "source": "data", "text": piece}) {
			return
		}
	}
	answer := comp.CatalogAnswer
	mode := "catalog"
	note := ""
	if h.model != nil && h.model.Enabled() {
		if !send(map[string]any{"type": "phase", "phase": "model"}) {
			return
		}
		messages := h.modelMessages(input, comp.Intent, comp.Plan, comp.Filters, comp.Entries, comp.Alternatives)
		modelCtx, modelCancel := context.WithTimeout(ctx, 30*time.Second)
		var modelText strings.Builder
		streamErr := h.model.Stream(modelCtx, h.model.LargeModel(), messages, 0.2, func(delta string) error {
			modelText.WriteString(delta)
			send(map[string]any{"type": "delta", "source": "model", "text": delta})
			return nil
		})
		modelCancel()
		if streamErr == nil && strings.TrimSpace(modelText.String()) != "" {
			answer = strings.TrimSpace(modelText.String())
			mode = "ai"
		} else {
			note = "模型暂时不可用，上面是按真实票务数据整理的回答。"
			h.stats.markFallback()
		}
	}
	final := payloadBase(mode)
	final["type"] = "final"
	final["answer"] = answer
	if note != "" {
		final["note"] = note
	}
	send(final)
	h.stats.record(askRecord{At: time.Now().UTC().Format(time.RFC3339), Question: input.Message, Mode: mode, Intent: comp.Intent, LatencyMS: time.Since(started).Milliseconds(), Items: len(comp.Entries), Alternatives: len(comp.Alternatives)})
}
