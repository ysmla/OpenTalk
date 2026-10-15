import { useEffect, useState, type FormEvent } from 'react';
import { listAllBoards, listAllTags, searchContents, type PublicContent } from '../api/content';
import type { CurrentUser } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { ContentCard } from '../components/ContentCard';
import { Markdown } from '../components/Markdown';
import { routeHref } from '../hooks/useHashRoute';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type SearchViewProps = {
 /** The term from the hash. Empty means the reader has not searched yet. */
 q?: string;
 boardId?: string;
 tagId?: string;
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
 * The term and the filters live in the hash (`#/search?q=…&board_id=…&tag_id=…`), so a narrowed result page
 * can be linked, reloaded and shared without any state of ours. An empty box is not a search: the server
 * refuses an empty `q`, and the view does not call it.
 */
export function SearchView({ q, boardId, tagId, sessionReady, onUnauthenticated }: SearchViewProps) {
 const [term, setTerm] = useState(q ?? '');
 const [results, setResults] = useState<PublicContent[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(false);
 const [loadingMore, setLoadingMore] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [boards, setBoards] = useState<{ id: string; name: string }[]>([]);
 const [tags, setTags] = useState<{ id: string; name: string }[]>([]);

 // The box follows the hash, so navigating to a search URL (or going back) shows the term it searched for.
 useEffect(() => {
  setTerm(q ?? '');
 }, [q]);

 // Filter choices. Boards and tags are read the same way every other view reads them: all pages of the
 // public list, so a filter cannot silently omit a board that fell on a later page.
 useEffect(() => {
  const controller = new AbortController();
  Promise.all([listAllBoards(controller.signal), listAllTags(controller.signal)])
   .then(([boardList, tagList]) => {
    if (controller.signal.aborted) return;
    setBoards(boardList.map(board => ({ id: board.id, name: board.name })));
    setTags(tagList.map(tag => ({ id: tag.id, name: tag.name })));
   })
   .catch(() => {
    // A filter list that cannot be loaded is not fatal: search without filters still works, and the
    // request failure for the search itself is reported separately.
   });
  return () => controller.abort();
 }, []);

 useEffect(() => {
  if ((q ?? '') === '') {
   setResults([]);
   setCursor(undefined);
   setHasMore(false);
   setFailure(undefined);
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  searchContents({ q: q ?? '', board_id: boardId, tag_id: tagId }, controller.signal).then(page => {
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
 }, [q, boardId, tagId]);

 function go(next: { q: string; boardId?: string; tagId?: string }) {
  window.location.hash = routeHref({ name: 'search', q: next.q, boardId: next.boardId, tagId: next.tagId }).slice(1);
 }

 function handleSubmit(event: FormEvent) {
  event.preventDefault();
  const next = term.trim();
  if (next === '') return;
  go({ q: next, boardId, tagId });
 }

 // Changing a filter keeps the term that was searched and re-runs against it; the box may hold text the
 // reader has not submitted yet, and silently searching that would be a surprise.
 function handleFilter(board?: string, tag?: string) {
  if ((q ?? '') === '') return;
  go({ q: q ?? '', boardId: board, tagId: tag });
 }

 async function loadMore() {
  if (loadingMore || cursor === undefined) return;
  setLoadingMore(true);
  try {
   const page = await searchContents({ q: q ?? '', board_id: boardId, tag_id: tagId, cursor });
   setResults(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setLoadingMore(false);
  }
 }

 const filtered = boardId !== undefined || tagId !== undefined;
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
    <p className="hint">匹配标题与正文，最多 100 个字符；只搜索可见内容。</p>
   </div>
   <div className="field">
    <label htmlFor="search-board">板块</label>
    <select
     id="search-board"
     value={boardId ?? ''}
     onChange={event => handleFilter(event.target.value === '' ? undefined : event.target.value, tagId)}
    >
     <option value="">全部板块</option>
     {boards.map(board => <option key={board.id} value={board.id}>{board.name}</option>)}
    </select>
   </div>
   <div className="field">
    <label htmlFor="search-tag">标签</label>
    <select
     id="search-tag"
     value={tagId ?? ''}
     onChange={event => handleFilter(boardId, event.target.value === '' ? undefined : event.target.value)}
    >
     <option value="">全部标签</option>
     {tags.map(tag => <option key={tag.id} value={tag.id}>{tag.name}</option>)}
    </select>
   </div>
   <div className="actions">
    <button type="submit" disabled={term.trim() === ''}>搜索</button>
   </div>
  </form>
  {!sessionReady && <Status>正在确认登录状态…</Status>}

  {loading && <Status>正在搜索…</Status>}
  <Alert failure={failure} />

  {(q ?? '') !== '' && !loading && results.length === 0 && !failure &&
   <p className="hint">没有找到与「{q}」匹配的可见内容{filtered ? '（当前有筛选条件）' : ''}。</p>}

  {results.length > 0 && <>
   <p className="hint">
    已加载 {results.length} 条{hasMore ? '，还有更多' : ''}；按最新排序。
   </p>
   <ul className="topics">
    {results.map(item => <li key={item.id}><ContentCard item={item} /></li>)}
   </ul>
   {hasMore && <div className="actions">
    <button type="button" className="secondary" disabled={loadingMore} onClick={() => void loadMore()}>
     {loadingMore ? '正在加载…' : '加载更多结果'}
    </button>
   </div>}
  </>}
 </main>;
}
