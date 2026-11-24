import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { AdminError, guideAdminAPI , agentAPI, type AgentActionRow, type AgentCheckpointRow, type AgentRun, type BenchReport, type RagReport } from '../api/guideAdmin';
import type { AdminAskResponse, AdminOverview, AdminSession, CatalogAnalytics, CustomCase, DraftShow, EvalReport, PromptDoc, PromptSaveResult, TierRow } from '../api/guideAdmin';
import { cityLabel } from '../api/ticketing';
import { dateLabel } from './DiscoveryDetails';
import './guide-admin.css';

const TOKEN_KEY = 'echo-guide-admin-token';
type ChatTurn = { question: string; result?: AdminAskResponse; error?: string };
const chatStarters = ['周六，两个人，总预算 600 元，能看什么？', '哪些演出没有票档？', '哪些票档缺退改规则？', '上海的城市数据怎么样？', '有哪些异常低价的票档？'];

function modelLabel(session?: AdminSession) {
  if (!session) return '读取中';
  if (!session.model.enabled) return '未配置模型 · 仅用票务数据回答';
  return `${session.model.provider || '模型'} · ${session.model.model || '默认'}`;
}

function timeLabel(value: string) {
  if (!value) return '尚未记录';
  return dateLabel(value);
}

function levelLabel(level: string) {
  return level === 'ok' ? '正常' : level === 'warn' ? '需留意' : '异常';
}

function priceRange(min: number | null, max: number | null) {
  if (min === null && max === null) return '待公布';
  if (min === null) return `¥${max}`;
  if (max === null || max === min) return `¥${min}`;
  return `¥${min} – ¥${max}`;
}

// Every badge is a fact from Echo's ticket record, not a guess.
function tierBadges(tier: TierRow) {
  const badges: { label: string; tone: string }[] = [];
  if (tier.lifecycle === 'ended') badges.push({ label: '历史票档', tone: '' });
  else if (!tier.on_sale) badges.push({ label: '未上架', tone: '' });
  else if (tier.sold_out) badges.push({ label: '已售罄', tone: 'ga-chip-bad' });
  else badges.push({ label: '可售', tone: 'ga-chip-ok' });
  if (tier.has_seats) badges.push({ label: '需选座', tone: '' });
  if (tier.is_seckill) badges.push({ label: tier.seckill_active ? '秒杀进行中' : '秒杀票', tone: tier.seckill_active ? 'ga-chip-ai' : '' });
  if (tier.stock_source === 'mysql_snapshot') badges.push({ label: '余票为快照', tone: 'ga-chip-bad' });
  if (tier.rules_missing) badges.push({ label: '缺退改规则', tone: 'ga-chip-bad' });
  return badges;
}

