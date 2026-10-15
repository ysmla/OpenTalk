import { useEffect, useState } from 'react';
import { listNotifications, markNotificationRead, type Notification } from '../api/notifications';
import { Alert, Status } from '../components/Alert';
import { authorLabel } from '../components/ContentMeta';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type NotificationsViewProps = {
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

const EVENT_LABEL: Record<string, string> = {
 new_comment: '新评论',
 new_answer: '新回答',
 new_reply: '新回复',
 moderation_result: '审核结果',
};

/** 通知只带内容 ID，所以链接走内容页路由；它按 kind 自行分发到主题、回答或评论。 */
function contentHref(contentId: string): string {
 return `#/topic/${encodeURIComponent(contentId)}`;
}

/**
 * 站内通知。
 *
 * 载荷是服务端生成的**安全摘要**加内容定位：不含 actor_id、不含原文快照，内容不可见时连作者都没有。
 * 因此这里不做任何推断——摘要直接展示，作者只在服务端给了才显示，内容不可见时点进去会看到"不可见"。
 *
 * 未读与已读的区别只体现在标签与按钮上：标记已读是幂等的，所以失败也不值得打断读者。
 */
export function NotificationsView({ sessionReady, signedIn, onUnauthenticated }: NotificationsViewProps) {
 const [items, setItems] = useState<Notification[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(true);
 const [busy, setBusy] = useState(false);
 const [failure, setFailure] = useState<Failure>();

 useEffect(() => {
  if (!sessionReady) return;
  if (!signedIn) {
   setLoading(false);
   onUnauthenticated();
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  listNotifications({}, controller.signal).then(page => {
   if (controller.signal.aborted) return;
   setItems(page.items);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  }).catch((error: unknown) => {
   if (!controller.signal.aborted) setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [sessionReady, signedIn, onUnauthenticated]);

 async function loadMore() {
  if (busy || cursor === undefined) return;
  setBusy(true);
  try {
   const page = await listNotifications({ cursor });
   setItems(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 async function markRead(notification: Notification) {
  try {
   await markNotificationRead(notification.id);
   // 就地改状态而不是重新拉整页：标记已读不该让读者的位置跳走。
   setItems(current => current.map(item => item.id === notification.id
    ? { ...item, read_at: new Date().toISOString() }
    : item));
  } catch (error) {
   setFailure(describeError(error));
  }
 }

 const unread = items.filter(item => !item.read_at).length;
 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>通知</h1>
  <p className="hint">别人回复你、回答你的提问，或你的举报有结果时会出现在这里；邮件与站内通知是两件事，关掉邮件不影响这里。</p>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {!loading && items.length === 0 && !failure && <p className="hint">还没有通知。</p>}
  {items.length > 0 && <>
   <p className="hint">
    已加载 {items.length} 条{unread > 0 ? `，其中 ${unread} 条未读` : ''}{hasMore ? '，还有更多' : ''}。
   </p>
   <ul className="reports">
    {items.map(item => <li key={item.id}>
     <p className="byline">
      <span className="chip">{EVENT_LABEL[item.event_type] ?? item.event_type}</span>
      {!item.read_at && <span className="chip">未读</span>}
      <span className="time">{formatDateTime(item.created_at)}</span>
     </p>
     {/* 摘要是服务端生成的安全文本；不在这里拼装或推断任何内容。 */}
     <p>{item.summary}</p>
     {item.actor !== undefined && <p className="hint">{authorLabel(item.actor)}</p>}
     <div className="actions">
      {item.content_id !== undefined && <a href={contentHref(item.content_id)}>查看相关内容</a>}
      {!item.read_at && <button type="button" className="link" onClick={() => void markRead(item)}>标记已读</button>}
     </div>
    </li>)}
   </ul>
   {hasMore && <div className="actions">
    <button type="button" className="secondary" disabled={busy} onClick={() => void loadMore()}>
     {busy ? '正在加载…' : '加载更多通知'}
    </button>
   </div>}
  </>}
 </main>;
}
