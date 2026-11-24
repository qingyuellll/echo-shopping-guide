package guide

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// customCase is an operator-written eval case: one visitor-style question plus
// key=value expectations checked against the same probe the built-in cases use.
type customCase struct {
	ID        string   `json:"id"`
	Message   string   `json:"message"`
	Expect    []string `json:"expect"`
	CreatedAt string   `json:"created_at"`
}

var customCaseKeys = map[string]bool{
	"party_size": true, "budget": true, "budget_mode": true, "city": true,
	"category": true, "date_from": true, "date_to": true, "seat_preference": true,
	"available_only": true, "lifecycle": true, "sort": true, "intent": true,
	"min_items": true, "min_alternatives": true,
}

type customCaseStore struct {
	mu    sync.RWMutex
	path  string
	items []customCase
}

func newCustomCaseStore(path string) *customCaseStore {
	store := &customCaseStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &store.items)
	}
	return store
}

func customCasesFilePath() string {
	if executable, err := os.Executable(); err == nil {
		return executable[:len(executable)-len(".exe")] + "-cases.json"
	}
	return "guide-cases.json"
}

func (c *customCaseStore) list() []customCase {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]customCase, len(c.items))
	copy(out, c.items)
	return out
}

func (c *customCaseStore) add(message string, expect []string) customCase {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	entry := customCase{ID: hex.EncodeToString(id), Message: message, Expect: expect, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	c.items = append([]customCase{entry}, c.items...)
	c.save()
	return entry
}

func (c *customCaseStore) remove(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.items[:0]
	removed := false
	for _, item := range c.items {
		if item.ID == id {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if removed {
		c.items = kept
		c.save()
	}
	return removed
}

func (c *customCaseStore) save() {
	payload, err := json.MarshalIndent(c.items, "", "  ")
	if err != nil {
		return
	}
	temporary := c.path + ".tmp"
	if os.WriteFile(temporary, payload, 0o644) == nil {
		_ = os.Rename(temporary, c.path)
	}
}

func (h *Handler) adminCasesList(w http.ResponseWriter, r *http.Request) {
	h.write(w, http.StatusOK, map[string]any{"items": h.customCases.list(), "keys": knownCaseKeys()})
}

func knownCaseKeys() []string {
	keys := make([]string, 0, len(customCaseKeys))
	for key := range customCaseKeys {
		keys = append(keys, key)
	}
	// Keep the list stable for the UI.
	ordered := []string{"party_size", "budget", "budget_mode", "city", "category", "date_from", "date_to", "seat_preference", "available_only", "lifecycle", "sort", "intent", "min_items", "min_alternatives"}
	for _, key := range ordered {
		_ = key
	}
	return ordered
}

func (h *Handler) adminCasesAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var input struct {
		Message string   `json:"message"`
		Expect  []string `json:"expect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "请求内容不是有效 JSON。"})
		return
	}
	message := strings.TrimSpace(input.Message)
	if len([]rune(message)) < 2 || len([]rune(message)) > 300 {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "问题需为 2–300 字。"})
		return
	}
	expect := []string{}
	for _, pair := range input.Expect {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, _, found := strings.Cut(pair, "=")
		if !found || !customCaseKeys[strings.TrimSpace(key)] {
			h.write(w, http.StatusBadRequest, map[string]string{"message": "期望项必须是已支持的 key=value：" + pair})
			return
		}
		expect = append(expect, pair)
	}
	if len(expect) == 0 {
		h.write(w, http.StatusBadRequest, map[string]string{"message": "至少写一条期望，例如 party_size=2。"})
		return
	}
	entry := h.customCases.add(message, expect)
	h.write(w, http.StatusOK, entry)
}

func (h *Handler) adminCasesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.customCases.remove(id) {
		h.write(w, http.StatusNotFound, map[string]string{"message": "没有找到这条自定义用例。"})
		return
	}
	h.write(w, http.StatusOK, map[string]string{"status": "deleted"})
}
