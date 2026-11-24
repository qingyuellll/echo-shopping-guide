import type { Coverage, DiscoveryFilters, ShoppingPlan } from './ticketing';

const BASE = '/api/v1/guide-admin';

export class AdminError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

async function call<T>(path: string, token: string | undefined, init?: RequestInit): Promise<T> {
  const response = await fetch(`${BASE}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { 'X-Guide-Admin-Token': token } : {}),
      ...(init?.headers ?? {})
    }
  });
  let payload: any = null;
  try { payload = await response.json(); } catch { /* Keep the status message below. */ }
  if (!response.ok) throw new AdminError(payload?.message || `管理接口返回 ${response.status}`, response.status);
  return payload as T;
}

export interface AdminModel { enabled: boolean; provider?: string; model?: string; thinking?: boolean }
export interface AdminSession { enabled: boolean; authenticated: boolean; model: AdminModel; prompt: { version: number; updated_at: string }; started_at: string }
export interface HealthItem { level: 'ok' | 'warn' | 'error'; label: string; detail: string }
export interface AskRecord { at: string; question: string; mode: string; intent: string; latency_ms: number; items: number; alternatives: number }
export interface AdminOverview {
  coverage: Coverage;
  health: HealthItem[];
  model: AdminModel;
  prompt: { version: number; updated_at: string };
  stats: { total: number; ai: number; catalog: number; fallback: number; avg_latency_ms: number; recent: AskRecord[]; since: string };
  cities: number;
  categories: number;
  updated_at: string;
  cache_note: string;
}
export interface PromptDoc { text: string; version: number; updated_at: string; default: string; max_runes: number; history: PromptHistoryEntry[] }
export interface PromptHistoryEntry { version: number; at: string; action: string; text: string }
export interface PromptSaveResult { version: number; updated_at: string; action: string; eval?: EvalReport }
export interface EvalCheck { label: string; ok: boolean; got: string }
export interface EvalCase { name: string; question: string; pass: boolean; checks: EvalCheck[] }
export interface EvalReport { ran_at: string; duration_ms: number; passed: number; total: number; cases: EvalCase[]; note: string }


export interface CityRow {
  city: string;
  shows: number;
  active: number;
  ended: number;
  rated: number;
  rating_count: number;
  average: number;
  reviews: number;
  min_price: number | null;
  max_price: number | null;
  tiers: number;
  selectable: number;
  available_stock: number;
  missing_tickets: number;
  missing_reviews: number;
}
export interface CategoryRow { name: string; shows: number; active: number; purchasable: number }
export interface TicketSummary {
  tiers: number; purchasable: number; sold_out: number; seckill: number; selectable: number; no_seat: number;
  snapshot_stock: number; missing_rules: number; ended_tiers: number; stock: number; suspicious_price: number;
  average_price: number; min_price: number; max_price: number; shows_without_tickets: number;
}
export interface PriceBand { label: string; min: number; max: number; count: number; share: number }
export interface TierRow {
  show_id: string; show: string; city: string; lifecycle: string;
  title: string; sub_title: string; price: number; stock: number; stock_source: string;
  has_seats: boolean; is_seckill: boolean; seckill_active: boolean; sold_out: boolean; on_sale: boolean; rules_missing: boolean;
}
export interface CatalogAnalytics {
  updated_at: string;
  cities: CityRow[];
  categories: CategoryRow[];
  ticket_summary: TicketSummary;
  price_bands: PriceBand[];
  tiers: TierRow[];
  notes: string[];
}


export interface AdminAskRow { title: string; sub: string; value: string; tags: string[] }
export interface AdminAskResponse {
  answer: string;
  kind: 'data' | 'guide';
  mode: string;
  intent: string;
  plan?: ShoppingPlan;
  filters?: DiscoveryFilters;
  rows: AdminAskRow[];
  flags: string[];
  latency_ms: number;
  updated_at: string;
}

export interface CustomCase { id: string; message: string; expect: string[]; created_at: string }
export interface DraftTier { title: string; price: number; stock: number; has_seats: boolean; is_seckill: boolean; seckill_begin?: string; seckill_end?: string; rules: string }
export interface DraftShow { name: string; type_name: string; city: string; address: string; intro: string; open_time: string; tiers: DraftTier[]; generated: string }

export const guideAdminAPI = {
  session: (token?: string) => call<AdminSession>('/session', token),
  overview: (token: string, refresh = false) => call<AdminOverview>(`/overview${refresh ? '?refresh=true' : ''}`, token),
  prompt: (token: string) => call<PromptDoc>('/prompt', token),
  savePrompt: (token: string, body: { text?: string; restore?: boolean; version?: number; confirm: string }) =>
    call<PromptSaveResult>('/prompt', token, { method: 'PUT', body: JSON.stringify(body) }),
  evaluate: (token: string) => call<EvalReport>('/eval', token, { method: 'POST', body: '{}' }),
  catalog: (token: string, refresh = false) => call<CatalogAnalytics>(`/catalog${refresh ? '?refresh=true' : ''}`, token),
  ask: (token: string, body: { message: string; plan?: ShoppingPlan; use_model?: boolean }) =>
    call<AdminAskResponse>('/ask', token, { method: 'POST', body: JSON.stringify(body) }),
  cases: (token: string) => call<{ items: CustomCase[]; keys: string[] }>('/cases', token),
  addCase: (token: string, body: { message: string; expect: string[] }) =>
    call<CustomCase>('/cases', token, { method: 'POST', body: JSON.stringify(body) }),
  deleteCase: (token: string, id: string) => call<{ status: string }>(`/cases/${id}`, token, { method: 'DELETE' }),
  draft: (token: string, text: string) =>
    call<{ draft: DraftShow; csv: string }>('/draft', token, { method: 'POST', body: JSON.stringify({ text }) })
};

// ---------- Agent 观测 ----------

export interface AgentRunStep { index: number; phase: string; tool?: string; title: string; detail?: string; ok: boolean; ms?: number; at?: string }
export interface AgentRun {
  run_id: string; session_id: string; query: string; intent: string; mode: string;
  started_at: string; elapsed_ms: number; steps: AgentRunStep[]; tools: string[];
  show_ids: number[]; resumable?: boolean; resumed_from?: string; answer?: string; note?: string;
}
export interface AgentCheckpointRow { run_id: string; query: string; stage: string; reason?: string; round?: number; tools?: number; steps?: number; updated_at?: string }
export interface AgentActionRow { id: string; kind: string; title: string; summary: string; status: string; result?: string; owner_id?: string; created_at?: string; decided_at?: string }
export interface BenchReport {
  cases: number; passed: number; skipped: number; tool_hit_rate: number; intent_rate: number;
  avg_steps: number; avg_elapsed_ms: number; failures?: string[];
}
export interface RagStrategyRow { strategy: string; recall_at_k: number; mrr_at_k: number; avg_ms: number; misses: number }
export interface RagReport { embedder: string; k: number; cases: number; results: RagStrategyRow[]; examples?: { query: string; gold: string; baseline: string; hybrid: string }[] }

export const agentAPI = {
  runs: (token: string) => call<{ items: AgentRun[]; mode: string }>('/agent/runs?limit=15', token),
  checkpoints: (token: string) => call<{ items: AgentCheckpointRow[] }>('/agent/checkpoints?limit=20', token),
  actions: (token: string) => call<{ items: AgentActionRow[] }>('/agent/actions?limit=20', token),
  bench: (token: string) => call<{ report: BenchReport; mode: string; cases: number }>('/agent/bench', token, { method: 'POST' }),
  ragEval: (token: string) => call<{ report: RagReport; entries: number }>('/agent/rag-eval', token, { method: 'POST' })
};
