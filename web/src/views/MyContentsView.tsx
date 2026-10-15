import { useEffect, useState } from 'react';
import type { PublicContent } from '../api/content';
import { listMyContents, listUserContents } from '../api/profiles';
import { Alert, Status } from '../components/Alert';
import { ContentCard } from '../components/ContentCard';
import { describeError, type Failure } from '../lib/messages';

type ContentListProps = {
 /** When set, the list is that account's public content; otherwise it is the reader's own. */
 publicId?: string;
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

/**
 * 内容列表：本人内容（`/me/contents`）或某个账号的公开内容。
 *
 * 两个列表在界面层看起来一样，差别全在服务端：本人列表**包含自己的匿名帖**并以匿名模式渲染；公开列表
 * 在 SQL 里就排除了匿名行。界面不做任何一侧的过滤——把它写在这里就等于给了读取器一个泄露的机会。
 */
export function ContentListView({ publicId, sessionReady, signedIn, onUnauthenticated }: ContentListProps) {
 const [items, setItems] = useState<PublicContent[]>([]);
 const [cursor, setCursor] = useState<string>();
 const [hasMore, setHasMore] = useState(false);
 const [loading, setLoading] = useState(true);
 const [busy, setBusy] = useState(false);
 const [failure, setFailure] = useState<Failure>();

 useEffect(() => {
  // The public list needs no session; the private one does.
  if (publicId === undefined) {
   if (!sessionReady) return;
   if (!signedIn) {
    setLoading(false);
    onUnauthenticated();
    return;
   }
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  const load = publicId === undefined
   ? listMyContents({}, controller.signal)
   : listUserContents(publicId, {}, controller.signal);
  load.then(page => {
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
 }, [publicId, sessionReady, signedIn, onUnauthenticated]);

 async function loadMore() {
  if (busy || cursor === undefined) return;
  setBusy(true);
  try {
   const page = publicId === undefined
    ? await listMyContents({ cursor })
    : await listUserContents(publicId, { cursor });
   setItems(current => [...current, ...page.items]);
   setCursor(page.next_cursor);
   setHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 const own = publicId === undefined;
 return <>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {!loading && items.length === 0 && !failure &&
   <p className="hint">{own ? '你还没有发布过内容。' : '该用户还没有公开内容。'}</p>}
  {items.length > 0 && <>
   <p className="hint">已加载 {items.length} 条{hasMore ? '，还有更多' : ''}；按最新排序。</p>
   <ul className="topics">
    {items.map(item => <li key={item.id}><ContentCard item={item} /></li>)}
   </ul>
   {hasMore && <div className="actions">
    <button type="button" className="secondary" disabled={busy} onClick={() => void loadMore()}>
     {busy ? '正在加载…' : '加载更多'}
    </button>
   </div>}
  </>}
 </>;
}

/** 本人内容页：`#/me/contents`。 */
export function MyContentsView({ sessionReady, signedIn, onUnauthenticated }: {
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
}) {
 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>我的内容</h1>
  <p className="hint">你发布的帖子、回答与评论；<strong>你自己的匿名帖也会出现在这里</strong>，并仍以匿名身份展示。被隐藏或删除的内容不会列出——处置结果可在通知里看到。</p>
  <ContentListView sessionReady={sessionReady} signedIn={signedIn} onUnauthenticated={onUnauthenticated} />
 </main>;
}
