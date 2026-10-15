import { useEffect, useState } from 'react';
import { listAllBoards, listTopics, isTopic, type Board, type Topic, type TopicSort } from '../api/content';
import type { CurrentUser } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { authorLabel, TagList, TopicStatus } from '../components/ContentMeta';
import { useCursorList } from '../hooks/useCursorList';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type TopicsViewProps = {
 user?: CurrentUser;
 sessionReady: boolean;
 /** `undefined` means 全部板块; a board id filters the list server-side. */
 boardId?: string;
 /** A tag id filters the list server-side; set from `#/tag/{id}`. */
 tagId?: string;
 onUnauthenticated: () => void;
};

const SORTS: { value: TopicSort; label: string }[] = [
 { value: 'newest', label: '最新' },
 { value: 'active', label: '最近活跃' },
];

/** `GET /api/v1/topics` only ever returns topics, but the union allows more. */
function topicOf(item: Parameters<typeof isTopic>[0]): Topic | undefined {
 return isTopic(item) ? item : undefined;
}

export function TopicsView({ user, sessionReady, boardId, tagId, onUnauthenticated }: TopicsViewProps) {
 const [sort, setSort] = useState<TopicSort>('newest');
 const [boards, setBoards] = useState<Board[]>([]);
 const [boardFailure, setBoardFailure] = useState<Failure>();

 // The board strip is a convenience filter, so a failure here must not hide the
 // list itself: it is reported next to the strip and the list still loads. All
 // pages are read, so a board catalogue that outgrows one page stays complete.
 useEffect(() => {
  const controller = new AbortController();
  listAllBoards(controller.signal).then(items => {
   if (!controller.signal.aborted) setBoards(items);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setBoardFailure(describeError(error));
  });
  return () => controller.abort();
 }, []);

 const filterKey = [boardId ?? '', tagId ?? '', sort].join('|');
 const list = useCursorList<Topic>(
  filterKey,
  (cursor, signal) => listTopics({ cursor, board_id: boardId, tag_id: tagId, sort }, signal).then(page => ({
   items: page.items.map(topicOf).filter((topic): topic is Topic => topic !== undefined),
   has_more: page.has_more,
   next_cursor: page.next_cursor,
  })),
 );

 const activeBoard = boards.find(board => board.id === boardId);
 const boardName = activeBoard?.name;

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>{tagId !== undefined ? '标签下的主题' : boardName ? `${boardName} 的主题` : '主题列表'}</h1>
  {boardId !== undefined && boardName === undefined && !boardFailure &&
   <p className="hint">正在读取板块信息，或该板块不存在，下面显示的是全部板块的主题。</p>}
  {tagId !== undefined && <p className="hint">正在按标签筛选（标签 #{tagId}）。</p>}

  <nav className="links" aria-label="板块筛选">
   <a href="#/" aria-current={boardId === undefined ? 'page' : undefined}>全部板块</a>
   {boards.map(board => <a
    key={board.id}
    href={`#/board/${encodeURIComponent(board.id)}`}
    aria-current={board.id === boardId ? 'page' : undefined}
    title={board.description}
   >
    {/* The kind marker is only useful when the name does not already say it: the seeded Q&A board is
        literally called 问答, and "问答（问答）" reads like a rendering bug. */}
    {board.name}{board.kind === 'qa' && !board.name.includes('问答') ? '（问答）' : ''}{board.is_archived ? '（已停用）' : ''}
   </a>)}
  </nav>
  <Alert failure={boardFailure} />

  <div className="toolbar">
   <div className="field">
    <label htmlFor="topic-sort">排序</label>
    <select id="topic-sort" value={sort} onChange={event => setSort(event.target.value as TopicSort)}>
     {SORTS.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
    </select>
   </div>
   <div className="actions">
    {user
     ? <a href={boardId ? `#/new?board_id=${encodeURIComponent(boardId)}` : '#/new'}>发帖 / 提问</a>
     : <button
      type="button"
      className="secondary"
      disabled={!sessionReady}
      // 未登录时不给写入口，只引导登录。
      onClick={onUnauthenticated}
     >
      登录后发帖
     </button>}
   </div>
  </div>

  {list.loading && <Status>正在加载主题…</Status>}
  {!list.loading && list.items.length === 0 && !list.failure &&
   <p>这里还没有主题。{user ? '你可以发布第一个主题。' : '登录后可以发布主题。'}</p>}

  {list.items.length > 0 && <ul className="cards">
   {list.items.map(topic => <TopicCard
    key={topic.id}
    topic={topic}
    boardName={boards.find(board => board.id === topic.board_id)?.name}
   />)}
  </ul>}

  <Alert failure={list.failure} />
  <div className="actions">
   {list.hasMore && <button type="button" onClick={list.loadMore} disabled={list.loadingMore}>
    {list.loadingMore ? '正在加载…' : '加载更多'}
   </button>}
   {list.failure && <button type="button" className="secondary" onClick={list.reload}>重试</button>}
  </div>
 </main>;
}

function TopicCard({ topic, boardName }: { topic: Topic; boardName?: string }) {
 const createdAt = formatDateTime(topic.created_at);
 const lastActivityAt = formatDateTime(topic.last_activity_at);
 return <li className="card">
  <a className="card-title" href={`#/topic/${encodeURIComponent(topic.id)}`}>{topic.title}</a>
  <p className="byline">
   {authorLabel(topic.author)}
   <TopicStatus topic={topic} />
  </p>
  <TagList tags={topic.tags} />
  <p className="byline">
   <a href={`#/board/${encodeURIComponent(topic.board_id)}`}>
    {boardName ? `板块：${boardName}` : `板块 #${topic.board_id}`}
   </a>
   {createdAt && <span className="time">发布于 {createdAt}</span>}
   {lastActivityAt && <span className="time">最后活动 {lastActivityAt}</span>}
   {topic.edited_at && <span className="hint">已编辑</span>}
  </p>
 </li>;
}
