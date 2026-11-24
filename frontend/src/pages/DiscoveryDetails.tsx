import { useEffect, useMemo, useRef, useState } from 'react';
import { checkoutLink, money, safeEchoURL, ticketingAPI } from '../api/ticketing';
import type { RatingSummary, Recommendation, Review, ReviewReport, Show, ShowDetail, ShoppingPlan } from '../api/ticketing';

export function dateLabel(value: string) {
  if (!value) return '时间待公布';
  const normalized = value.replace(' ', 'T');
  const date = new Date(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?$/.test(normalized) ? `${normalized}+08:00` : normalized);
  if (!Number.isFinite(date.getTime())) return value;
  return new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'Asia/Shanghai' }).format(date);
}

export function lifecycleLabel(show: Show) {
  return show.lifecycle === 'ended' ? '历史演出 · 已结束' : show.lifecycle === 'upcoming' ? '即将开演' : show.lifecycle === 'live' ? '演出进行中' : '时间待确认';
}

export function ratingLabel(rating: RatingSummary) {
  return rating.count > 0 ? `${rating.average.toFixed(2)} / 5` : rating.available ? '暂无评分' : '评分未读取';
}

function useModal(onClose: () => void) {
  const ref = useRef<HTMLElement>(null);
  const close = useRef(onClose); close.current = onClose;
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    ref.current?.querySelector<HTMLElement>('button')?.focus();
    const listener = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); close.current(); }
      if (event.key !== 'Tab') return;
      const elements = [...(ref.current?.querySelectorAll<HTMLElement>('button:not(:disabled),a[href],select,input,summary') ?? [])].filter(element => element.getClientRects().length > 0);
      const first = elements[0], last = elements[elements.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
      if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
    };
    document.addEventListener('keydown', listener);
    return () => { document.body.style.overflow = overflow; document.removeEventListener('keydown', listener); previous?.focus(); };
  }, []);
  return ref;
}

const dimensionLabels:Record<string,string>={story:'剧情',performance:'表演',sound:'音效',view:'视野',value:'性价比'};

export function ReviewCard({ item }: { item: Review }) {
  return <article className="eg-review-card">
    <div className="eg-review-byline"><span>{item.author}</span><strong>{item.score >= 1 && item.score <= 5 ? `${item.score} 星` : '未提供评分'}</strong><time>{dateLabel(item.created_at)}</time></div>
    <div className="eg-evidence-badges">{item.purchased&&<span>已购票</span>}{item.verified&&<span>已核验</span>}{item.seat_area&&<span>{item.seat_area}</span>}{item.evidence_known&&!item.purchased&&!item.verified&&<small>未关联有效购票凭据</small>}</div>
    {!!Object.keys(item.dimensions||{}).length&&<p className="eg-review-dimensions">{Object.entries(item.dimensions||{}).map(([key,value])=><span key={key}>{dimensionLabels[key]||key} {value}/5</span>)}</p>}
    {item.title && <h4>{item.title}</h4>}
    <p>{item.content || '这位观众仅提交了评分。'}</p>
    <div className="eg-review-meta"><span>{item.likes} 次赞同 · {item.comments} 条讨论</span><a href={safeEchoURL(item.detail_url, `http://127.0.0.1:3002/review/${item.review_id}`)} target="_blank" rel="noopener noreferrer">阅读原文 ↗</a></div>
  </article>;
}

