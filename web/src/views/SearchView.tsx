import { useEffect, useState, type FormEvent } from 'react';
import { isDeleted, isQuestion, searchContents, type PublicContent } from '../api/content';
import type { CurrentUser } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { authorLabel, TagList, TopicStatus } from '../components/ContentMeta';
import { Markdown } from '../components/Markdown';
import { routeHref } from '../hooks/useHashRoute';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type SearchViewProps = {
 /** The term from the hash. Empty means the reader has not searched yet. */
 q?: string;
 user?: CurrentUser;
 sessionReady: boolean;
 onUnauthenticated: () => void;
};

/**
 * Public search over visible content.
 *
 * The endpoint returns topics, answers and comments in one page, so each result is labelled with what it is
 * and, for anything that is not a topic, linked to the discussion it belongs to — the answer or comment
 * itself has no page of its own.
 *
 * The term lives in the hash (`#/search?q=…`), so a result page can be linked, reloaded and shared without
 * any state of ours. An empty box is not a search: the server refuses an empty `q`, and the view does not
 * call it.
 */
export function SearchView({ q, sessionReady, onUnauthenticated }: SearchViewProps) {
 const [term, setTerm] = useState(q ?? '');
 const [submitted, setSubmitted] = useState(q ?? '');
 const [results, setResults] = useState<PublicContent[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(false);
 const [loadingMore, setLoadingMore] = useState(false);
 const [failure, setFailure] = useState<Failure>();

 // The hash is the source of truth: navigating to a search URL, or reloading one, re-runs the search.
 useEffect(() => {
  setTerm(q ?? '');
  setSubmitted(q ?? '');
 }, [q]);

 useEffect(() => {
  if (submitted === '') {
   setResults([]);
   setCursor(undefined);
   setHasMore(false);
   setFailure(undefined);
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  searchContents({ q: submitted }, controller.signal).then(page => {
   if (controller.signal.aborted) return;
   setResults(page.items);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  }).catch((error: unknown) => {
   if (!controller.signal.aborted) setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [submitted]);

 function handleSubmit(event: FormEvent) {
  event.preventDefault();
  const next = term.trim();
  if (next === '') return;
  if (next === submitted) return;
  // Writing the hash is what starts the search: the effect above owns the request.
  window.location.hash = routeHref({ name: 'search', q: next }).slice(1);
 }

 async function loadMore() {
  if (loadingMore || cursor === undefined) return;
  setLoadingMore(true);
  try {
   const page = await searchContents({ q: submitted, cursor });
   setResults(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setLoadingMore(false);
  }
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>搜索</h1>
  <form className="composer" onSubmit={handleSubmit} role="search">
   <div className="field">
    <label htmlFor="search-term">搜索公开内容</label>
    <input
     id="search-term"
     type="search"
     value={term}
     maxLength={100}
     onChange={event => setTerm(event.target.value)}
     placeholder="标题或正文里的词"
    />
    <p className="hint">匹配标题与正文的子串，最多 100 个字符；只搜索可见内容。</p>
   </div>
   <div className="actions">
    <button type="submit" disabled={term.trim() === ''}>搜索</button>
   </div>
  </form>
  {!sessionReady && <Status>正在确认登录状态…</Status>}

  {loading && <Status>正在搜索…</Status>}
  <Alert failure={failure} />

  {submitted !== '' && !loading && results.length === 0 && !failure &&
   <p className="hint">没有找到与「{submitted}」匹配的可见内容。</p>}

  {results.length > 0 && <>
   <p className="hint">共 {results.length} 条结果，按最新排序。</p>
   <ul className="topics">
    {results.map(item => <li key={item.id}><SearchResult item={item} /></li>)}
   </ul>
   {hasMore && <div className="actions">
    <button type="button" className="secondary" disabled={loadingMore} onClick={() => void loadMore()}>
     {loadingMore ? '正在加载…' : '加载更多结果'}
    </button>
   </div>}
  </>}
 </main>;
}

/** One result: what it is, who wrote it under the anonymity rules, and where to read it. */
function SearchResult({ item }: { item: PublicContent }) {
 if (isDeleted(item)) {
  // A tombstone in a result list says only that something used to be here; the server never sends the
  // original text or the author, and this view must not invent either.
  return <article className="result">
   <p className="byline"><span className="chip">已删除</span></p>
   <p className="hint">{item.message}</p>
  </article>;
 }
 const isTopic = item.kind === 'discussion' || item.kind === 'question';
 // Only a topic has its own page; an answer or comment is read inside its discussion.
 const href = isTopic ? `#/topic/${encodeURIComponent(item.id)}` : `#/topic/${encodeURIComponent(item.topic_id)}`;
 const createdAt = formatDateTime(item.created_at);
 return <article className="result">
  <p className="byline">
   <span className="chip">{item.kind === 'question' ? '问题' : item.kind === 'answer' ? '回答' : item.kind === 'comment' ? '评论' : '帖子'}</span>
   {authorLabel(item.author)}
   {createdAt && <span className="time">{createdAt}</span>}
  </p>
  {isTopic && <h2><a href={href}>{item.title}</a></h2>}
  {!isTopic && <p className="hint">在 <a href={href}>该主题</a> 下</p>}
  <Markdown source={item.body_md} />
  {isTopic && item.tags.length > 0 && <TagList tags={item.tags} />}
  {isTopic && isQuestion(item) && <TopicStatus topic={item} />}
 </article>;
}
