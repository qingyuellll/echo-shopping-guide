import { FormEvent, useEffect, useState } from 'react';
import { ticketingAPI } from '../api/ticketing';
import type { ReviewFeedPage } from '../api/ticketing';
import { ReviewCard } from './DiscoveryDetails';

export function ReviewFeed({ onSelect }: { onSelect: (id: string) => void }) {
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState('all');
  const [sort, setSort] = useState('latest');
  const [page, setPage] = useState(1);
  const [retry, setRetry] = useState(0);
  const [data, setData] = useState<ReviewFeedPage>();
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  useEffect(() => {
    const controller = new AbortController(); setState('loading');
    ticketingAPI.reviewFeed(query, filter, sort, page, controller.signal, retry > 0).then(result => { setData(result); setState('ready'); }).catch(error => { if (!controller.signal.aborted) { setError(error.message); setState('error'); } });
    return () => controller.abort();
  }, [query, filter, sort, page, retry]);
  function searchReviews(event: FormEvent) { event.preventDefault(); setPage(1); setQuery(search.trim()); }
  return <section className="eg-global-reviews" aria-label="全站公开观众评测">
    <div className="eg-feed-heading"><span className="eg-eyebrow">VOICES FROM THE AUDIENCE</span><h2>先听回响，再选现场。</h2><p>完整浏览 Echo 的公开评测，文字长评和纯评分都能找到。</p></div>
    {data && <div className="eg-feed-summary"><strong>{data.summary.total}<small>条公开评测</small></strong><span>已读取 {data.summary.complete ? '全部' : ''} {data.summary.count} 个有效评分 · {data.summary.text_count} 条含文字内容</span><button onClick={() => setRetry(value => value + 1)} disabled={state === 'loading'}>更新评测 ↻</button></div>}
    {!!data?.unlisted_count && <p className="eg-coverage-note">其中 {data.unlisted_count} 条关联的演出当前未在公开列表中，仍可阅读评测原文；这些内容不会作为当前购票推荐。</p>}
    {data?.warnings.map((warning,i) => <p className="eg-inline-warning" key={i}>{warning}</p>)}
    <form className="eg-search" onSubmit={searchReviews}><span aria-hidden="true">⌕</span><input aria-label="搜索评测正文、作者或演出" maxLength={100} value={search} onChange={event => setSearch(event.target.value)} placeholder="搜演出、评测内容或观众昵称" /><button>搜索</button></form>
    <div className="eg-review-toolbar"><label>评测内容<select value={filter} onChange={event => {setPage(1);setFilter(event.target.value);}}><option value="all">全部（含纯评分）</option><option value="text">只看文字评测</option><option value="positive">4–5 星</option><option value="critical">1–3 星</option></select></label><label>排序<select value={sort} onChange={event => {setPage(1);setSort(event.target.value);}}><option value="latest">最新发表</option><option value="helpful">赞同最多</option><option value="low">低分优先</option></select></label></div>
    {state === 'loading' && <p className="eg-notice" role="status">正在读取全站公开评测…</p>}
    {state === 'error' && <div className="eg-notice" role="alert"><p>{error}</p><button className="eg-secondary" onClick={() => setRetry(value => value + 1)}>重试</button></div>}
    {state === 'ready' && data && <><p className="eg-review-count" role="status">{data.total} 条匹配 · 第 {page} 页</p>{!data.items.length && <p className="eg-notice">这个条件下暂无评测，可以调整关键词或评分范围。</p>}{data.items.map(item => <div key={item.review_id} className="eg-feed-entry"><div className="eg-feed-show"><span>{item.show_name}</span>{item.show_available ? <button onClick={() => onSelect(item.show_id)}>{item.lifecycle === 'ended' ? '查看演出回顾' : '查看演出口碑'} ↗</button> : <small>关联演出当前不可查看</small>}</div><ReviewCard item={item} /></div>)}{(page > 1 || data.has_more) && <div className="eg-pagination"><button disabled={page <= 1} onClick={() => setPage(value => value - 1)}>← 上一页</button><span>第 {page} / {Math.max(1,Math.ceil(data.total/10))} 页</span><button disabled={!data.has_more} onClick={() => setPage(value => value + 1)}>下一页 →</button></div>}</>}
  </section>;
}