function ReviewPanel({ report, echoScore }: { report: ReviewReport; echoScore: number }) {
  const [filter, setFilter] = useState('all');
  const [seatArea,setSeatArea]=useState('');
  const [sort, setSort] = useState('latest');
  const [visibleCount, setVisibleCount] = useState(5);
  const summary = report.summary;
  const items = useMemo(() => {
    const items = report.items.filter(item=>!seatArea||item.seat_area===seatArea).filter(item => filter === 'all' || filter === 'text' && !!(item.content || item.title) || filter === 'positive' && item.score >= 4 && item.score <= 5 || filter === 'critical' && item.score >= 1 && item.score <= 3);
    return items.sort((a,b) => sort === 'helpful' ? b.likes - a.likes : sort === 'low' ? (a.score || 6) - (b.score || 6) : b.created_at.localeCompare(a.created_at));
  }, [report, filter, sort,seatArea]);
  useEffect(() => setVisibleCount(5), [filter, sort,seatArea]);
  return <section className="eg-review-panel">
    <div className="eg-rating-overview"><div><span>观众评测均分</span><strong>{summary.count > 0 ? summary.average.toFixed(2) : '—'}<small>/ 5</small></strong><p>{summary.count} 个有效评分 · {summary.total} 条评测</p><p>Echo 演出展示分：{echoScore > 0 ? `${echoScore.toFixed(1)} / 5` : '暂无'}</p></div>
      <div className="eg-rating-distribution" aria-label="评分分布">{[5,4,3,2,1].map(star => <div key={star}><span>{star} 星</span><div><i style={{ width: `${summary.count ? summary.distribution[star-1] / summary.count * 100 : 0}%` }} /></div><span>{summary.distribution[star-1] || 0}</span></div>)}</div>
    </div>
    <div className="eg-rating-footnotes"><span>4–5 星占比：{summary.count ? `${(summary.positive_rate * 100).toFixed(0)}%` : '暂无'}</span><span>{summary.text_count} 条有文字内容</span>{summary.low_sample && <span>评分样本少于 5 个</span>}{!summary.complete && <span>当前统计尚不完整</span>}</div>
    {!summary.available && <p role="status" className="eg-notice">观众评测暂未读取，不能据此判断口碑。可刷新详情重试。</p>}
    {report.warnings.map((warning,i) => <p className="eg-inline-warning" key={i}>{warning}</p>)}
    {(report.positive.length > 0 || report.critical.length > 0) && <div className="eg-review-evidence"><div><h4>高分观众怎么说</h4>{report.positive.slice(0,1).map(item => <blockquote key={item.review_id}>“{item.content}”<cite>{item.author} · {item.score} 星 · 原文 #{item.review_id}</cite></blockquote>)}{!report.positive.length && <p>当前没有可引用的高分文字评测。</p>}</div><div><h4>也听听不同意见</h4>{report.critical.slice(0,1).map(item => <blockquote key={item.review_id}>“{item.content}”<cite>{item.author} · {item.score} 星 · 原文 #{item.review_id}</cite></blockquote>)}{!report.critical.length && <p>当前没有可引用的 3 星及以下文字评测，不代表没有不同意见。</p>}</div></div>}
    <section className="eg-dimension-summary" aria-label="分项评分">{Object.entries(dimensionLabels).map(([key,label])=>{const value=report.insights?.dimensions?.[key];return <div key={key}><span>{label}</span><strong>{value?value.average.toFixed(1):'—'}<small>/5</small></strong><small>{value?`${value.count} 人评价`:'尚未填写'}</small></div>;})}</section><p className="eg-fineprint">分项评分由观众自行填写，各项样本单独统计；已购票和已核验分别依据 Echo 订单记录。</p>
    <div className="eg-review-toolbar"><label>座位区域<select value={seatArea} onChange={event=>setSeatArea(event.target.value)}><option value="">全部区域</option>{report.insights?.seat_areas?.map(area=><option key={area}>{area}</option>)}</select></label><label>查看<select value={filter} onChange={event => setFilter(event.target.value)}><option value="all">全部评测（含纯评分）</option><option value="text">只看文字评测</option><option value="positive">4–5 星</option><option value="critical">1–3 星</option></select></label><label>排序<select value={sort} onChange={event => setSort(event.target.value)}><option value="latest">最新发表</option><option value="helpful">赞同最多</option><option value="low">低分优先</option></select></label></div>
    <p className="eg-review-count">当前筛选 {items.length} 条</p>
    {!items.length && <p className="eg-notice">这个条件下暂无评测。</p>}
    {items.slice(0, visibleCount).map(item => <ReviewCard key={item.review_id} item={item} />)}
    {items.length > visibleCount && <button className="eg-secondary eg-load-more" onClick={() => setVisibleCount(value => value + 10)}>再看 10 条评测</button>}
  </section>;
}

// 秒杀倒计时：开抢前显示距开抢时间，进行中显示距结束，其余只给时间段。
function SeckillSchedule({ begin, end, active, ended }: { begin: string; end: string; active: boolean; ended: boolean }) {
  const [, setTick] = useState(0);
  const live = !ended && !!begin && !!end;
  useEffect(() => {
    if (!live) return;
    const timer = setInterval(() => setTick(value => value + 1), 1000);
    return () => clearInterval(timer);
  }, [live]);
  const parse = (value: string) => { const date = new Date(value.replace(' ', 'T')); return isNaN(date.getTime()) ? null : date; };
  const startDate = parse(begin); const endDate = parse(end);
  const format = (ms: number) => { const total = Math.max(0, Math.floor(ms / 1000)); const days = Math.floor(total / 86400); const hh = String(Math.floor(total % 86400 / 3600)).padStart(2, '0'); const mm = String(Math.floor(total % 3600 / 60)).padStart(2, '0'); const ss = String(total % 60).padStart(2, '0'); return days > 0 ? `${days}天 ${hh}:${mm}:${ss}` : `${hh}:${mm}:${ss}`; };
  const now = Date.now();
  let status = `活动：${begin || '待公布'} — ${end || '待公布'}`;
  if (live && startDate && endDate) {
    if (now < startDate.getTime()) status = `距开抢 ${format(startDate.getTime() - now)}`;
    else if (now < endDate.getTime()) status = active ? `抢票中 · 距结束 ${format(endDate.getTime() - now)}` : `活动：${begin} — ${end}`;
    else status = '活动已结束';
  }
  return <p className={`eg-tier-schedule${live && startDate && now >= startDate.getTime() && now < endDate!.getTime() && active ? ' eg-tier-live' : ''}`}>{status}</p>;
}

export function ShowDetails({ show, budget, plan, onClose, onDiscuss }: { show: Show; budget: number; plan?: ShoppingPlan; onClose: () => void; onDiscuss: (show: Show) => void }) {
  const [detail, setDetail] = useState<ShowDetail>();
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  const [retry, setRetry] = useState(0);
  const [tab, setTab] = useState('reviews');
  const ref = useModal(onClose);
  useEffect(() => {
    const controller = new AbortController(); setState('loading');
    ticketingAPI.detail(show.show_id, controller.signal, retry > 0).then(data => { setDetail(data); setState('ready'); }).catch(error => {
      if (!controller.signal.aborted) { setError(error.message); setState('error'); }
    });
    return () => controller.abort();
  }, [show.show_id, retry]);
  const current = detail?.show || show;
  const echoBase = safeEchoURL(current.detail_url, 'http://127.0.0.1:3002/show/' + current.show_id).replace(/\/show\/[^/]+$/, '');
  return <div className="eg-modal-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
    <section className="eg-dialog eg-detail-dialog" ref={ref} role="dialog" aria-modal="true" aria-labelledby="eg-detail-title">
      <div className="eg-dialog-top"><span className="eg-eyebrow">THE WHOLE PICTURE</span><button className="eg-icon-button" onClick={onClose} aria-label="关闭演出详情">×</button></div>
      <p className="eg-fineprint">数据更新：{dateLabel(detail?.updated_at||'')} · 缺失项目会明确标注；购票以确认页的最新信息为准。</p><div className="eg-detail-heading"><span className={`eg-lifecycle eg-lifecycle-${current.lifecycle}`}>{lifecycleLabel(current)}</span><h2 id="eg-detail-title">{current.name}</h2><p className="eg-muted">{current.type_name || '现场演出'} · {current.area}{current.address && current.address !== current.area ? ` · ${current.address}` : ''}</p><p className="eg-muted">{dateLabel(current.open_time)} — {dateLabel(current.end_time)}</p></div>
      {current.lifecycle === 'ended' && <p className="eg-history-notice">这场演出已结束。票档与评测用于回顾，不代表当前可购。</p>}
      <div className="eg-detail-tabs" role="tablist" aria-label="演出详情分类">{[['reviews','观众口碑'],['tickets','票档与规则'],['about','演出介绍']].map(([value,label]) => <button key={value} role="tab" aria-selected={tab === value} aria-controls={`eg-panel-${value}`} id={`eg-tab-${value}`} onClick={() => setTab(value)}>{label}</button>)}</div>
      {state === 'loading' && <p role="status" className="eg-notice">正在读取完整评测、评分分布和最新票档…</p>}
      {state === 'error' && <div role="alert" className="eg-notice"><p>{error}</p><button className="eg-secondary" onClick={() => setRetry(value => value + 1)}>重试</button></div>}
      {state === 'ready' && detail && <div role="tabpanel" id={`eg-panel-${tab}`} aria-labelledby={`eg-tab-${tab}`}>
        {detail.warnings.map((warning,i) => <p className="eg-inline-warning" key={i}>{warning}</p>)}
        {tab === 'reviews' && <ReviewPanel report={detail.reviews} echoScore={current.score} />}
        {tab === 'about' && <div className="eg-about-show"><p>{current.description || 'Echo 暂未提供演出介绍。'}</p><dl><div><dt>Echo 已售</dt><dd>{current.sold}</dd></div><div><dt>Echo 讨论数</dt><dd>{current.comments}</dd></div><div><dt>当前票档</dt><dd>{detail.items.length}</dd></div></dl></div>}
        {tab === 'tickets' && <><p className="eg-fineprint">票档核对：{dateLabel(detail.tickets_updated_at)}，余票会随其他用户购票变化。</p>{plan && <div className="eg-plan-carry" aria-label="已带入的购票需求"><span>已带入需求</span><span>{plan.party_size} 人</span><span>{plan.budget > 0 ? `${plan.budget_mode === 'per_person' ? '每人' : '总预算'} ¥${money(plan.budget)}` : '预算不限'}</span>{plan.seat_preference === 'front' && <span>偏好前排</span>}{plan.seat_preference === 'adjacent' && <span>偏好连座</span>}</div>}<div className="eg-section-label">{current.lifecycle === 'ended' ? '历史票档' : '票档比较'}{budget > 0 && <span>预算 ¥{money(budget)} / 人</span>}</div>{!detail.items.length && <p className="eg-notice">暂时没有可展示的票档。</p>}<div className="eg-tier-list">{detail.items.map(tier => <article key={tier.ticket_id} className={`eg-tier ${tier.sold_out ? 'eg-tier-sold' : ''}`}><div className="eg-tier-heading"><h3>{tier.title}</h3><strong>¥{money(tier.price)}</strong></div>{tier.sub_title && <p>{tier.sub_title}</p>}<div className="eg-tier-tags"><span>{tier.has_seats ? '需要选座' : '无需选座'}</span><span>{current.lifecycle === 'ended' ? '历史票档' : tier.sold_out ? '已售罄' : tier.stock_source === 'mysql_snapshot' ? '余票待 Echo 确认' : `参考余票 ${tier.stock} 张`}</span>{tier.is_seckill && <span>{current.lifecycle !== 'ended' && tier.seckill_active ? '限时抢票中' : '秒杀票'}</span>}{budget > 0 && <span className={tier.price <= budget ? 'eg-in-budget' : ''}>{tier.price <= budget ? '预算内' : '超出预算'}</span>}</div>{tier.is_seckill && <SeckillSchedule begin={tier.seckill_begin} end={tier.seckill_end} active={tier.seckill_active} ended={current.lifecycle === 'ended'} />}<details><summary>购票须知与退改规则</summary><p>{tier.rules || '此票档尚未提供具体规则，请到 Echo 查看。'}</p></details>{current.lifecycle !== 'ended' && tier.on_sale && !tier.sold_out && (!tier.is_seckill || tier.seckill_active) && <a className="eg-primary eg-tier-buy" href={checkoutLink(tier, plan)}>{plan && plan.party_size > 1 ? `${plan.party_size} 张，` : ''}选择此票档，确认订单 ↗</a>}</article>)}</div></>}
      </div>}
      <div className="eg-dialog-actions"><button className="eg-secondary" onClick={() => setRetry(value => value + 1)} disabled={state === 'loading'}>刷新数据 ↻</button><button className="eg-secondary" onClick={() => onDiscuss(current)}>让助手分析口碑</button><a className="eg-secondary" href={`${echoBase}/reviews/publish?showId=${encodeURIComponent(current.show_id)}&from=guide`}>写评分与评测 ↗</a><a className="eg-primary" href={safeEchoURL(current.detail_url, `http://127.0.0.1:3002/show/${current.show_id}`)} target="_blank" rel="noopener noreferrer">{current.lifecycle === 'ended' ? 'Echo 演出回顾' : '前往 Echo 查看 / 购票'} ↗</a></div>
      <p className="eg-fineprint">评分来自已读取的公开评测。选择票档后由你确认订单，最终票价、余票及付款以 Echo 为准。</p>
    </section>
  </div>;
}

export function ComparisonDialog({ ids, onClose, onDiscuss }: { ids: string[]; onClose: () => void; onDiscuss: () => void }) {
  const [entries, setEntries] = useState<Recommendation[]>([]);
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  const [retry, setRetry] = useState(0);
  const ref = useModal(onClose);
  const key = ids.join(',');
  useEffect(() => {
    const controller = new AbortController();setState('loading');
    ticketingAPI.compare(ids, controller.signal).then(data => { setEntries(data.items); setState('ready'); }).catch(error => { if (!controller.signal.aborted) { setError(error.message); setState('error'); } });
    return () => controller.abort();
  }, [key, retry]);
  return <div className="eg-modal-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}><section className="eg-dialog eg-compare-dialog" ref={ref} role="dialog" aria-modal="true" aria-labelledby="eg-compare-title">
    <div className="eg-dialog-top"><span className="eg-eyebrow">SIDE BY SIDE</span><button className="eg-icon-button" onClick={onClose} aria-label="关闭演出对比">×</button></div><h2 id="eg-compare-title">喜欢的现场，放在一起看</h2><p className="eg-muted">比较价格、口碑、评分样本和选座要求，找到自己的选择。</p>
    {state === 'loading' && <p role="status" className="eg-notice">正在读取对比数据…</p>}
    {state === 'error' && <div role="alert" className="eg-notice"><p>{error}</p><button className="eg-secondary" onClick={() => setRetry(value => value + 1)}>重试</button></div>}
    {state === 'ready' && <div className="eg-comparison-scroll"><table className="eg-comparison-table"><caption>演出与口碑比较</caption><thead><tr><th scope="col">比较项目</th>{entries.map(entry => <th key={entry.show.show_id} scope="col">{entry.show.name}</th>)}</tr></thead><tbody>
      <tr><th scope="row">状态</th>{entries.map(entry => <td key={entry.show.show_id}>{lifecycleLabel(entry.show)}</td>)}</tr>
      <tr><th scope="row">类型 / 场馆</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.show.type_name}<br />{entry.show.address}</td>)}</tr>
      <tr><th scope="row">演出时间</th>{entries.map(entry => <td key={entry.show.show_id}>{dateLabel(entry.show.open_time)}</td>)}</tr>
      <tr><th scope="row">票档价格</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.tickets.length ? `¥${money(Math.min(...entry.tickets.map(tier => tier.price)))} – ¥${money(Math.max(...entry.tickets.map(tier => tier.price)))}` : '暂未提供'}{entry.show.lifecycle === 'ended' && <small>历史票档价格</small>}</td>)}</tr>
      <tr><th scope="row">观众评测均分</th>{entries.map(entry => <td key={entry.show.show_id}><strong>{ratingLabel(entry.reviews.summary)}</strong><small>{entry.reviews.summary.count} 个有效评分{!entry.reviews.summary.complete && ' · 统计未完整'}</small></td>)}</tr>
      <tr><th scope="row">Echo 展示分</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.show.score > 0 ? `${entry.show.score.toFixed(1)} / 5` : '暂无'}</td>)}</tr>
      <tr><th scope="row">4–5 星占比</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.reviews.summary.count ? `${(entry.reviews.summary.positive_rate * 100).toFixed(0)}%` : '暂无'}</td>)}</tr>
      <tr><th scope="row">选座 / 余票</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.tickets.some(tier => tier.has_seats) ? '包含选座票档' : '未提供选座票档'}<small>{entry.show.lifecycle === 'ended' ? '已结束，当前不可购' : `${entry.tickets.reduce((sum,tier) => sum + (tier.on_sale && !tier.sold_out ? tier.stock : 0),0)} 张参考余票`}</small></td>)}</tr>
      <tr><th scope="row">高分原文</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.reviews.positive[0]?.content || '暂无高分文字评测'}</td>)}</tr>
      <tr><th scope="row">不同意见</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.reviews.critical[0]?.content || '暂无可引用的中低分文字评测'}</td>)}</tr>
      <tr><th scope="row">需要注意</th>{entries.map(entry => <td key={entry.show.show_id}>{entry.cautions.join('；') || '购票规则与价格请在 Echo 确认'}</td>)}</tr>
    </tbody></table></div>}
    <div className="eg-dialog-actions"><button className="eg-primary" disabled={state !== 'ready'} onClick={onDiscuss}>让助手给出对比建议 ↗</button></div>
  </section></div>;
}
