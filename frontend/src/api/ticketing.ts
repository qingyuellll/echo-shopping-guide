export interface Show {
	poster_url: string;
	lifecycle: 'upcoming' | 'live' | 'ended' | 'unknown';
	rating: RatingSummary;
	min_price: number | null;
	max_price: number | null;
	available_stock: number;
	tickets_known: boolean;
	ticket_tier_count: number;
	has_seckill: boolean;
  show_id: string;
  name: string;
  type_name: string;
  area: string;
  address: string;
  avg_price: number;
  score: number;
  sold: number;
  comments: number;
  open_time: string;
  end_time: string;
  description: string;
  detail_url: string;
}

export interface TicketTier {
  checkout_url: string;
  ticket_id: string;
  show_id: string;
  title: string;
  sub_title: string;
  rules: string;
  price: number;
  original_price: number;
  stock: number;
  stock_source: string;
  has_seats: boolean;
  sold_out: boolean;
  on_sale: boolean;
  is_seckill: boolean;
  seckill_active: boolean;
  seckill_begin: string;
  seckill_end: string;
}

export interface ShoppingPlan {party_size:number; budget:number; budget_mode:'total'|'per_person';seat_preference:string; filters:DiscoveryFilters}
export interface ReviewTopic {name:string;count:number;quotes:Review[]}
export interface ReviewInsights {positive:ReviewTopic[];critical:ReviewTopic[];dimensions:Record<string,{average:number;count:number}>;seat_areas:string[]}
export interface ConversationMessage { role: 'user' | 'assistant'; content: string }
/** Agent 的一步推理：phase 决定前端如何分组展示。 */
export interface AgentStep {
  index: number;
  phase: 'planner' | 'tool' | 'observe' | 'answer' | 'check' | string;
  title: string;
  detail?: string;
  tool?: string;
  ok: boolean;
  ms?: number;
}
export interface AgentConfig { enabled: boolean; mode: string; tools: string[]; resume?: boolean; rag?: string }
/** 服务端返回的「可以继续的运行」摘要。 */
export interface AgentResult {
  answer: string;
  mode: string;
  intent: string;
  note?: string;
  run_id: string;
  session_id?: string;
  items: Recommendation[];
  plan?: ShoppingPlan;
  steps?: AgentStep[];
  elapsed_ms?: number;
  resumable?: boolean;
  resumed_from?: string;
}
export interface AgentCheckpoint {
  run_id: string;
  query: string;
  stage: string;
  reason?: string;
  round?: number;
  tools?: number;
  steps?: number;
  updated_at?: string;
}
/** 一次待确认的写操作：确认后才会真正落库。 */
export interface AgentAction {
  id: string;
  kind: 'watchlist' | 'preference' | string;
  title: string;
  summary: string;
  risk?: string;
  show_id?: number;
  show_name?: string;
  key?: string;
  value?: string;
  status: 'pending' | 'confirmed' | 'rejected' | 'expired' | string;
  result?: string;
}
export interface RatingSummary {
  average: number; count: number; total: number; distribution: number[];
  positive_rate: number; adjusted_score: number; complete: boolean;
  available: boolean; text_count: number; low_sample: boolean;
}
export interface Review {
  purchased?:boolean;verified?:boolean;evidence_known?:boolean;seat_area?:string;dimensions?:Record<string,number>;
  review_id: string; show_id: string; title: string; content: string; score: number;
  author: string; likes: number; comments: number; created_at: string; detail_url: string;
}
export interface ReviewReport { insights?:ReviewInsights; items: Review[]; summary: RatingSummary; positive: Review[]; critical: Review[]; warnings: string[] }
export interface Coverage { public_total: number; loaded: number; active: number; ended: number; review_count: number; rated_count: number; complete: boolean; warnings: string[]; updated_at: string }
export interface DiscoveryFilters { query?: string; category?: string; city?: string; lifecycle?: string; sort?: string; budget?: number; min_score?: number; available_only?: boolean; date_from?: string; date_to?: string; ids?: string }
export interface CatalogPage { items: Show[]; total: number; page: number; has_more: boolean; categories: string[]; cities: string[]; coverage: Coverage }
export interface ReviewQuote { show: string; author: string; score: number; excerpt: string; review_id: string }
export interface Recommendation { updated_at:string; show: Show; tickets: TicketTier[]; reviews: ReviewReport; reasons: string[]; cautions: string[]; keywords?: string[]; related?: ReviewQuote[] }
export interface ShowDetail { tickets_updated_at:string; show: Show; items: TicketTier[]; reviews: ReviewReport; purchasable: boolean; warnings: string[]; updated_at: string }
export interface ReviewFeedEntry extends Review { show_name: string; show_available: boolean; lifecycle: string }
export interface ReviewFeedPage { items: ReviewFeedEntry[]; summary: RatingSummary; total: number; page: number; has_more: boolean; unlisted_count: number; warnings: string[] }

function filterQuery(filters: DiscoveryFilters, page = 1, refresh = false) {
  const query = new URLSearchParams({ page: String(page), page_size: '12' });
  for (const [key, value] of Object.entries(filters)) if (value !== undefined && value !== '') query.set(key, String(value));
  if (refresh) query.set('refresh', 'true');
  return query;
}