export function GuideAdminPage({ onExit }: { onExit: () => void }) {
  const [token, setToken] = useState(() => { try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch { return ''; } });
  const [tokenDraft, setTokenDraft] = useState('');
  const [session, setSession] = useState<AdminSession>();
  const [overview, setOverview] = useState<AdminOverview>();
  const [promptDoc, setPromptDoc] = useState<PromptDoc>();
  const [promptDraft, setPromptDraft] = useState('');
  const [report, setReport] = useState<EvalReport>();
  const [catalog, setCatalog] = useState<CatalogAnalytics>();
  const [cityFilter, setCityFilter] = useState('');
  const [bandFilter, setBandFilter] = useState('');
  const [tierQuery, setTierQuery] = useState('');
  const [onlyAvailable, setOnlyAvailable] = useState(false);
  const [onlyAttention, setOnlyAttention] = useState(false);
  const [visibleTiers, setVisibleTiers] = useState(12);
  const [promptConfirm, setPromptConfirm] = useState<{ mode: 'save' | 'restore' | 'rollback'; version?: number }>();
  const [confirmToken, setConfirmToken] = useState('');
  const [confirmError, setConfirmError] = useState('');
  const [lastEval, setLastEval] = useState<EvalReport>();
  const [chat, setChat] = useState<ChatTurn[]>(() => {
    try { const saved = JSON.parse(sessionStorage.getItem('ga-chat') || '[]'); return Array.isArray(saved) ? saved.slice(-10) : []; } catch { return []; }
  });
  const [chatInput, setChatInput] = useState('');
  const [useModel, setUseModel] = useState(false);
  const [asking, setAsking] = useState(false);
  const [state, setState] = useState<'loading' | 'gate' | 'ready' | 'error'>('loading');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [refreshing, setRefreshing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState(false);
  const [agentRuns, setAgentRuns] = useState<AgentRun[]>([]);
  const [agentMode, setAgentMode] = useState('');
  const [agentOpenRun, setAgentOpenRun] = useState('');
  const [agentCheckpoints, setAgentCheckpoints] = useState<AgentCheckpointRow[]>([]);
  const [agentActions, setAgentActions] = useState<AgentActionRow[]>([]);
  const [benchData, setBenchData] = useState<{ report: BenchReport; mode: string }>();
  const [ragData, setRagData] = useState<{ report: RagReport; entries: number }>();
  const [agentBusy, setAgentBusy] = useState('');
  const [cases, setCases] = useState<CustomCase[]>([]);
  const [caseKeys, setCaseKeys] = useState<string[]>([]);
  const [caseMessage, setCaseMessage] = useState('');
  const [caseExpect, setCaseExpect] = useState('');
  const [draftText, setDraftText] = useState('');
  const [draft, setDraft] = useState<DraftShow>();
  const [draftCsv, setDraftCsv] = useState('');
  const [drafting, setDrafting] = useState(false);

  useEffect(() => { try { sessionStorage.setItem('ga-chat', JSON.stringify(chat.slice(-10))); } catch { /* storage unavailable */ } }, [chat]);

  const remember = useCallback((value: string) => {
    try { value ? sessionStorage.setItem(TOKEN_KEY, value) : sessionStorage.removeItem(TOKEN_KEY); } catch { /* The console still works without storage. */ }
  }, []);

  useEffect(() => {
    let live = true;
    guideAdminAPI.session().then(data => {
      if (!live) return;
      setSession(data);
      setState(data.enabled ? 'gate' : 'error');
      if (!data.enabled) setError('管理端未启用：请用 GUIDE_ADMIN_TOKEN 启动导购服务后再打开。');
    }).catch(failure => { if (live) { setState('error'); setError(failure.message || '管理接口暂时不可用'); } });
    return () => { live = false; };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const load = useCallback(async (activeToken: string, refresh = false) => {
    const [freshOverview, freshPrompt, freshCatalog, freshCases, runs, checkpoints, actions] = await Promise.all([
      guideAdminAPI.overview(activeToken, refresh),
      guideAdminAPI.prompt(activeToken),
      guideAdminAPI.catalog(activeToken, refresh),
      guideAdminAPI.cases(activeToken),
      agentAPI.runs(activeToken).catch(() => ({ items: [], mode: '' })),
      agentAPI.checkpoints(activeToken).catch(() => ({ items: [] })),
      agentAPI.actions(activeToken).catch(() => ({ items: [] }))
    ]);
    setAgentRuns(runs.items || []); setAgentMode(runs.mode || '');
    setAgentCheckpoints(checkpoints.items || []); setAgentActions(actions.items || []);
    setCases(freshCases.items || []); setCaseKeys(freshCases.keys || []);
    setOverview(freshOverview);
    setPromptDoc(freshPrompt);
    setCatalog(freshCatalog);
    setPromptDraft(current => (current && current !== promptDoc?.text ? current : freshPrompt.text));
    setState('ready');
    setError('');
    return freshOverview;
  }, [promptDoc?.text]);

  const authorize = useCallback(async (activeToken: string, refresh = false) => {
    setState('loading'); setError('');
    try {
      await load(activeToken, refresh);
      setSession(current => current ? { ...current, authenticated: true } : current);
      setRunning(true);
      guideAdminAPI.evaluate(activeToken).then(result => setReport(result)).catch(() => undefined).finally(() => setRunning(false));
    } catch (failure) {
      if (failure instanceof AdminError && failure.status === 401) {
        remember(''); setToken(''); setState('gate'); setError('管理口令无效，请重新输入。');
        return;
      }
      setState('error'); setError(failure instanceof Error ? failure.message : '暂时无法读取管理数据');
    }
  }, [load, remember]);

  useEffect(() => { if (token && state === 'gate') void authorize(token); }, [token, state, authorize]);

  function submitToken(event: FormEvent) {
    event.preventDefault();
    const value = tokenDraft.trim();
    if (!value) return;
    remember(value); setToken(value); setTokenDraft('');
  }

  async function runBench() {
    if (!token || agentBusy) return;
    setAgentBusy('bench'); setNotice('');
    try { const data = await agentAPI.bench(token); setBenchData(data); setNotice(`Agent 跑分完成：${data.report.passed}/${data.report.cases} 通过。`); }
    catch (failure) { setError(failure instanceof Error ? failure.message : '跑分失败'); }
    finally { setAgentBusy(''); }
  }
  async function runRagEval() {
    if (!token || agentBusy) return;
    setAgentBusy('rag'); setNotice('');
    try { setRagData(await agentAPI.ragEval(token)); setNotice('常识检索评估完成，见下方三策略对比。'); }
    catch (failure) { setError(failure instanceof Error ? failure.message : '评估失败'); }
    finally { setAgentBusy(''); }
  }

  async function refresh() {
    if (!token || refreshing) return;
    setRefreshing(true); setNotice('');
    try { await load(token, true); setNotice('已重新读取 Echo 的演出、票档与评测。'); }
    catch (failure) { setError(failure instanceof Error ? failure.message : '刷新失败'); }
    finally { setRefreshing(false); }
  }

  // 保存 / 恢复 / 回滚都要先弹出二次确认，再输入一次口令才真正生效。
  function requestPromptChange(mode: 'save' | 'restore' | 'rollback', version?: number) {
    if (!token || saving) return;
    setConfirmToken(''); setConfirmError('');
    setPromptConfirm({ mode, version });
  }

  async function submitPromptChange() {
    if (!token || !promptConfirm || saving) return;
    if (!confirmToken.trim()) { setConfirmError('请再次输入管理口令。'); return; }
    setSaving(true); setConfirmError(''); setError(''); setNotice('');
    const body = promptConfirm.mode === 'restore' ? { restore: true, confirm: confirmToken }
      : promptConfirm.mode === 'rollback' ? { version: promptConfirm.version, confirm: confirmToken }
      : { text: promptDraft, confirm: confirmToken };
    try {
      const result: PromptSaveResult = await guideAdminAPI.savePrompt(token, body);
      const fresh = await guideAdminAPI.prompt(token);
      setPromptDoc(fresh); setPromptDraft(fresh.text);
      setLastEval(result.eval);
      const verb = promptConfirm.mode === 'restore' ? '恢复默认' : promptConfirm.mode === 'rollback' ? `回滚到第 ${promptConfirm.version} 版` : '保存';
      const evalNote = result.eval ? (result.eval.passed === result.eval.total ? `，巡检 ${result.eval.passed}/${result.eval.total} 通过` : `，巡检仅 ${result.eval.passed}/${result.eval.total} 通过，请检查失败项`) : '';
      setNotice(`提示词已${verb}为第 ${result.version} 版${evalNote}，下一次咨询立即生效。`);
      setPromptConfirm(undefined); setConfirmToken('');
    } catch (failure) {
      setConfirmError(failure instanceof Error ? failure.message : '保存失败');
    } finally { setSaving(false); }
  }

  async function addCase() {
    if (!token || !caseMessage.trim()) return;
    const expect = caseExpect.split(/[,，\n]/).map(item => item.trim()).filter(Boolean);
    try {
      const added = await guideAdminAPI.addCase(token, { message: caseMessage.trim(), expect });
      setCases(items => [added, ...items]); setCaseMessage(''); setCaseExpect(''); setNotice('自定义用例已加入，下次巡检会执行。');
    } catch (failure) { setError(failure instanceof Error ? failure.message : '用例保存失败'); }
  }

  async function removeCase(id: string) {
    if (!token) return;
    try { await guideAdminAPI.deleteCase(token, id); setCases(items => items.filter(item => item.id !== id)); }
    catch (failure) { setError(failure instanceof Error ? failure.message : '删除失败'); }
  }

  async function generateDraft() {
    if (!token || drafting || draftText.trim().length < 10) return;
    setDrafting(true); setError('');
    try {
      const result = await guideAdminAPI.draft(token, draftText.trim());
      setDraft(result.draft); setDraftCsv(result.csv); setNotice('草稿已生成，核对无误后可下载 CSV 到 Echo 录入。');
    } catch (failure) { setError(failure instanceof Error ? failure.message : '草稿生成失败'); }
    finally { setDrafting(false); }
  }

  function downloadCSV(filename: string, csv: string) {
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url; link.download = filename; link.click();
    URL.revokeObjectURL(url);
  }

  function citiesCSV() {
    if (!catalog) return;
    const rows = [['城市','演出','未结束','历史','有效评分','加权均分','评测数','最低价','最高价','票档','需选座','缺票档','评测不全']];
    for (const row of catalog.cities) rows.push([row.city, String(row.shows), String(row.active), String(row.ended), String(row.rating_count), row.average.toFixed(2), String(row.reviews), row.min_price === null ? '' : row.min_price.toFixed(2), row.max_price === null ? '' : row.max_price.toFixed(2), String(row.tiers), String(row.selectable), String(row.missing_tickets), String(row.missing_reviews)]);
    downloadCSV('城市数据.csv', toCSV(rows));
  }

  function tiersCSV() {
    if (!catalog) return;
    const rows = [['演出','城市','状态','票档','副标题','价格','余票','需选座','秒杀','秒杀进行中','售罄','上架','缺退改规则']];
    for (const tier of catalog.tiers) rows.push([tier.show, tier.city, tier.lifecycle, tier.title, tier.sub_title, tier.price.toFixed(2), String(tier.stock), String(tier.has_seats), String(tier.is_seckill), String(tier.seckill_active), String(tier.sold_out), String(tier.on_sale), String(tier.rules_missing)]);
    downloadCSV('票种票档.csv', toCSV(rows));
  }

  function toCSV(rows: (string | number)[][]) {
    return '\uFEFF' + rows.map(row => row.map(cell => `"${String(cell).replace(/"/g, '""')}"`).join(',')).join('\r\n');
  }

  async function runEval() {
    if (!token || running) return;
    setRunning(true); setError('');
    try { setReport(await guideAdminAPI.evaluate(token)); }
    catch (failure) { setError(failure instanceof Error ? failure.message : '巡检失败'); }
    finally { setRunning(false); }
  }

  const filteredTiers = useMemo(() => {
    const query = tierQuery.trim().toLowerCase();
    const band = bandFilter === '' ? undefined : catalog?.price_bands[Number(bandFilter)];
    return (catalog?.tiers || []).filter(tier => {
      if (cityFilter && tier.city !== cityFilter) return false;
      if (band && !(tier.price >= band.min && tier.price < band.max)) return false;
      if (onlyAvailable && !(tier.on_sale && !tier.sold_out && tier.lifecycle !== 'ended')) return false;
      if (onlyAttention && !(tier.rules_missing || tier.stock_source === 'mysql_snapshot' || tier.sold_out)) return false;
      if (query && !`${tier.show} ${tier.title} ${tier.city}`.toLowerCase().includes(query)) return false;
      return true;
    });
  }, [catalog, cityFilter, bandFilter, tierQuery, onlyAvailable, onlyAttention]);

  useEffect(() => { setVisibleTiers(12); }, [cityFilter, bandFilter, tierQuery, onlyAvailable, onlyAttention]);

  async function sendAsk(text?: string) {
    const message = (text ?? chatInput).trim();
    if (!message || !token || asking) return;
    const plan = [...chat].reverse().find(turn => turn.result?.plan)?.result?.plan;
    setAsking(true); setChatInput('');
    setChat(items => [...items, { question: message }]);
    try {
      const result = await guideAdminAPI.ask(token, { message, plan, use_model: useModel });
      setChat(items => items.map((item, index) => index === items.length - 1 ? { ...item, result } : item));
    } catch (failure) {
      setChat(items => items.map((item, index) => index === items.length - 1 ? { ...item, error: failure instanceof Error ? failure.message : '请求失败，请稍后再试' } : item));
    } finally { setAsking(false); }
  }

  const warnCount = (overview?.health || []).filter(item => item.level !== 'ok').length;
  const coverage = overview?.coverage;
  return <div className="eg-page ga-page">
    <header className="eg-header"><div className="eg-header-inner">
      <button className="eg-logo-link ga-logo" onClick={onExit} aria-label="返回演出导购"><span className="eg-wordmark">echo<span>®</span></span><span className="eg-brand-divider" /><span>回响<br /><small>ASSISTANT CONSOLE</small></span></button>
      <nav aria-label="管理导航"><a href="#ga-overview">助手概览{warnCount > 0 && <span className="ga-warn-badge">{warnCount}</span>}</a><a href="#ga-chat">对话试验台</a><a href="#ga-eval">质量巡检</a><a href="#ga-cities">城市数据</a><a href="#ga-tickets">票种票档</a><a href="#ga-agent">Agent 观测</a><a href="#ga-draft">数据草稿</a><a href="#ga-prompt">提示词</a><a href="#ga-recent">最近请求</a></nav>
      <button className="eg-echo-link ga-refresh" onClick={refresh} disabled={state !== 'ready' || refreshing}>{refreshing ? '正在刷新…' : '刷新数据 ↻'}</button>
    </div></header>

    <main className="eg-main">
      <section className="eg-hero ga-hero">
        <div>
          <p className="eg-eyebrow"><span className="eg-dot" /> ECHO / ASSISTANT CONSOLE</p>
          <h1>让助手，<br />有据<span>可查。</span></h1>
          <p className="eg-hero-description">核对助手读到哪些数据、记住了什么需求、每次推荐依据了哪条评测。<br />改提示词、跑巡检、回看最近请求，都不用离开导购。</p>
        </div>
        <div className="ga-status">
          <div className="ga-status-row"><span>回答模型</span><strong>{modelLabel(session)}</strong></div>
          <div className="ga-status-row"><span>提示词版本</span><strong>v{session?.prompt.version ?? '—'}{session?.prompt.updated_at ? <small> · {timeLabel(session.prompt.updated_at)}</small> : null}</strong></div>
          <div className="ga-status-row"><span>数据更新时间</span><strong>{coverage?.updated_at ? timeLabel(coverage.updated_at) : '读取中'}</strong></div>
          <p className="ga-status-note">缺少评分或票档的演出会在下面单独列出来，不会拿猜测补位。</p>
        </div>
      </section>

      {state === 'loading' && <p className="eg-notice" role="status">正在读取 Echo 的演出、评测与助手状态…</p>}
      {state === 'error' && <div className="eg-empty ga-empty" role="alert"><span className="eg-empty-symbol">e.</span><h3>管理端暂时打不开</h3><p>{error}</p><button className="eg-secondary" onClick={onExit}>返回导购</button></div>}

      {state === 'gate' && <section className="ga-gate" aria-label="管理口令">
        <p className="eg-eyebrow">ADMIN ACCESS</p>
        <h2>先输入管理口令</h2>
        <p>口令来自 <code>guide.env.cmd</code> 里的 <code>GUIDE_ADMIN_TOKEN</code>，只保存在当前浏览器标签页，不会写入页面代码。</p>
        <form onSubmit={submitToken}><input type="password" aria-label="管理口令" autoComplete="current-password" value={tokenDraft} onChange={event => setTokenDraft(event.target.value)} placeholder="请输入管理口令" maxLength={200} /><button className="eg-primary" disabled={!tokenDraft.trim()}>进入管理端</button></form>
        {error && <p className="ga-error" role="alert">{error}</p>}
      </section>}

      {state === 'ready' && overview && <>
        <section id="ga-overview" className="ga-metrics" aria-label="助手数据概览">
          <div><strong>{coverage?.loaded ?? '—'}</strong><span>公开演出</span><small>{coverage?.complete ? '已完整读取' : `共 ${coverage?.public_total ?? '—'} 场，未读完`}</small></div>
          <div><strong>{coverage?.active ?? '—'}</strong><span>可购场次</span><small>{coverage?.ended ?? '—'} 场历史回顾</small></div>
          <div><strong>{coverage?.rated_count ?? '—'}</strong><span>有效评分</span><small>来自 {coverage?.review_count ?? '—'} 条评测</small></div>
          <div><strong>{overview.stats.total}</strong><span>重启后咨询</span><small>平均 {overview.stats.avg_latency_ms} ms</small></div>
        </section>

        <section id="ga-chat" className="ga-card">
          <div className="eg-section-label"><span>对话试验台</span><span>{useModel ? '会调用模型' : '仅票务数据'}{chat.length > 0 ? ` · 已问 ${chat.length} 条` : ''}</span></div>
          <p className="ga-note">按访客原话提问验证解析与回答，也可直接问数据缺口；多轮沿用上一条需求。</p>
          <div className="ga-chat-starters">{chatStarters.map(text => <button key={text} className="ga-chat-starter" disabled={asking || state !== 'ready'} onClick={() => void sendAsk(text)}>{text}</button>)}</div>
          <div className="ga-chat-log" aria-live="polite">
            {!chat.length && <p className="ga-empty-row">还没有提问。可以先点上面的示例，或直接写一个访客需求。</p>}
            {chat.map((turn, index) => <div className="ga-chat-turn" key={index}>
              <p className="ga-chat-q"><span>你</span>{turn.question}</p>
              {turn.error && <p className="ga-error">{turn.error}</p>}
              {turn.result && <div className="ga-chat-a">
                <p className="ga-chat-answer">{turn.result.answer}</p>
                <div className="ga-chat-meta">
                  <span className={`ga-chip ${turn.result.mode === 'ai' ? 'ga-chip-ai' : turn.result.kind === 'data' ? '' : 'ga-chip-ok'}`}>{turn.result.mode === 'ai' ? '模型回答' : turn.result.kind === 'data' ? '数据核对' : '票务数据回答'}</span>
                  <span className="ga-chip">意图 {turn.result.intent}</span>
                  <span className="ga-chip">{turn.result.latency_ms} ms</span>
                  <span className="ga-chip">{turn.result.rows.length} 行结果</span>
                </div>
                {turn.result.plan && <div className="ga-chat-plan">
                  <span className="ga-chip">已解析需求</span>
                  <span className="ga-chip">{turn.result.plan.party_size} 人</span>
                  {turn.result.plan.budget > 0 && <span className="ga-chip">{turn.result.plan.budget_mode === 'per_person' ? '每人' : '总预算'} ¥{turn.result.plan.budget}</span>}
                  {turn.result.plan.filters.city && <span className="ga-chip">{cityLabel(turn.result.plan.filters.city)}</span>}
                  {turn.result.plan.filters.date_from && <span className="ga-chip">{turn.result.plan.filters.date_from}{turn.result.plan.filters.date_to && turn.result.plan.filters.date_to !== turn.result.plan.filters.date_from ? ` 至 ${turn.result.plan.filters.date_to}` : ''}</span>}
                  {turn.result.plan.seat_preference === 'front' && <span className="ga-chip">偏好前排</span>}
                  {turn.result.plan.seat_preference === 'adjacent' && <span className="ga-chip">偏好连座</span>}
                  {turn.result.filters && turn.result.filters.budget ? <span className="ga-chip">单张 ¥{turn.result.filters.budget}</span> : null}
                </div>}
                {turn.result.rows.length > 0 && <div className="ga-table-wrap"><table className="ga-table ga-chat-table">
                  <thead><tr><th scope="col">结果</th><th scope="col">数据标记</th><th scope="col">数值</th></tr></thead>
                  <tbody>{turn.result.rows.map((row, rowIndex) => <tr key={rowIndex}>
                    <td className="ga-question"><strong>{row.title}</strong>{row.sub && <small>{row.sub}</small>}</td>
                    <td><div className="ga-badges">{row.tags.map(tag => <span className="ga-chip" key={tag}>{tag}</span>)}</div></td>
                    <td>{row.value}</td>
                  </tr>)}</tbody>
                </table></div>}
                {turn.result.flags.length > 0 && <ul className="ga-chat-flags">{turn.result.flags.map(flag => <li key={flag}>{flag}</li>)}</ul>}
              </div>}
            </div>)}
            {asking && <p className="ga-chat-loading" role="status">正在按真实数据解析…</p>}
          </div>
          <form className="ga-chat-form" onSubmit={event => { event.preventDefault(); void sendAsk(); }}>
            <textarea aria-label="向管理端提问" rows={2} maxLength={300} value={chatInput} placeholder="例如：周六两个人 600 元能看什么？或：哪些演出没有票档？" onChange={event => setChatInput(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void sendAsk(); } }} />
            <div className="ga-chat-controls">
              <label className="ga-check"><input type="checkbox" checked={useModel} onChange={event => setUseModel(event.target.checked)} />调用模型（关闭时只走票务数据）</label>
              {chat.length > 0 && <button type="button" className="eg-secondary" onClick={() => setChat([])} disabled={asking}>清空对话</button>}
              <button className="eg-primary" type="submit" disabled={!chatInput.trim() || asking}>{asking ? '解析中…' : '发送 ↑'}</button>
            </div>
          </form>
        </section>

        <div className="ga-columns">
          <section className="ga-card">
            <div className="eg-section-label"><span>数据健康</span><span>{overview.cities} 个城市 · {overview.categories} 个类型</span></div>
            <ul className="ga-health">{overview.health.map((item, index) => <li key={index} className={`ga-health-${item.level}`}>
              <span className="ga-chip">{levelLabel(item.level)}</span>
              <div><strong>{item.label}</strong><p>{item.detail}</p></div>
            </li>)}</ul>
            <p className="eg-fineprint ga-left">{overview.cache_note} 票档以 Echo 确认页为准。</p>
          </section>

          <section id="ga-eval" className="ga-card">
            <div className="eg-section-label"><span>助手巡检</span><span>{report ? `${report.passed} / ${report.total} 通过` : running ? '正在运行' : '尚未运行'}</span></div>
            <p className="ga-note">真实数据回放固定需求，关闭模型；失败会指明哪条判断不成立。</p>
            <div className="ga-actions"><button className="eg-primary" onClick={runEval} disabled={running || state !== 'ready'}>{running ? '正在巡检…' : '运行巡检'}</button>{report && <small>上次运行 {timeLabel(report.ran_at)} · {report.duration_ms} ms</small>}</div>
            <div className="ga-cases">
              <div className="ga-cases-head"><strong>自定义用例 {cases.length ? `· ${cases.length} 条` : ''}</strong><small>期望写法：party_size=2, budget=300, min_items=1</small></div>
              {!!cases.length && <ul>{cases.map(item => <li key={item.id}><span className="ga-chip">{item.expect.join(' · ')}</span><span className="ga-case-q">{item.message}</span><button className="eg-secondary" onClick={() => void removeCase(item.id)}>删除</button></li>)}</ul>}
              <div className="ga-cases-form"><input aria-label="自定义用例问题" placeholder="新用例问题，如：五个人看话剧，人均 150" value={caseMessage} maxLength={300} onChange={event => setCaseMessage(event.target.value)} /><input aria-label="期望条件" placeholder="期望：party_size=5, category=话剧" value={caseExpect} maxLength={200} onChange={event => setCaseExpect(event.target.value)} /><button className="eg-secondary" onClick={() => void addCase()} disabled={!caseMessage.trim() || !caseExpect.trim()}>加入用例</button></div>
              <small>可用键：{caseKeys.join('、') || '见接口文档'}</small>
            </div>
            {report && <div className="ga-eval-list">{report.cases.map(item => <details key={item.name} className={item.pass ? 'ga-eval-ok' : 'ga-eval-bad'} open={!item.pass}>
              <summary><span className={`ga-chip ${item.pass ? 'ga-chip-ok' : 'ga-chip-bad'}`}>{item.pass ? '通过' : '未通过'}</span><strong>{item.name}</strong><small>{item.question}</small></summary>
              <ul>{item.checks.map((check, index) => <li key={index}><span className={check.ok ? 'ga-check-ok' : 'ga-check-bad'}>{check.ok ? '✓' : '✗'}</span>{check.label}<em>{check.got}</em></li>)}</ul>
            </details>)}</div>}
            {report && <p className="eg-fineprint ga-left">{report.note}</p>}
          </section>
        </div>

        <section id="ga-cities" className="ga-card">
          <div className="eg-section-label"><span>城市数据</span><span>{catalog ? `${catalog.cities.length} 个城市 · ${catalog.categories.length} 个类型` : '正在汇总'}{catalog && <button className="eg-secondary" onClick={citiesCSV}>导出 CSV</button>}</span></div>
          <p className="ga-note">按城市看演出供给、口碑样本与票价区间；缺数据的城市单独标出。</p>
          {!catalog && <p className="ga-empty-row">正在汇总城市与票档…</p>}
          {catalog && <div className="ga-table-wrap"><table className="ga-table ga-city-table">
            <thead><tr><th scope="col">城市</th><th scope="col">演出</th><th scope="col">可购</th><th scope="col">有效评分</th><th scope="col">评测</th><th scope="col">票价区间</th><th scope="col">票档</th><th scope="col">待补</th></tr></thead>
            <tbody>{catalog.cities.map(row => <tr key={row.city}>
              <td><strong>{cityLabel(row.city)}</strong></td>
              <td>{row.shows}{row.ended > 0 && <small>（含 {row.ended} 场历史）</small>}</td>
              <td>{row.active}</td>
              <td>{row.rating_count > 0 ? <>{row.rating_count} 个 · {row.average.toFixed(2)}</> : <span className="ga-muted">暂无评分</span>}</td>
              <td>{row.reviews}</td>
              <td>{priceRange(row.min_price, row.max_price)}</td>
              <td>{row.tiers}{row.selectable > 0 && <small>（{row.selectable} 需选座）</small>}</td>
              <td><div className="ga-badges">{row.missing_tickets > 0 && <span className="ga-chip ga-chip-bad">缺票档 {row.missing_tickets}</span>}{row.missing_reviews > 0 && <span className="ga-chip ga-chip-bad">评测不全 {row.missing_reviews}</span>}{row.missing_tickets === 0 && row.missing_reviews === 0 && <span className="ga-muted">—</span>}</div></td>
            </tr>)}</tbody>
          </table></div>}
          {catalog && <div className="ga-category-strip"><span className="ga-strip-label">类型分布</span>{catalog.categories.map(item => <span className="ga-chip" key={item.name}>{item.name} · {item.shows}</span>)}</div>}
        </section>

        <section id="ga-tickets" className="ga-card">
          <div className="eg-section-label"><span>票种与票档</span><span>{catalog ? `共 ${catalog.ticket_summary.tiers} 档 · 可售 ${catalog.ticket_summary.purchasable} 档` : '正在汇总'}{catalog && <button className="eg-secondary" onClick={tiersCSV}>导出 CSV</button>}</span></div>
          <p className="ga-note">按票档核对价格带、余票与库存来源；助手推荐只用「可售」票档。</p>
          {!catalog && <p className="ga-empty-row">正在读取各场票档…</p>}
          {catalog && <>
            <div className="ga-chips">
              <span className="ga-chip">票档 {catalog.ticket_summary.tiers}</span>
              <span className="ga-chip ga-chip-ok">可售 {catalog.ticket_summary.purchasable}</span>
              <span className="ga-chip ga-chip-bad">售罄 {catalog.ticket_summary.sold_out}</span>
              <span className="ga-chip">需选座 {catalog.ticket_summary.selectable}</span>
              <span className="ga-chip">免选座 {catalog.ticket_summary.no_seat}</span>
              <span className="ga-chip">秒杀 {catalog.ticket_summary.seckill}</span>
              {catalog.ticket_summary.snapshot_stock > 0 && <span className="ga-chip ga-chip-bad">快照库存 {catalog.ticket_summary.snapshot_stock}</span>}
              {catalog.ticket_summary.missing_rules > 0 && <span className="ga-chip ga-chip-bad">缺退改规则 {catalog.ticket_summary.missing_rules}</span>}
              {catalog.ticket_summary.suspicious_price > 0 && <span className="ga-chip ga-chip-bad">异常低价 {catalog.ticket_summary.suspicious_price}</span>}
              <span className="ga-chip">余票 {catalog.ticket_summary.stock} 张</span>
              <span className="ga-chip">均价 ¥{catalog.ticket_summary.average_price}</span>
            </div>
            <div className="ga-bands" aria-label="价格带分布">{catalog.price_bands.map((band, index) => <div className="ga-band" key={band.label}><span className="ga-band-label">{band.label}</span><div className="ga-bar"><i style={{ width: `${band.share}%` }} /></div><span className="ga-band-value">{band.count} 档 · {band.share}%</span></div>)}</div>
            <div className="ga-filters">
              <input aria-label="搜索演出或票档" placeholder="搜索演出或票档" value={tierQuery} maxLength={60} onChange={event => setTierQuery(event.target.value)} />
              <select aria-label="按城市筛选" value={cityFilter} onChange={event => setCityFilter(event.target.value)}><option value="">全部城市</option>{catalog.cities.map(row => <option key={row.city} value={row.city}>{cityLabel(row.city)}</option>)}</select>
              <select aria-label="按价格带筛选" value={bandFilter} onChange={event => setBandFilter(event.target.value)}><option value="">全部价位</option>{catalog.price_bands.map((band, index) => <option key={band.label} value={index}>{band.label}（{band.count}）</option>)}</select>
              <label className="ga-check"><input type="checkbox" checked={onlyAvailable} onChange={event => setOnlyAvailable(event.target.checked)} />只看可售</label>
              <label className="ga-check"><input type="checkbox" checked={onlyAttention} onChange={event => setOnlyAttention(event.target.checked)} />只看需留意</label>
              <span className="ga-filter-count">{filteredTiers.length} / {catalog.tiers.length} 档</span>
            </div>
            {!filteredTiers.length && <p className="ga-empty-row">这个条件下没有票档，换个城市或取消筛选试试。</p>}
            {!!filteredTiers.length && <div className="ga-table-wrap"><table className="ga-table">
              <thead><tr><th scope="col">演出</th><th scope="col">票档</th><th scope="col">价格</th><th scope="col">余票</th><th scope="col">状态</th></tr></thead>
              <tbody>{filteredTiers.slice(0, visibleTiers).map((tier, index) => <tr key={`${tier.show_id}-${tier.title}-${index}`}>
                <td className="ga-question"><strong>{tier.show}</strong><small>{cityLabel(tier.city)}</small></td>
                <td>{tier.title}{tier.sub_title && <small>{tier.sub_title}</small>}</td>
                <td>¥{tier.price}</td>
                <td>{tier.stock}</td>
                <td><div className="ga-badges">{tierBadges(tier).map(badge => <span className={`ga-chip ${badge.tone}`} key={badge.label}>{badge.label}</span>)}</div></td>
              </tr>)}</tbody>
            </table></div>}
            {filteredTiers.length > visibleTiers && <button className="eg-secondary ga-more" onClick={() => setVisibleTiers(value => value + 20)}>再看 20 档 · 还剩 {filteredTiers.length - visibleTiers}</button>}
            {!!catalog.notes.length && <ul className="ga-notes">{catalog.notes.map(note => <li key={note}>{note}</li>)}</ul>}
          </>}
        </section>

        <section id="ga-draft" className="ga-card">
          <div className="eg-section-label"><span>数据草稿</span><span>{draft ? `${draft.tiers.length} 个票档` : '等待生成'}</span></div>
          <p className="ga-note">粘贴一段演出描述，模型整理成演出 + 票档草稿并可导出 CSV；不写入 Echo，录入前请核对。</p>
          <textarea aria-label="演出描述" rows={4} maxLength={2000} value={draftText} placeholder="例如：12 月 20 日晚在上海大剧院话剧《回响》，票价 180 / 280 / 480，前两档各 50 张，480 需选座…" onChange={event => setDraftText(event.target.value)} />
          <div className="ga-actions"><button className="eg-primary" onClick={() => void generateDraft()} disabled={drafting || draftText.trim().length < 10}>{drafting ? '生成中…' : '生成草稿'}</button>{draft && <button className="eg-secondary" onClick={() => downloadCSV('演出票种草稿.csv', draftCsv)}>下载 CSV</button>}</div>
          {draft && <div className="ga-draft-result">
            <strong>{draft.name}</strong><span className="ga-chip">{draft.type_name || '类型待定'}</span>{draft.city && <span className="ga-chip">{draft.city}</span>}{draft.open_time && <span className="ga-chip">{draft.open_time}</span>}
            {draft.intro && <p>{draft.intro}</p>}
            <table className="ga-table"><thead><tr><th scope="col">票档</th><th scope="col">价格</th><th scope="col">余票</th><th scope="col">标记</th></tr></thead><tbody>{draft.tiers.map((tier, index) => <tr key={index}><td>{tier.title}{tier.rules && <small>{tier.rules}</small>}</td><td>¥{tier.price}</td><td>{tier.stock}</td><td><div className="ga-badges">{tier.has_seats && <span className="ga-chip">需选座</span>}{tier.is_seckill && <span className="ga-chip ga-chip-ai">秒杀</span>}</div></td></tr>)}</tbody></table>
          </div>}
        </section>

        <section id="ga-prompt" className="ga-card">
          <div className="eg-section-label"><span>助手提示词</span><span>v{promptDoc?.version ?? '—'} · {promptDoc?.updated_at ? timeLabel(promptDoc.updated_at) : '出厂版本'}</span></div>
          <p className="ga-note">提示词决定助手的口径与边界。保存、恢复默认与回滚都需要再次输入管理口令确认，每次改动会记入下方的修改记录并立即跑一遍质量巡检；改动建议保留“不虚构票价、余票、座位”等约束。</p>
          <textarea aria-label="助手提示词" value={promptDraft} maxLength={promptDoc?.max_runes ?? 8000} onChange={event => setPromptDraft(event.target.value)} rows={9} spellCheck={false} />
          <div className="ga-prompt-foot"><small>{promptDraft.length} / {promptDoc?.max_runes ?? 8000} 字</small><div className="ga-actions"><button className="eg-secondary" onClick={() => requestPromptChange('restore')} disabled={saving}>恢复默认</button><button className="eg-primary" onClick={() => requestPromptChange('save')} disabled={saving || !promptDoc || promptDraft.trim() === promptDoc.text.trim()}>{saving ? '保存中…' : '保存提示词'}</button></div></div>
          {promptConfirm && <form className="ga-confirm" onSubmit={event => { event.preventDefault(); void submitPromptChange(); }}>
            <strong>{promptConfirm.mode === 'restore' ? '确认恢复默认提示词？' : promptConfirm.mode === 'rollback' ? `确认回滚到第 ${promptConfirm.version} 版？` : '确认保存新提示词？'}</strong>
            <p>这次修改会立即影响助手对访客的回答，并新增一个版本记录。请再次输入管理口令以确认。</p>
            <input type="password" aria-label="再次输入管理口令" autoComplete="current-password" value={confirmToken} onChange={event => setConfirmToken(event.target.value)} placeholder="再次输入管理口令" maxLength={200} autoFocus />
            {confirmError && <p className="ga-error">{confirmError}</p>}
            <div className="ga-actions"><button type="button" className="eg-secondary" onClick={() => { setPromptConfirm(undefined); setConfirmToken(''); }}>取消</button><button className="eg-primary" type="submit" disabled={saving || !confirmToken.trim()}>{saving ? '正在保存并巡检…' : '确认修改'}</button></div>
          </form>}
          {lastEval && <div className="ga-last-eval">
            <span className={`ga-chip ${lastEval.passed === lastEval.total ? 'ga-chip-ok' : 'ga-chip-bad'}`}>保存后巡检 {lastEval.passed}/{lastEval.total} 通过</span>
            {lastEval.passed !== lastEval.total && <span className="ga-failed-names">{lastEval.cases.filter(c => !c.pass).map(c => c.name).join('、')} 未通过</span>}
          </div>}
          {!!promptDoc?.history?.length && <div className="ga-history">
            <div className="eg-section-label"><span>修改记录</span><span>只保留最近 {promptDoc.history.length} 条</span></div>
            <ul>{promptDoc.history.map(entry => <li key={entry.version}>
              <span className="ga-chip">{entry.action === 'restore' ? '恢复默认' : entry.action === 'rollback' ? '回滚' : '编辑'}</span>
              <strong>第 {entry.version} 版</strong>
              <small>{timeLabel(entry.at)}</small>
              <p>{entry.text.slice(0, 60)}{entry.text.length > 60 ? '…' : ''}</p>
              {entry.version !== promptDoc.version && <button className="eg-secondary" onClick={() => requestPromptChange('rollback', entry.version)} disabled={saving}>回滚到此版</button>}
            </li>)}</ul>
          </div>}
        </section>

        <section id="ga-agent" className="ga-card">
          <div className="eg-section-label"><span>Agent 观测</span><span>{agentRuns.length > 0 ? `${agentRuns.length} 次运行 · ${agentMode}` : '还没有运行记录'}</span></div>

          <h3>运行轨迹</h3>
          <p className="ga-hint">每次咨询的意图、工具链与逐步耗时；点行展开完整推理步骤，用于回答「它为什么这么答」。</p>
          {agentRuns.length === 0 && <p className="ga-hint">先在导购页与选票助手对话一次，再回来刷新。</p>}
          {!!agentRuns.length && <div className="ga-table-wrap"><table className="ga-table">
            <thead><tr><th scope="col">时间</th><th scope="col">问题</th><th scope="col">意图</th><th scope="col">模式</th><th scope="col">工具链</th><th scope="col">步数</th><th scope="col">耗时</th></tr></thead>
            <tbody>{agentRuns.map(run => [<tr key={run.run_id} className="ga-agent-row" onClick={() => setAgentOpenRun(current => current === run.run_id ? '' : run.run_id)}>
              <td>{run.started_at.slice(11, 19)}</td>
              <td className="ga-agent-q">{run.query.length > 26 ? `${run.query.slice(0, 26)}…` : run.query}{run.resumable && <span className="ga-chip ga-chip-ai">可续跑</span>}{run.resumed_from && <span className="ga-chip">续跑自 {run.resumed_from.slice(0, 6)}</span>}</td>
              <td>{run.intent}</td>
              <td>{run.mode === 'agent-react' ? '模型编排' : '规则编排'}</td>
              <td className="ga-agent-tools">{run.tools.join(' → ') || '—'}</td>
              <td>{run.steps.length}</td>
              <td>{run.elapsed_ms} ms</td>
            </tr>,
            agentOpenRun === run.run_id && <tr key={`${run.run_id}-detail`} className="ga-agent-detail"><td colSpan={7}>
              <ol className="ga-agent-steps">{run.steps.map(step => <li key={step.index} className={step.ok ? '' : 'ga-agent-step-fail'}>
                <span className="ga-chip">{step.phase}</span>
                <strong>{step.title}</strong>
                {step.detail && <small>{step.detail}</small>}
                {typeof step.ms === 'number' && <small> {step.ms} ms</small>}
              </li>)}</ol>
              {run.answer && <p className="ga-agent-answer">{run.answer}</p>}
              {run.note && <p className="ga-hint">备注：{run.note}</p>}
            </td></tr>])}
            </tbody></table></div>}

          <h3>断点快照（可恢复）</h3>
          <p className="ga-hint">推理被超时或断连打断时留下的中途快照；用户可按 run_id 接着跑，已查到的事实不重查。</p>
          {agentCheckpoints.length === 0 && <p className="ga-hint">当前没有待恢复的运行（这是常态：跑完即清）。</p>}
          {!!agentCheckpoints.length && <div className="ga-table-wrap"><table className="ga-table">
            <thead><tr><th scope="col">run_id</th><th scope="col">问题</th><th scope="col">停在哪</th><th scope="col">原因</th><th scope="col">轮次</th><th scope="col">更新时间</th></tr></thead>
            <tbody>{agentCheckpoints.map(item => <tr key={item.run_id}>
              <td>{item.run_id.slice(0, 10)}…</td><td>{item.query.length > 24 ? `${item.query.slice(0, 24)}…` : item.query}</td>
              <td>{item.stage}</td><td>{item.reason || '—'}</td><td>{item.round}</td>
              <td>{item.updated_at?.slice(11, 19)}</td>
            </tr>)}</tbody></table></div>}

          <h3>写操作审计</h3>
          <p className="ga-hint">模型只能提案，用户确认后才落库；这里能看到所有设备的决策结果，重复确认返回 409。</p>
          {agentActions.length === 0 && <p className="ga-hint">还没有写操作提案。</p>}
          {!!agentActions.length && <div className="ga-table-wrap"><table className="ga-table">
            <thead><tr><th scope="col">时间</th><th scope="col">类型</th><th scope="col">内容</th><th scope="col">状态</th><th scope="col">结果</th></tr></thead>
            <tbody>{agentActions.map(item => <tr key={item.id}>
              <td>{(item.decided_at || item.created_at || '').slice(11, 19)}</td>
              <td>{item.kind === 'watchlist' ? '观演清单' : '长期偏好'}</td>
              <td>{item.summary}</td>
              <td><span className={`ga-chip${item.status === 'confirmed' ? ' ga-chip-ai' : ''}`}>{item.status === 'pending' ? '待确认' : item.status === 'confirmed' ? '已执行' : item.status === 'rejected' ? '已取消' : item.status}</span></td>
              <td>{item.result || '—'}</td>
            </tr>)}</tbody></table></div>}

          <h3>跑分与检索评估</h3>
          <p className="ga-hint">改提示词、加工具或换模型之后各跑一次，退化会直接体现在数字上。</p>
          <div className="ga-agent-buttons">
            <button className="eg-secondary" disabled={agentBusy === 'bench'} onClick={runBench}>{agentBusy === 'bench' ? '回放中…' : '跑 Agent 跑分（8 条用例）'}</button>
            <button className="eg-secondary" disabled={agentBusy === 'rag'} onClick={runRagEval}>{agentBusy === 'rag' ? '评估中…' : '跑 RAG 召回对比（15 条标注）'}</button>
          </div>
          {benchData && <div className="ga-metrics">
            <div><strong>{benchData.report.passed}/{benchData.report.cases}</strong><span>用例通过</span><small>{benchData.mode === 'agent-react' ? '模型编排' : '规则编排'}</small></div>
            <div><strong>{Math.round(benchData.report.tool_hit_rate * 100)}%</strong><span>工具命中率</span><small>期望工具被实际调用</small></div>
            <div><strong>{Math.round(benchData.report.intent_rate * 100)}%</strong><span>意图准确率</span><small>与规则底线一致</small></div>
            <div><strong>{benchData.report.avg_steps.toFixed(1)}</strong><span>平均步数</span><small>平均 {benchData.report.avg_elapsed_ms.toFixed(0)} ms</small></div>
          </div>}
          {!!benchData?.report.failures?.length && <ul className="ga-agent-failures">{benchData.report.failures.map(item => <li key={item}>{item}</li>)}</ul>}
          {ragData && <div className="ga-table-wrap"><table className="ga-table">
            <thead><tr><th scope="col">策略</th><th scope="col">recall@{ragData.report.k}</th><th scope="col">MRR@{ragData.report.k}</th><th scope="col">平均耗时</th><th scope="col">未命中</th></tr></thead>
            <tbody>{ragData.report.results.map(item => <tr key={item.strategy}>
              <td>{item.strategy === 'keyword' ? '词面基线' : item.strategy === 'vector' ? '纯向量召回' : '向量召回 + 重排'}</td>
              <td>{(item.recall_at_k * 100).toFixed(0)}%</td><td>{item.mrr_at_k.toFixed(2)}</td>
              <td>{item.avg_ms.toFixed(2)} ms</td><td>{item.misses}</td>
            </tr>)}</tbody></table></div>}
          {ragData && <p className="ga-hint">向量器：{ragData.report.embedder} · 常识条目 {ragData.entries} 条</p>}
          {!!ragData?.report.examples?.length && <ul className="ga-agent-failures">{ragData.report.examples.map(item => <li key={item.query}>「{item.query}」 词面基线{item.baseline ? `首条命中「${item.baseline}」` : '零召回'}，混合策略命中「{item.hybrid}」</li>)}</ul>}
        </section>

        <section id="ga-recent" className="ga-card">
          <div className="eg-section-label"><span>最近请求</span><span>服务启动于 {timeLabel(overview.stats.since)} · 只保留最近 20 条</span></div>
          <p className="ga-note">记录问题、模式与耗时；不保存用户身份信息。</p>
          {!overview.stats.recent.length && <p className="ga-empty-row">服务重启后还没有人咨询。可以去导购页问一句“周六，两个人，总预算 600 元”，再回来核对。</p>}
          {!!overview.stats.recent.length && <div className="ga-table-wrap"><table className="ga-table"><thead><tr><th scope="col">时间</th><th scope="col">问题</th><th scope="col">模式</th><th scope="col">结果</th><th scope="col">耗时</th></tr></thead><tbody>
            {overview.stats.recent.map((item, index) => <tr key={index}><td>{timeLabel(item.at)}</td><td className="ga-question"><span className={item.question.length > 120 ? 'ga-q-clamp' : ''}>{item.question}</span>{item.question.length > 120 && <button className="eg-secondary ga-q-toggle" onClick={event => { const node = event.currentTarget.previousElementSibling as HTMLElement; const open = node.classList.toggle('ga-q-clamp'); event.currentTarget.textContent = open ? '展开' : '收起'; }}>展开</button>}</td><td><span className={`ga-chip ${item.mode === 'ai' ? 'ga-chip-ai' : ''}`}>{item.mode === 'ai' ? '模型' : '票务数据'}</span></td><td>{item.items} 场推荐{item.alternatives ? ` · ${item.alternatives} 条备选` : ''}</td><td>{item.latency_ms} ms</td></tr>)}
          </tbody></table></div>}
          <p className="eg-fineprint ga-left">模型失败会自动回退到票务数据回答，回退次数：{overview.stats.fallback}。</p>
        </section>
      </>}

      {notice && <p className="ga-notice" role="status">{notice}</p>}
      {state === 'ready' && error && <p className="ga-error" role="alert">{error}</p>}
    </main>

    <footer className="eg-footer ga-footer"><span>echo 回响 <span>·</span> 助手管理</span><div><button onClick={onExit}>返回演出导购</button></div><small>ASSISTANT CONSOLE</small></footer>
  </div>;
}
