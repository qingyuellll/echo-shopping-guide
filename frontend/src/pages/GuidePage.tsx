import { FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import { checkoutLink, cityLabel, money, safeEchoURL, ticketingAPI } from '../api/ticketing';
import type { AgentAction, AgentStep, CatalogPage, ConversationMessage, DiscoveryFilters, Recommendation, Show, ShoppingPlan } from '../api/ticketing';
import { ComparisonDialog, dateLabel, lifecycleLabel, ratingLabel, ShowDetails } from './DiscoveryDetails';
import { ReviewFeed } from './ReviewFeed';
import './guide.css';
import './discovery.css';

type SavedShow = { id: string; name: string };
// 一条消息除了文字，还可能带 Agent 的推理步骤（只用于渲染，不会回传给模型）。
type ChatEntry = ConversationMessage & { steps?: AgentStep[]; agentMode?: string; actions?: AgentAction[]; resumableRunID?: string };
function newSessionId() { return `g-${Math.random().toString(36).slice(2, 10)}${Date.now().toString(36)}`; }
function savedSessionId(): string {
  try { const saved = sessionStorage.getItem('echo-guide-session'); if (saved) return saved; } catch { /* 隐私模式下不可用 */ }
  const created = newSessionId();
  try { sessionStorage.setItem('echo-guide-session', created); } catch { /* 同上 */ }
  return created;
}
// 设备标识：比会话活得久，偏好与观演清单都挂在它下面。
function savedProfileId(): string {
  try { const saved = localStorage.getItem('echo-guide-profile'); if (saved) return saved; } catch { /* 隐私模式 */ }
  const created = `p-${Math.random().toString(36).slice(2, 10)}${Date.now().toString(36)}`;
  try { localStorage.setItem('echo-guide-profile', created); } catch { /* 同上 */ }
  return created;
}
function stepPhaseLabel(phase: string) {
  switch (phase) {
    case 'planner': return '规划';
    case 'tool': return '调用';
    case 'observe': return '观察';
    case 'check': return '自检';
    default: return '回答';
  }
}
function loadSaved(key: string): SavedShow[] {
  try {
    const data: unknown = JSON.parse(localStorage.getItem(key) || '[]');
    return Array.isArray(data) ? data.filter((item): item is SavedShow => item && typeof item.id === 'string' && /^\d+$/.test(item.id) && typeof item.name === 'string').slice(0, 100) : [];
  } catch { return []; }
}
function saveLocal(key: string, data: SavedShow[]) { try { localStorage.setItem(key, JSON.stringify(data)); } catch { /* Browsing still works when local storage is unavailable. */ } }
const starters = ['周六，两个人，总预算 600 元，推荐能买票的演出', '有哪些口碑好的演出？', '看看话剧的评分和观众评价', '500 元以内，推荐能买票的演出'];

function savedConversation():{messages:ConversationMessage[];plan?:ShoppingPlan} {
 try {const saved=JSON.parse(sessionStorage.getItem('echo-shopping-conversation')||'null');if(saved&&Date.now()-saved.at<24*3600*1000&&Array.isArray(saved.messages))return {messages:saved.messages.filter((m:ConversationMessage)=>['user','assistant'].includes(m.role)&&typeof m.content==='string').slice(-8),plan:saved.plan};}catch{}
 return {messages:[]};
}

function Brand() {
  return <span className="eg-brand"><span className="eg-wordmark">echo<span>®</span></span><span className="eg-brand-divider" /><span>回响<br /><small>SHOPPING GUIDE</small></span></span>;
}

function ShowCard({ show, index, favorite, compared, onSelect, onFavorite, onCompare, ranking }: { show: Show; index: number; favorite: boolean; compared: boolean; onSelect: (show: Show) => void; onFavorite: (show: Show) => void; onCompare: (show: Show) => void; ranking: boolean }) {
  return <article className="eg-show-card eg-discovery-card">
    <button className="eg-card-open" onClick={() => onSelect(show)} aria-label={`查看${show.name}的评分、评测和票档`}>
      <div className={`eg-poster eg-poster-${index % 4}`}>
        {show.poster_url && <img className="eg-real-poster" src={safeEchoURL(show.poster_url, '')} alt="" loading="lazy" onError={event => { event.currentTarget.style.display = 'none'; }} />}
        <span className="eg-poster-label">{ranking ? `REPUTATION / ${String(index + 1).padStart(2,'0')}` : `ECHO SELECT / ${String(index + 1).padStart(2,'0')}`}</span><span className="eg-poster-type">{show.type_name.includes('剧') ? 'THEATRE' : show.type_name.includes('乐') ? 'MUSIC' : 'LIVE'}</span>
        <span className="eg-poster-circle" /><span className="eg-poster-line" /><strong>{show.name}</strong><span className="eg-poster-footer">LIVE IN THE MOMENT <span>↗</span></span>
      </div>
      <div className="eg-show-copy"><div className="eg-card-labels"><span className="eg-genre">{show.type_name || '现场演出'}</span><span className={`eg-lifecycle eg-lifecycle-${show.lifecycle}`}>{show.lifecycle === 'ended' ? '已结束' : show.lifecycle === 'upcoming' ? '即将开演' : '演出中 / 待确认'}</span></div><h3>{show.name}</h3><p>{show.area || show.address ? `${show.area || '区域待公布'}${show.address && show.address !== show.area ? ` · ${show.address}` : ''}` : '区域与场馆待公布'}</p><p className="eg-show-time">{dateLabel(show.open_time)}</p>
        <div className="eg-card-rating"><strong>★ {ratingLabel(show.rating)}</strong><span>{show.rating.count} 人评分</span>{show.rating.count > 0 && show.rating.low_sample && <small>样本少</small>}</div>
        <div className="eg-show-bottom"><span>{show.min_price !== null ? <><strong>¥{money(show.min_price)}</strong><small>{show.lifecycle === 'ended' ? ' 历史票档起' : ' 票档起'}</small></> : <small>票档价格待确认</small>}</span><span className="eg-card-arrow">↗</span></div>
      </div>
    </button>
    <div className="eg-card-actions"><button aria-pressed={favorite} onClick={() => onFavorite(show)}>{favorite ? '♥ 已收藏' : '♡ 收藏'}</button><button aria-pressed={compared} onClick={() => onCompare(show)}>{compared ? '✓ 已加入对比' : '+ 加入对比'}</button></div>
  </article>;
}

// URL 参数让筛选可以分享和刷新：?view=ranking&type=话剧&city=上海&from=…
const urlParams = new URLSearchParams(window.location.search);
const urlGet = (key: string) => urlParams.get(key) || '';

export function GuidePage({ onManage }: { onManage: () => void }) {
  const [query, setQuery] = useState(() => urlGet('q'));
  const [search, setSearch] = useState(() => urlGet('q'));
  const [category, setCategory] = useState(() => urlGet('type'));
  const [city, setCity] = useState(() => urlGet('city'));
  const [lifecycle, setLifecycle] = useState(() => urlGet('lc') || 'all');
  const [sort, setSort] = useState(() => urlGet('sort') || 'popular');
  const [minimumScore, setMinimumScore] = useState(() => urlGet('score') || '0');
  const [priceLimit, setPriceLimit] = useState(() => urlGet('max'));
  const [budget, setBudget] = useState('');
  const [availableOnly, setAvailableOnly] = useState(() => urlGet('avail') === '1');
  const [dateFrom, setDateFrom] = useState(() => urlGet('from'));
  const [dateTo, setDateTo] = useState(() => urlGet('to'));
  const [mode, setMode] = useState(() => urlGet('view') || 'discover');
  const [page, setPage] = useState(1);
  const [catalog, setCatalog] = useState<CatalogPage>();
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  const [retry, setRetry] = useState(0);
  const [favorites, setFavorites] = useState<SavedShow[]>(() => loadSaved('echo-guide-favorites'));
  const [compared, setCompared] = useState<SavedShow[]>(() => loadSaved('echo-guide-comparison').slice(0,4));
  const [comparisonOpen, setComparisonOpen] = useState(false);
  const [notice, setNotice] = useState('');
  const [selected, setSelected] = useState<Show>();
  const [discussing, setDiscussing] = useState<Show>();
  const [echoHome, setEchoHome] = useState('http://127.0.0.1:3002');
  const [aiEnabled, setAIEnabled] = useState(false);
  const [agentEnabled, setAgentEnabled] = useState(false);
  const [agentMode, setAgentMode] = useState('');
  const [useAgent, setUseAgent] = useState(true);
  const [sessionId, setSessionId] = useState(() => savedSessionId());
  const [profileId] = useState(() => savedProfileId());
  const [actionsBusy, setActionsBusy] = useState('');
  const [managementEnabled, setManagementEnabled] = useState(false);
  const [message, setMessage] = useState('');
  const [messages, setMessages] = useState<ChatEntry[]>(() => savedConversation().messages);
  const [plan,setPlan]=useState<ShoppingPlan|undefined>(()=>savedConversation().plan);
  const [recommendations, setRecommendations] = useState<Recommendation[]>([]);
  const [alternatives, setAlternatives] = useState<Recommendation[]>([]);
  const [chatState, setChatState] = useState('ready');
  const [chatError, setChatError] = useState('');
  const [answerMode, setAnswerMode] = useState('');
  const composer = useRef<HTMLTextAreaElement>(null);
  const conversation = useRef<HTMLDivElement>(null);
  const chatController = useRef<AbortController | null>(null);
  const requestInFlight = useRef(false);
  const refreshRequested = useRef(false);
  const numericBudget = Number(budget) > 0 ? Number(budget) : 0;
  const favoriteIDs = favorites.map(item => item.id).join(',');
  const filters = useMemo<DiscoveryFilters>(() => ({ query: search, category, city, lifecycle, sort: mode === 'ranking' ? 'reputation' : sort, min_score: mode === 'ranking' ? Math.max(Number(minimumScore),1) : Number(minimumScore), budget: Number(priceLimit) || 0, available_only: availableOnly, date_from: dateFrom, date_to: dateTo, ...(mode === 'favorites' ? { ids: favoriteIDs || '0' } : {}) }), [search, category, city, lifecycle, sort, minimumScore, priceLimit, availableOnly, dateFrom, dateTo, mode, favoriteIDs]);

  useEffect(() => {
    const controller = new AbortController();
    ticketingAPI.config(controller.signal).then(config => { setEchoHome(safeEchoURL(config.echo_home, 'http://127.0.0.1:3002')); setAIEnabled(config.ai_enabled); setManagementEnabled(config.management_enabled); setAgentEnabled(config.agent?.enabled ?? false); setAgentMode(config.agent?.mode ?? ''); }).catch(() => {});
    return () => { controller.abort(); chatController.current?.abort(); };
  }, []);
  useEffect(() => {
    const controller = new AbortController(); setState('loading'); setError('');
    const refresh = refreshRequested.current; refreshRequested.current = false;
    ticketingAPI.discover(filters, page, controller.signal, refresh).then(data => { setCatalog(data); setState('ready'); }).catch(error => {
      if (!controller.signal.aborted) { setError(error.message); setState('error'); }
    });
    return () => controller.abort();
  }, [filters, page, retry]);
  useEffect(() => { const node = conversation.current; if (node) node.scrollTop = node.scrollHeight; }, [messages, chatState]);
  useEffect(() => { saveLocal('echo-guide-favorites', favorites); }, [favorites]);
  useEffect(() => { saveLocal('echo-guide-comparison', compared); }, [compared]);
  // 筛选变化写进地址栏（不产生历史记录），刷新和分享都保留当前视图。
  useEffect(() => {
    const params = new URLSearchParams();
    const put = (key: string, value: string, fallback: string) => { if (value && value !== fallback) params.set(key, value); };
    put('view', mode, 'discover'); put('q', search, ''); put('type', category, ''); put('city', city, '');
    put('lc', lifecycle, 'all'); put('sort', sort, 'popular'); put('score', minimumScore, '0');
    put('max', priceLimit, ''); put('avail', availableOnly ? '1' : '', ''); put('from', dateFrom, ''); put('to', dateTo, '');
    const qs = params.toString();
    window.history.replaceState(null, '', qs ? `?${qs}` : window.location.pathname);
  }, [mode, search, category, city, lifecycle, sort, minimumScore, priceLimit, availableOnly, dateFrom, dateTo]);

  function updateFilter(action: () => void) { setPage(1); action(); }
  function resetFilters() { setQuery(''); setSearch(''); setCategory(''); setCity(''); setLifecycle('all'); setMinimumScore('0'); setPriceLimit(''); setAvailableOnly(false); setDateFrom(''); setDateTo(''); setPage(1); }
  function submitSearch(event: FormEvent) { event.preventDefault(); updateFilter(() => setSearch(query.trim())); }
  function toggleFavorite(show: Show) { setFavorites(items => items.some(item => item.id === show.show_id) ? items.filter(item => item.id !== show.show_id) : [...items, { id: show.show_id, name: show.name }].slice(-100)); }
  function toggleCompare(show: Show) { setNotice(''); if (compared.some(item => item.id === show.show_id)) setCompared(items => items.filter(item => item.id !== show.show_id)); else if (compared.length >= 4) setNotice('最多同时比较 4 场演出，请先移除一个。'); else setCompared(items => [...items, { id: show.show_id, name: show.name }]); }
  function discuss(show: Show) { setDiscussing(show); setSelected(undefined); setMessage(`帮我分析《${show.name}》的评分、观众评测和需要注意的地方。`); window.setTimeout(() => { composer.current?.scrollIntoView({ behavior: 'smooth', block: 'center' }); composer.current?.focus(); }, 0); }
  // 流式提问：真实数据的回答先逐段出现（推荐列表同时就位），模型随后在
  // 同一条消息里接续润色；模型失败时数据回答保留，等待永远不挡内容。
  async function send(text = message, compareIDs?: string[]) {
    text = text.trim(); if (!text || requestInFlight.current) return;
    requestInFlight.current = true;
    const controller = new AbortController(); chatController.current = controller;
    // 历史只回传角色与正文：推理步骤是展示用的，不该占用模型上下文。
    const history = messages.slice(-8).map(({ role, content }) => ({ role, content }));
    const agentOn = useAgent && agentEnabled;
    setMessages(items => [...items, { role: 'user', content: text }, { role: 'assistant', content: '', steps: agentOn ? [] : undefined, agentMode: agentOn ? agentMode : undefined }]);
    const assistantIndex = messages.length + 1;
    let streamed = '';
    let finalMode = '';
    let phaseNote = '';
    let steps: AgentStep[] = [];
    const patchAssistant = (patch: Partial<ChatEntry>) => setMessages(items => items.map((item, index) => index === assistantIndex ? { ...item, role: 'assistant' as const, ...patch } : item));
    try {
      const response = await fetch(agentOn ? '/api/v1/ticketing/agent/stream' : '/api/v1/ticketing/assist/stream', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        signal: controller.signal,
        body: JSON.stringify({ session_id: sessionId, profile_id: profileId, plan, message: text, history, show_id: compareIDs ? undefined : discussing?.show_id, query: search, budget: numericBudget, filters: { ...filters, ids: undefined }, compare_ids: compareIDs })
      });
      if (!response.ok || !response.body) throw new Error(`HTTP ${response.status}`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      const appendDelta = (delta: string) => {
        streamed += delta;
        setMessages(items => items.map((item, index) => index === assistantIndex ? { ...item, role: 'assistant' as const, content: streamed } : item));
      };
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let split: number;
        while ((split = buffer.indexOf('\n\n')) >= 0) {
          const raw = buffer.slice(0, split); buffer = buffer.slice(split + 2);
          for (const line of raw.split('\n')) {
            if (!line.startsWith('data: ')) continue;
            let event: any; try { event = JSON.parse(line.slice(6)); } catch { continue; }
            if (event.type === 'action' && event.action) {
              const action = event.action as AgentAction;
              setMessages(items => items.map((item, index) => index === assistantIndex ? { ...item, actions: [...(item.actions || []), action] } : item));
            }
            else if (event.type === 'step' && event.step) {
              steps = [...steps, event.step as AgentStep];
              patchAssistant({ steps, agentMode: agentMode || 'agent-rule' });
            }
            else if (event.type === 'meta') { setRecommendations(event.items || []); setAlternatives(event.alternatives || []); setPlan(event.plan); setAnswerMode(agentOn ? (event.mode || 'agent-rule') : 'catalog'); }
            else if (event.type === 'delta') { if (event.source === 'model') { if (streamed && !phaseNote) { streamed = ''; phaseNote = 'model'; } } appendDelta(event.text || ''); }
            else if (event.type === 'phase') { setChatState('polishing'); }
            else if (event.type === 'final') {
              finalMode = event.mode || 'catalog';
              const answer = event.note ? `${event.answer}\n\n（${event.note}）` : event.answer;
              patchAssistant({ content: answer, steps, agentMode: event.mode || agentMode, actions: event.actions && event.actions.length ? event.actions : undefined, resumableRunID: event.resumable ? event.run_id : undefined });
              setAnswerMode(finalMode); setChatState('ready');
              try{sessionStorage.setItem('echo-shopping-conversation',JSON.stringify({at:Date.now(),messages:[...history,{role:'user',content:text},{role:'assistant',content:answer}].slice(-8),plan:event.plan}));}catch{}
            }
          }
        }
      }
      if (!finalMode) { setChatState('ready'); setChatError('连接中断，请重试'); }
    } catch (error) {
      if (!controller.signal.aborted) {
        // 流式不可用时退回普通接口，行为与旧版一致。
        try {
          const response = agentOn
            ? await ticketingAPI.agent({ session_id: sessionId, profile_id: profileId, plan, message: text, history, show_id: compareIDs ? undefined : discussing?.show_id, query: search, budget: numericBudget, filters: { ...filters, ids: undefined }, compare_ids: compareIDs }, controller.signal)
            : await ticketingAPI.assist({ plan, message: text, history, show_id: compareIDs ? undefined : discussing?.show_id, query: search, budget: numericBudget, filters: { ...filters, ids: undefined }, compare_ids: compareIDs }, controller.signal);
          const agentSteps = 'steps' in response && Array.isArray(response.steps) ? response.steps : undefined;
          patchAssistant({ content: response.answer, steps: agentSteps, agentMode: 'mode' in response ? response.mode : agentMode });
          setRecommendations(response.items); setAlternatives('alternatives' in response ? response.alternatives || [] : []); setAnswerMode(response.mode); setPlan(response.plan);
          try{sessionStorage.setItem('echo-shopping-conversation',JSON.stringify({at:Date.now(),messages:[...history,{role:'user',content:text},{role:'assistant',content:response.answer}].slice(-8),plan:response.plan}));}catch{}
        } catch (fallbackError) {
          setChatError(fallbackError instanceof Error ? fallbackError.message : '回复失败，请稍后再试'); setMessage(text);
          setMessages(items => items.filter((_, index) => index !== assistantIndex));
        }
      }
    }
    finally { if (!controller.signal.aborted) setChatState('ready'); requestInFlight.current = false; }
  }
  // 写操作必须由用户点确认；这里只做「调用 + 原地更新卡片」，失败不影响对话。
  async function decideAction(id: string, approve: boolean) {
    if (actionsBusy) return;
    setActionsBusy(id);
    try {
      const data = (approve ? await ticketingAPI.confirmAction(id) : await ticketingAPI.rejectAction(id)) as {
        action: AgentAction;
        watchlist?: { show_id: number; show_name: string }[];
      };
      setMessages(items => items.map(item => item.actions ? { ...item, actions: item.actions.map(action => action.id === id ? data.action : action) } : item));
      if (approve && Array.isArray(data.watchlist)) {
        setNotice(`观演清单已更新：共 ${data.watchlist.length} 场`);
      }
    } catch (error) {
      setChatError(error instanceof Error ? error.message : '操作失败，请稍后再试');
    } finally {
      setActionsBusy('');
    }
  }
  // 断点续跑：上一轮推理没跑完（超时或断连），点这里接着跑；
  // 已查到的结果由服务端快照复用，不会重新查询。
  async function resumeRun(runID: string) {
    if (requestInFlight.current) return;
    requestInFlight.current = true;
    setChatState('loading'); setChatError('');
    setMessages(items => [...items, { role: 'user', content: '继续上次没跑完的推理' }, { role: 'assistant', content: '', steps: [], agentMode: 'resumed' }]);
    const assistantIndex = messages.length + 1;
    let steps: AgentStep[] = [];
    const patch = (patchData: Partial<ChatEntry>) => setMessages(items => items.map((item, index) => index === assistantIndex ? { ...item, role: 'assistant' as const, ...patchData } : item));
    try {
      const response = await fetch(`/api/v1/ticketing/agent/resume/${encodeURIComponent(runID)}/stream`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionId, profile_id: profileId })
      });
      if (!response.ok || !response.body) throw new Error(`HTTP ${response.status}`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      let streamed = '';
      let finished = false;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let split: number;
        while ((split = buffer.indexOf('\n\n')) >= 0) {
          const raw = buffer.slice(0, split); buffer = buffer.slice(split + 2);
          for (const line of raw.split('\n')) {
            if (!line.startsWith('data: ')) continue;
            let event: any; try { event = JSON.parse(line.slice(6)); } catch { continue; }
            if (event.type === 'step' && event.step) {
              steps = [...steps, event.step as AgentStep];
              patch({ steps, agentMode: 'resumed' });
            }
            else if (event.type === 'delta') { streamed += event.text || ''; patch({ content: streamed }); }
            else if (event.type === 'meta') {
              setRecommendations(event.items || []); setPlan(event.plan); setAnswerMode(event.mode || 'agent-rule');
            }
            else if (event.type === 'final') {
              finished = true;
              const answer = event.note ? `${event.answer}\n\n（${event.note}）` : event.answer;
              patch({ content: answer, steps, agentMode: event.mode || 'agent-rule', actions: event.actions && event.actions.length ? event.actions : undefined, resumableRunID: event.resumable ? event.run_id : undefined });
              setAnswerMode(event.mode || 'agent-rule'); setChatState('ready');
            }
            else if (event.type === 'error') { setChatError(event.message || '续跑失败，请稍后再试'); }
          }
        }
      }
      if (!finished) { setChatState('ready'); setChatError('连接中断，请稍后再试'); }
    } catch (error) {
      setChatError(error instanceof Error ? error.message : '续跑失败，请稍后再试');
    }
    finally { setChatState('ready'); requestInFlight.current = false; }
  }
  function newConversation(){if(requestInFlight.current)return;const next= newSessionId();setSessionId(next);try{sessionStorage.setItem('echo-guide-session',next)}catch{};setMessages([]);setPlan(undefined);setRecommendations([]);setAlternatives([]);setDiscussing(undefined);setChatError('');setBudget('');try{sessionStorage.removeItem('echo-shopping-conversation')}catch{}}
  const coverage = catalog?.coverage;
  const hasFilters = !!(search || category || city || lifecycle !== 'all' || minimumScore !== '0' || priceLimit || availableOnly || dateFrom || dateTo);

  return <div className="eg-page">
    <a className="eg-skip-link" href="#eg-discover">跳到演出列表</a>
    <header className="eg-header"><div className="eg-header-inner"><a href="#" className="eg-logo-link" aria-label="Echo 回响导购首页"><Brand /></a><nav aria-label="主导航"><a className="eg-nav-current" href="#eg-discover">发现与口碑</a><a href={`${echoHome.replace(/\/$/, '')}/guide`}>我的购票与评测</a><a href="#eg-assistant">选票助手</a></nav><a className="eg-echo-link" href={echoHome} target="_blank" rel="noopener noreferrer">回到 Echo <span>↗</span></a></div></header>
    <main className="eg-main">
      <section className="eg-hero"><div><p className="eg-eyebrow"><span className="eg-dot" /> GOOD SHOWS. REAL VOICES.</p><h1>听过回响，<br />再赴<span>现场。</span></h1><p className="eg-hero-description">不只看票价，也听听去过的人怎么说。<br />从评分、真实评测到票档，把喜欢选得更明白。</p><a className="eg-hero-action" href="#eg-discover">发现演出与口碑 <span>↓</span></a></div><div className="eg-hero-art" aria-hidden="true"><div className="eg-art-top"><span>ECHO PRESENTS</span><span>EVERY VOICE MATTERS</span></div><div className="eg-orbits"><i /><i /><i /><i /><span>e.</span></div><div className="eg-art-caption"><strong>Find your show.<br />Hear the echo.</strong><span>好现场<br />有回响</span></div><div className="eg-ticket-stub"><span>DISCOVER / READ / COMPARE</span><span className="eg-barcode" /></div></div></section>
      <section className="eg-catalog-overview" aria-label="演出数据概览"><div><strong>{coverage?.loaded ?? '—'}</strong><span>公开演出</span></div><div><strong>{coverage?.active ?? '—'}</strong><span>未结束场次</span></div><div><strong>{coverage?.ended ?? '—'}</strong><span>历史演出</span></div><div><strong>{coverage?.review_count ?? '—'}</strong><span>演出关联评测 · 含纯评分</span></div><button onClick={() => { refreshRequested.current = true; setRetry(value => value + 1); }} disabled={state === 'loading'}>更新数据 ↻</button></section>
      {coverage && <div className="eg-coverage-note"><span>{coverage.complete ? `已读取 Echo 全部 ${coverage.public_total} 场公开演出` : `已读取 ${coverage.loaded} / ${coverage.public_total} 场，数据尚未完整`}</span><time>更新于 {dateLabel(coverage.updated_at)}</time></div>}
      {!!coverage?.warnings.length && <details className="eg-coverage-warnings"><summary>有 {coverage.warnings.length} 项信息暂未读取完整</summary>{coverage.warnings.map((warning,i) => <p key={i}>{warning}</p>)}</details>}
      <div className="eg-content-grid"><section id="eg-discover" className="eg-discover">
        <div className="eg-view-tabs" role="group" aria-label="浏览内容">{[['discover','全部演出'],['ranking','口碑榜'],['reviews','观众评测'],['favorites',`我的收藏 ${favorites.length || ''}`]].map(([value,label]) => <button key={value} aria-pressed={mode === value} onClick={() => updateFilter(() => setMode(value))}>{label}</button>)}</div>
        {mode === 'reviews' ? <ReviewFeed onSelect={id => { void ticketingAPI.detail(id).then(data => setSelected(data.show)).catch(error => setNotice(error.message)); }} /> : <>
        {mode === 'ranking' && <div className="eg-ranking-explanation"><strong>评分多的优先</strong><p>先按评分人数分层，同层内按样本修正分排序，少数五星压不过多数人的评分。<details><summary>排序方式</summary><p>参考分 =（评分总和 + 5 × 3.5）÷（评分数 + 5）；卡片展示真实均分，少于 5 个评分标记“样本少”。</p></details></p></div>}
        <form className="eg-search" onSubmit={submitSearch}><span aria-hidden="true">⌕</span><input aria-label="搜索演出、城市或场馆" placeholder="演出名、城市、场馆，多个词用空格分开" value={query} onChange={event => setQuery(event.target.value)} maxLength={100} /><button type="submit">搜索</button></form>
        <div className="eg-categories" role="group" aria-label="演出类型">{['', ...(catalog?.categories || [])].map(value => <button key={value} aria-pressed={category === value} className={category === value ? 'eg-category-active' : ''} onClick={() => updateFilter(() => setCategory(value))}>{value || '全部类型'}</button>)}</div>
        <div className="eg-lifecycle-options" role="group" aria-label="演出状态">{[['all','全部场次'],['active','未结束'],['ended','历史回顾']].map(([value,label]) => <button key={value} aria-pressed={lifecycle === value} onClick={() => updateFilter(() => setLifecycle(value))}>{label}</button>)}<label><input type="checkbox" checked={availableOnly} onChange={event => updateFilter(() => setAvailableOnly(event.target.checked))} />只看有余票</label></div>
        <div className="eg-filter-grid"><label>城市 / 区域<select value={city} onChange={event => updateFilter(() => setCity(event.target.value))}><option value="">不限</option>{(catalog?.cities || []).map(value => <option key={value} value={value}>{cityLabel(value)}</option>)}</select></label><label>观众评分<select value={minimumScore} onChange={event => updateFilter(() => setMinimumScore(event.target.value))}><option value="0">不限</option><option value="3">3 分及以上</option><option value="4">4 分及以上</option><option value="4.5">4.5 分及以上</option></select></label><label>票价上限<select value={priceLimit} onChange={event => updateFilter(() => setPriceLimit(event.target.value))}><option value="">不限</option><option value="200">¥200</option><option value="500">¥500</option><option value="1000">¥1000</option></select></label></div>
        <details className="eg-date-filters"><summary>按开演日期筛选</summary><div><label>从<input type="date" value={dateFrom} max={dateTo || undefined} onChange={event => updateFilter(() => setDateFrom(event.target.value))} /></label><label>至<input type="date" value={dateTo} min={dateFrom || undefined} onChange={event => updateFilter(() => setDateTo(event.target.value))} /></label></div></details>
        <div className="eg-list-toolbar"><span role="status">{state === 'loading' ? '正在读取完整演出、票档和评测…' : state === 'error' ? '演出信息暂未连接' : `${catalog?.total ?? 0} 场匹配 · 第 ${page} 页`}</span><div>{hasFilters && <button className="eg-reset-filters" onClick={resetFilters}>清除筛选</button>}{mode !== 'ranking' && <select aria-label="演出排序" value={sort} onChange={event => updateFilter(() => setSort(event.target.value))}><option value="popular">热门优先</option><option value="reputation">综合口碑</option><option value="rating">评测均分</option><option value="discussed">评分人数</option><option value="price">票价从低到高</option><option value="soon">开演时间</option></select>}</div></div>
        {state === 'loading' && <div className="eg-show-grid" aria-busy="true">{[0,1,2,3].map(i => <div className="eg-skeleton" key={i}><div /><span /><span /></div>)}</div>}
        {state === 'error' && <div className="eg-empty" role="alert"><span className="eg-empty-symbol">e.</span><h3>稍等一下，好现场还在</h3><p>{error}</p><button className="eg-secondary" onClick={() => setRetry(value => value + 1)}>重新连接 ↻</button></div>}
        {state === 'ready' && !catalog?.items.length && <div className="eg-empty"><span className="eg-empty-symbol">↗</span><h3>{mode === 'favorites' && !favorites.length ? '先收藏几场心动的演出' : '再换一种寻找方式'}</h3><p>{mode === 'favorites' && !favorites.length ? '在演出卡片上点“收藏”，下次打开还能找到。' : '当前筛选下没有匹配的演出。历史评测可在“全部场次”查看。'}</p><button className="eg-secondary" onClick={() => { resetFilters(); setMode('discover'); }}>查看全部演出</button></div>}
        {state === 'ready' && !!catalog?.items.length && <div className="eg-show-grid">{catalog.items.map((show,index) => <ShowCard key={show.show_id} show={show} index={(page-1)*12+index} favorite={favorites.some(item => item.id === show.show_id)} compared={compared.some(item => item.id === show.show_id)} onSelect={setSelected} onFavorite={toggleFavorite} onCompare={toggleCompare} ranking={mode === 'ranking'} />)}</div>}
        {state === 'ready' && (page > 1 || catalog?.has_more) && <div className="eg-pagination"><button disabled={page <= 1} onClick={() => { setPage(value => value-1); document.getElementById('eg-discover')?.scrollIntoView({ behavior: 'smooth' }); }}>← 上一页</button><span>第 {page} / {Math.max(1,Math.ceil((catalog?.total || 0)/12))} 页</span><button disabled={!catalog?.has_more} onClick={() => { setPage(value => value+1); document.getElementById('eg-discover')?.scrollIntoView({ behavior: 'smooth' }); }}>下一页 →</button></div>}
        </>}
      </section>
      <aside className="eg-assistant" id="eg-assistant"><div className="eg-assistant-top"><span className="eg-assistant-icon">✳</span><div><h2>你的选票与口碑搭子</h2><p>{aiEnabled ? '结合真实评测，帮你把选择想清楚' : '从真实评分和票档出发'}</p></div><span className="eg-status-dot" /></div><div className="eg-assistant-body">
        <div className="eg-assistant-welcome"><span className="eg-eyebrow">EVERY GOOD CHOICE HAS A REASON</span><h3>喜欢，也要有依据。</h3><p>问口碑、比价格、看不同意见，一句话就够。</p></div>
        <label className="eg-budget">每人预算<span>¥ <input type="number" inputMode="decimal" aria-label="咨询预算上限（元）" min="0" max="1000000" placeholder="不限" value={budget} onChange={event => setBudget(event.target.value)} /></span></label>{agentEnabled && <button type="button" className={`eg-agent-toggle${useAgent ? ' is-on' : ''}`} aria-pressed={useAgent} onClick={() => setUseAgent(value => !value)} title={agentMode === 'agent-react' ? '模型自己决定调用哪些工具，能看到每一步' : '没有模型时用规则编排工具链，同样能看到每一步'}><span className="eg-agent-toggle-dot" />多步推理<span className="eg-agent-toggle-mode">{agentMode === 'agent-react' ? '模型编排' : '规则编排'}</span></button>}
        {plan && <section className="eg-plan" aria-label="已记住的购票需求"><strong>本次需求</strong><p>{plan.party_size} 人{plan.budget>0?` · ${plan.budget_mode==='total'?'总预算':'每人预算'} ¥${money(plan.budget)}`:' · 预算不限'}</p><p>{[plan.filters.city&&cityLabel(plan.filters.city),plan.filters.category,plan.filters.date_from&&`${plan.filters.date_from} 至 ${plan.filters.date_to}`,plan.seat_preference==='front'?'偏好前排':plan.seat_preference==='adjacent'?'偏好连座':''].filter(Boolean).join(' · ')||'城市与日期不限'}</p><small>可直接说“改成三个人”“预算 800”“想坐近一点”；座位在 Echo 确认。</small></section>}
        {(messages.length > 0 || plan) && <div className="eg-conversation-tools"><span>对话只保存在本标签页</span><button className="eg-new-conversation" disabled={chatState === 'loading'} onClick={newConversation}>开始新的需求 ↻</button></div>}
        <div className="eg-starters">{starters.map(text => <button key={text} disabled={chatState === 'loading'} onClick={() => send(text)}>{text}<span>↗</span></button>)}</div>
        {discussing && <div className="eg-context"><span>正在聊：{discussing.name}</span><button aria-label="取消演出上下文" onClick={() => setDiscussing(undefined)}>×</button></div>}
        <div className="eg-conversation" role="log" aria-live="polite" aria-label="选票对话" ref={conversation}>{messages.map((item,i) => <div key={i} className={`eg-chat-message eg-chat-${item.role}`}><span>{item.role === 'user' ? '你' : 'echo'}</span>{item.steps && item.steps.length > 0 && <details className="eg-agent-trace"><summary><span className="eg-agent-badge">AGENT</span>{item.steps.length} 步推理 · {item.agentMode === 'agent-react' ? '模型编排工具' : '规则编排工具'}</summary><ol>{item.steps.map((step, index) => <li key={index} className={`eg-step eg-step-${step.phase}${step.ok === false ? ' eg-step-fail' : ''}`}><span className="eg-step-phase">{stepPhaseLabel(step.phase)}</span><span className="eg-step-title">{step.title}</span>{step.detail && <small>{step.detail}</small>}</li>)}</ol></details>}{item.actions && item.actions.length > 0 && <div className="eg-agent-actions">{item.actions.map(action => <div key={action.id} className={`eg-agent-action eg-agent-action-${action.status}`}><div className="eg-agent-action-head"><strong>{action.title}</strong><span>{action.status === 'pending' ? '待确认' : action.status === 'confirmed' ? '已执行' : action.status === 'rejected' ? '已取消' : '已过期'}</span></div><p>{action.summary}</p>{action.status === 'pending' ? <div className="eg-agent-action-buttons"><button type="button" className="eg-agent-ok" disabled={actionsBusy === action.id} onClick={() => decideAction(action.id, true)}>确认执行</button><button type="button" className="eg-agent-cancel" disabled={actionsBusy === action.id} onClick={() => decideAction(action.id, false)}>取消</button><small>确认后才会真正写入</small></div> : <small className="eg-agent-action-result">{action.result || ''}</small>}</div>)}</div>}{item.resumableRunID && chatState === 'ready' && <button type="button" className="eg-agent-resume" onClick={() => resumeRun(item.resumableRunID!)}>继续上次推理</button>}<p>{item.content}</p></div>)}{chatState === 'loading' && <p className="eg-chat-loading" role="status">{useAgent && agentEnabled ? '正在规划需求、调用工具并核对票档…' : '正在查询真实演出、评分与原文评测…'}</p>}{chatState === 'polishing' && <p className="eg-chat-loading" role="status">数据结果已在上方，模型正在接续润色…</p>}</div>
        {chatError && <p role="alert" className="eg-chat-error">{chatError}</p>}
        {recommendations.length > 0 && <div className="eg-chat-recommendations">{recommendations.map(item => <div className="eg-action-recommendation" key={item.show.show_id}><button onClick={() => setSelected(item.show)}><span>{item.show.name}</span><span>查看口碑 / 票档 ↗</span></button><p className="eg-recommendation-sample">{item.reviews.summary.count} 个有效评分 · {item.reviews.summary.complete?'已完整读取':'样本未读完整'}{item.reviews.summary.low_sample?' · 样本少':''}</p><details><summary>为什么推荐 · 口碑依据</summary><ul>{item.reasons.map((reason,i)=><li key={i}>{reason}</li>)}</ul><div>{[['positive','高分评测提及'],['critical','中低分评测提及']].map(([kind,label])=><div key={kind}><strong>{label}</strong>{(kind==='positive'?item.reviews.insights?.positive:item.reviews.insights?.critical)?.slice(0,2).map(topic=><p key={topic.name}>{topic.name} · {topic.count} 人{topic.count>=2?'反复提及':'的个人意见'}{topic.quotes.slice(0,1).map(quote=><a key={quote.review_id} href={safeEchoURL(quote.detail_url,`${echoHome}/review/${quote.review_id}`)} target="_blank" rel="noopener noreferrer">“{quote.content.slice(0,90)}” · 原文 #{quote.review_id} ↗</a>)}</p>)}</div>)}</div>{!!item.keywords?.length && <p>观众反复提到：{item.keywords.join('、')}</p>}{!!item.related?.length && item.related.map(quote => <p key={quote.review_id}>与提问相关 #{quote.review_id} · {quote.author}（{quote.score} 星）：「{quote.excerpt}」</p>)}{!item.reviews.insights?.critical?.length&&<p>暂无可引用的中低分文字意见，不代表没有争议。</p>}<ul>{item.cautions.map((caution,i)=><li key={i}>{caution}</li>)}</ul><small>票档查询：{dateLabel(item.updated_at)}</small></details>{item.show.lifecycle !== 'ended' && item.tickets.filter(tier => tier.on_sale && !tier.sold_out && (!tier.is_seckill || tier.seckill_active)).slice(0,2).map(tier => <a key={tier.ticket_id} href={checkoutLink(tier, plan, echoHome)}>{tier.title} · ¥{money(tier.price)} / 张{plan&&plan.party_size>1?` · ${plan.party_size} 张 ¥${money(tier.price*plan.party_size)}`:''}<span>确认订单 ↗</span></a>)}</div>)}</div>}
        {alternatives.length > 0 && <section className="eg-chat-alternatives" aria-label="条件放宽后的备选"><p className="eg-alternatives-note">没有完全匹配的演出，以下是放宽日期或预算后的备选。</p><div className="eg-chat-recommendations">{alternatives.map(item => <div className="eg-action-recommendation" key={`alt-${item.show.show_id}`}><button onClick={() => setSelected(item.show)}><span>{item.show.name}</span><span>查看口碑 / 票档 ↗</span></button><p className="eg-recommendation-sample">{item.reviews.summary.count} 个有效评分 · {item.reviews.summary.complete?'已完整读取':'样本未读完整'}{item.reviews.summary.low_sample?' · 样本少':''}</p><details><summary>为什么是备选 · 条件差异</summary><ul>{item.reasons.slice(0,3).map((reason,i)=><li key={i}>{reason}</li>)}{item.cautions.slice(0,2).map((caution,i)=><li key={`c-${i}`}>{caution}</li>)}</ul></details>{item.tickets.filter(tier => tier.on_sale && !tier.sold_out && (!tier.is_seckill || tier.seckill_active)).slice(0,1).map(tier => <a key={tier.ticket_id} href={checkoutLink(tier, plan, echoHome)}>{tier.title} · ¥{money(tier.price)} / 张<span>确认订单 ↗</span></a>)}</div>)}</div></section>}
        <form className="eg-composer" onSubmit={event => { event.preventDefault(); void send(); }}><textarea ref={composer} aria-label="向选票助手提问" placeholder="例如：哪场话剧口碑好，有哪些不同意见？" maxLength={1500} value={message} onChange={event => setMessage(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void send(); } }} /><button aria-label="发送问题" type="submit" disabled={!message.trim() || chatState === 'loading'}>↑</button></form>
        <p className="eg-assistant-note">{answerMode === 'agent-react' ? '模型基于工具查到的事实改写，数字均来自票务数据' : answerMode === 'agent-rule' ? '规则编排 + 工具查询，内容全部来自真实数据' : answerMode === 'catalog' ? '本次根据真实评分、评测和票档整理' : '评测是个人体验，购票前请阅读具体规则'}</p>
      </div><div className="eg-assistant-footer"><span>听过回响，再赴现场。</span><span>echo / guide</span></div></aside></div>
      <section className="eg-bottom-note"><span>01 / 看评分与评测</span><span>02 / 比较心动选择</span><span>03 / 去 Echo 赴约 ↗</span></section>
    </main>
    <footer className="eg-footer"><span>echo 回响 <span>·</span> 让每次选择都有回响</span>{managementEnabled && <button onClick={onManage}>导购管理 ↗</button>}<small>SHOPPING GUIDE</small></footer>
    {notice && <div className="eg-floating-notice" role="status">{notice}<button aria-label="关闭提示" onClick={() => setNotice('')}>×</button></div>}
    {!!compared.length && <div className="eg-compare-tray" aria-label="演出对比清单"><div><strong>对比清单 {compared.length} / 4</strong><div>{compared.map(item => <button key={item.id} onClick={() => setCompared(items => items.filter(value => value.id !== item.id))} aria-label={`移除${item.name}`}>{item.name}<span>×</span></button>)}</div></div><button className="eg-secondary" onClick={() => setCompared([])}>清空</button><button className="eg-primary" disabled={compared.length < 2} onClick={() => setComparisonOpen(true)}>开始对比 ↗</button></div>}
    {selected && <ShowDetails key={selected.show_id} show={selected} budget={numericBudget} plan={plan} onClose={() => setSelected(undefined)} onDiscuss={discuss} />}
    {comparisonOpen && <ComparisonDialog ids={compared.map(item => item.id)} onClose={() => setComparisonOpen(false)} onDiscuss={() => { setComparisonOpen(false); setDiscussing(undefined); void send(`请比较这 ${compared.length} 场演出的口碑、价格、评分样本及需要注意的地方，帮我做选择。`, compared.map(item => item.id)); window.setTimeout(() => composer.current?.scrollIntoView({ behavior:'smooth', block:'center' }),0); }} />}
  </div>;
}