async function call<T>(path: string, signal?: AbortSignal, body?: unknown): Promise<T> {
  const response = await fetch(`/api/v1/ticketing${path}`, {
    signal,
    method: body ? 'POST' : 'GET',
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined
  });
  let data;
  try { data = await response.json(); } catch { throw new Error('暂时无法连接导购服务，请启动服务后重试。'); }
  if (!response.ok) throw new Error(data.message || '查询暂时不可用，请稍后重试。');
  return data as T;
}

export const ticketingAPI = {
  config: (signal?: AbortSignal) => call<{ ai_enabled: boolean; management_enabled: boolean; echo_home: string; agent?: AgentConfig }>('/config', signal),
  shows: (query: string, signal?: AbortSignal) => call<{ items: Show[]; updated_at: string }>(`/shows?query=${encodeURIComponent(query)}`, signal),
  discover: (filters: DiscoveryFilters, page: number, signal?: AbortSignal, refresh = false) => call<CatalogPage>(`/shows?${filterQuery(filters, page, refresh)}`, signal),
  detail: (id: string, signal?: AbortSignal, refresh = false) => call<ShowDetail>(`/shows/${encodeURIComponent(id)}/detail${refresh ? '?refresh=true' : ''}`, signal),
  rankings: (lifecycle: string, signal?: AbortSignal) => call<{ items: Show[]; coverage: Coverage; method: string }>(`/rankings?lifecycle=${encodeURIComponent(lifecycle)}`, signal),
  compare: (ids: string[], signal?: AbortSignal) => call<{ items: Recommendation[]; updated_at: string }>(`/compare?ids=${ids.map(encodeURIComponent).join(',')}`, signal),
  reviewFeed: (query: string, filter: string, sort: string, page: number, signal?: AbortSignal, refresh = false) => call<ReviewFeedPage>(`/review-feed?${new URLSearchParams({ query, filter, sort, page: String(page), refresh: String(refresh) })}`, signal),
  tickets: (id: string, signal?: AbortSignal) => call<{ show: Show; items: TicketTier[]; updated_at: string }>(`/shows/${encodeURIComponent(id)}/tickets`, signal),
  checkpoints: (profileId: string, signal?: AbortSignal) =>
    call<{ items: AgentCheckpoint[] }>(`/agent/checkpoints?profile_id=${encodeURIComponent(profileId)}`, signal),
  resume: (runId: string) => call<AgentResult>(`/agent/resume/${encodeURIComponent(runId)}`, undefined, {}),
  confirmAction: (id: string) => call<{ action: AgentAction; watchlist: { show_id: number; show_name: string }[] }>(`/agent/actions/${encodeURIComponent(id)}/confirm`, undefined, {}),
  rejectAction: (id: string) => call<{ action: AgentAction }>(`/agent/actions/${encodeURIComponent(id)}/reject`, undefined, {}),
  agent: (body: { session_id?: string; profile_id?: string; plan?:ShoppingPlan; message: string; history: ConversationMessage[]; show_id?: string; query: string; budget: number; filters?: DiscoveryFilters; compare_ids?: string[] }, signal?: AbortSignal) =>
    call<{ answer: string; mode: string; intent: string; note?: string; run_id: string; session_id: string; items: Recommendation[]; plan:ShoppingPlan; steps: AgentStep[]; elapsed_ms: number; resumable?: boolean }>('/agent', signal, body),
  assist: (body: { plan?:ShoppingPlan; message: string; history: ConversationMessage[]; show_id?: string; query: string; budget: number; filters?: DiscoveryFilters; compare_ids?: string[] }, signal?: AbortSignal) =>
    call<{ answer: string; mode: string; items: Recommendation[]; alternatives?: Recommendation[]; plan:ShoppingPlan; updated_at:string }>('/assist', signal, body)
};

export function safeEchoURL(value: string, fallback: string): string {
  try { const url = new URL(value); if (['http:', 'https:'].includes(url.protocol)) return url.href; } catch { /* Use the known local Echo address. */ }
  return fallback;
}

// Filter values stay stable; only the label follows the standard place name.
export function cityLabel(value: string) {
  return value === '香港' ? '中国香港' : value === '澳门' ? '中国澳门' : value === '台湾' ? '中国台湾' : value;
}

// Carry the remembered party size, total budget and seat preference into Echo,
// so a confirmed choice keeps the requirement the chat already understood.
export function checkoutLink(tier: TicketTier, plan?: ShoppingPlan, home = 'http://127.0.0.1:3002') {
  const fallback = `${home.replace(/\/$/, '')}/guide/checkout/${tier.ticket_id}`;
  const base = safeEchoURL(tier.checkout_url, fallback);
  if (!plan) return base;
  try {
    const url = new URL(base);
    url.searchParams.set('count', String(Math.min(10, Math.max(1, plan.party_size))));
    if (plan.budget > 0) {
      const total = plan.budget_mode === 'per_person' ? plan.budget * plan.party_size : plan.budget;
      url.searchParams.set('budget', total.toFixed(2));
    } else {
      url.searchParams.delete('budget');
    }
    if (plan.seat_preference) url.searchParams.set('seatPreference', plan.seat_preference);
    else url.searchParams.delete('seatPreference');
    return url.toString();
  } catch { return base; }
}

export function money(value: number) {
  return new Intl.NumberFormat('zh-CN', { minimumFractionDigits: 0, maximumFractionDigits: 2 }).format(value);
}
