import { useEffect, useState } from 'react';
import { createComment, deleteContent, listComments, type Comment } from '../api/content';
import { Alert, Status } from '../components/Alert';
import { CommentComposer, CommentList } from '../components/CommentThread';
import { ImageUploader, type UploadItem } from '../components/ImageUploader';
import { useCursorList } from '../hooks/useCursorList';
import { describeError, isStatus, type Failure } from '../lib/messages';
import { contentActions, type Viewer } from '../lib/ownership';

type CommentsSectionProps = {
 topicId: string;
 /** Omitted for 主题评论; set to an answer id for 回答下的评论. */
 answerId?: string;
 /** `can_create_comment` from GET /api/v1/contents/{content_id}/permissions. */
 canComment: boolean;
 /** 主题已锁定或问题已解决：这是"不能评论"的原因本身，必须与"未登录"分开说。 */
 closed?: boolean;
 /**
  * 主题是否被锁定。与 `closed` 不同：已解决只禁止新增互动，**不**禁止编辑正文，
  * 所以编辑入口要看这个字段（服务端 EditContent 也只拒绝锁定）。
  */
 topicIsLocked?: boolean;
 /** 当前是否有会话。为 false 时才知道该提示登录而不是提示账号受限。 */
 signedIn?: boolean;
 /** 用于推导"这条评论是不是我的"（见 lib/ownership.ts）。 */
 viewer: Viewer;
 /** 当前用户发评论是否强制匿名（仅本人可见的权限字段）。 */
 forceAnonymous?: boolean;
 onUnauthenticated: () => void;
 emptyText: string;
};

/** The replies of one thread root, with its own cursor. */
type ThreadState = {
 replies: Comment[];
 cursor?: string;
 hasMore: boolean;
 loading: boolean;
};

/**
 * One comment list over one topic or one answer, with the composer beneath it.
 *
 * 两层展示: the list holds top-level comments (`parent_comment_id` omitted) and
 * renders their replies under them. A reply posts `reply_to_comment_id` — the real
 * target — and the server derives the display root, so the real tree can be deeper
 * than the two levels shown here.
 *
 * Both levels page on their own cursor, which is what the endpoint's shape asks
 * for: the top level is an accumulating cursor list, and each thread root's
 * replies are one more page hung off it. Paging one level at a time is also what
 * keeps the display consistent — a page mixing both could show a reply whose root
 * is not on screen.
 */
export function CommentsSection({ topicId, answerId, canComment, closed, topicIsLocked, signedIn, viewer, forceAnonymous, onUnauthenticated, emptyText }: CommentsSectionProps) {
 const listKey = `${topicId}:${answerId ?? 'topic'}`;
 // The endpoint pages PublicContent (a topic, an answer, a comment or a tombstone); this section only
 // ever wants the comments, so the projection happens once, here.
 const topList = useCursorList<Comment>(listKey, async (cursor, signal) => {
  const page = await listComments(topicId, { answer_id: answerId, cursor }, signal);
  return {
   items: page.items.filter((item): item is Comment => item.kind === 'comment'),
   has_more: page.has_more,
   next_cursor: page.next_cursor,
  };
 });
 const tops = topList.items;

 const [threads, setThreads] = useState<Record<string, ThreadState>>({});
 /** Ids removed locally, so a delete does not have to refetch and lose the pages already read. */
 const [deletedIds, setDeletedIds] = useState<string[]>([]);
 const [draft, setDraft] = useState('');
 const [images, setImages] = useState<UploadItem[]>([]);
 const [target, setTarget] = useState<Comment>();
 // 每一条评论/回复独立选择身份（requirements.md:19）。匿名楼主由服务端强制匿名，
 // 这个开关对它固定为选中且不可关闭；其他参与者可以逐条选择。
 const [anonymous, setAnonymous] = useState(false);
 const [posting, setPosting] = useState(false);
 const [postFailure, setPostFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [deletingId, setDeletingId] = useState<string>();
 const [loadFailure, setLoadFailure] = useState<Failure>();

 // One request per top-level comment on the page that has no replies loaded yet. They are issued
 // together, and the effect re-runs only when the set of top-level ids changes (a new page, or a
 // comment the reader just posted), not on every render.
 const topIds = tops.map(top => top.id).join(',');
 useEffect(() => {
  const controller = new AbortController();
  const pending = tops.filter(top => threads[top.id] === undefined);
  if (pending.length === 0) return () => controller.abort();
  Promise.all(pending.map(async top => ({
   id: top.id,
   page: await listComments(topicId, { answer_id: answerId, parent_comment_id: top.id }, controller.signal),
  }))).then(results => {
   if (controller.signal.aborted) return;
   setThreads(current => {
    const next = { ...current };
    for (const { id, page } of results) {
     next[id] = {
      replies: page.items.filter((item): item is Comment => item.kind === 'comment'),
      cursor: page.next_cursor,
      hasMore: page.has_more,
      loading: false,
     };
    }
    return next;
   });
  }).catch((error: unknown) => {
   if (!controller.signal.aborted) setLoadFailure(describeError(error));
  });
  return () => controller.abort();
  // `threads` is read only to skip what is already loaded; listing it would re-run the effect on its
  // own write and fetch the same replies forever.
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [topIds, topicId, answerId]);

 async function loadMoreReplies(rootId: string) {
  const thread = threads[rootId];
  if (thread === undefined || thread.loading || !thread.hasMore || thread.cursor === undefined) return;
  setThreads(current => ({ ...current, [rootId]: { ...current[rootId], loading: true } }));
  setLoadFailure(undefined);
  try {
   const page = await listComments(topicId, { answer_id: answerId, parent_comment_id: rootId, cursor: thread.cursor });
   const more = page.items.filter((item): item is Comment => item.kind === 'comment');
   setThreads(current => {
    const existing = current[rootId];
    return {
     ...current,
     [rootId]: {
      replies: [...(existing?.replies ?? []), ...more],
      cursor: page.next_cursor,
      hasMore: page.has_more,
      loading: false,
     },
    };
   });
  } catch (error) {
   setThreads(current => ({ ...current, [rootId]: { ...current[rootId], loading: false } }));
   setLoadFailure(describeError(error));
  }
 }

 /**
  * Delete one comment or reply. The server authorizes it again; the entry is only
  * rendered for content the page derived as the viewer's own. A removed top-level
  * comment takes the replies shown under it with it, because a reply's
  * `parent_comment_id` is that display root — which the filter below covers.
  */
 async function handleDelete(comment: Comment) {
  const scope = comment.parent_comment_id === undefined ? '它下面的回复会一并删除，' : '';
  if (!window.confirm(`确定删除这条${comment.parent_comment_id === undefined ? '评论' : '回复'}吗？${scope}删除后不可恢复。`)) return;
  setDeletingId(comment.id);
  setPostFailure(undefined);
  setNotice(undefined);
  try {
   await deleteContent(comment.id);
   setDeletedIds(current => [...current, comment.id]);
   setNotice('评论已删除。');
   // A reply that was being composed may have pointed at the deleted row.
   if (target !== undefined && target.id === comment.id) setTarget(undefined);
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setPostFailure(describeError(error));
  } finally {
   setDeletingId(undefined);
  }
 }

 /**
  * Insert a freshly uploaded image into the draft as its own paragraph.
  *
  * The same rule the topic editor follows, for the same reason: Markdown keeps
  * consecutive lines in one paragraph, so an image appended straight after a line of
  * text shares that paragraph's line box and the text ends up looking buried under
  * the picture. The image is also attached through `attachment_ids` on submit, so it
  * shows even if the author edits the text and removes the markup.
  */
 function insertUploadedImage(item: UploadItem) {
  const markup = `![${item.name}](${item.attachment.url})`;
  setDraft(current => (current.trim() === '' ? `${markup}\n` : `${current.replace(/\s+$/, '')}\n\n${markup}\n`));
 }

 async function handleSubmit() {
  if (posting || draft.trim() === '') return;
  setPosting(true);
  setPostFailure(undefined);
  setNotice(undefined);
  try {
   const created = await createComment({
    topic_id: topicId,
    ...(answerId === undefined ? {} : { answer_id: answerId }),
    ...(target === undefined ? {} : { reply_to_comment_id: target.id }),
    body_md: draft,
    is_anonymous: forceAnonymous === true || anonymous,
    // The images already uploaded for this comment. They are associated in the same transaction as the
    // comment, which is also what turns them from "pending" into attached content.
    ...(images.length === 0 ? {} : { attachment_ids: images.map(item => item.attachment.id) }),
   });
   // Show the new comment through the server's own answer rather than guessing where it lands: the
   // server decides the display root, so a reply joins its thread and a top-level comment joins the list.
   const rootId = created.parent_comment_id;
   if (rootId === undefined) {
    topList.addItem(created);
   } else {
    setThreads(current => {
     const existing = current[rootId] ?? { replies: [], hasMore: false, loading: false };
     return { ...current, [rootId]: { ...existing, replies: [...existing.replies, created] } };
    });
   }
   setDraft('');
   setImages([]);
   setNotice(target === undefined ? '评论已发布。' : '回复已发布。');
   setTarget(undefined);
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setPostFailure(describeError(error));
  } finally {
   setPosting(false);
  }
 }

 const visibleTops = tops.filter(top => !deletedIds.includes(top.id));
 const replies = Object.values(threads)
  .flatMap(thread => thread.replies)
  .filter(reply => !deletedIds.includes(reply.id) && !deletedIds.includes(reply.parent_comment_id ?? ''));
 const loading = topList.loading;
 const failure = loadFailure ?? topList.failure;
 const comments = [...visibleTops, ...replies];

 return <section aria-label={answerId === undefined ? '主题评论' : '回答的评论'}>
  <h3>{answerId === undefined ? '评论' : '回答的评论'}</h3>
  {loading && <Status>正在加载评论…</Status>}
  {!loading && comments.length === 0 && !failure && <p className="hint">{emptyText}</p>}
  {comments.length > 0 && <CommentList
   comments={comments}
   canComment={canComment}
   onReply={setTarget}
   actionsFor={comment => contentActions(comment, viewer, { topicIsLocked: topicIsLocked === true })}
   deletingId={deletingId}
   onDelete={comment => void handleDelete(comment)}
   threadMore={rootId => {
    const thread = threads[rootId];
    if (thread === undefined || !thread.hasMore) return undefined;
    return { loading: thread.loading, onLoadMore: () => void loadMoreReplies(rootId) };
   }}
   signedIn={signedIn === true}
   onUnauthenticated={onUnauthenticated}
  />}
  <Alert failure={failure} />
  {failure && <div className="actions">
   <button type="button" className="secondary" onClick={() => topList.reload()}>重新加载</button>
  </div>}
  {topList.hasMore && <div className="actions">
   <button
    type="button"
    className="secondary"
    disabled={topList.loadingMore}
    onClick={() => void topList.loadMore()}
   >
    {topList.loadingMore ? '正在加载…' : '加载更多评论'}
   </button>
  </div>}

  {canComment
   ? <div className="comment-composer">
    <ImageUploader
     images={images}
     onChange={setImages}
     disabled={posting}
     onUploaded={insertUploadedImage}
    />
    <CommentComposer
    label={target ? '回复' : '发表评论'}
    target={target}
    value={draft}
    onChange={setDraft}
    onSubmit={() => void handleSubmit()}
    onCancel={target ? () => setTarget(undefined) : undefined}
    pending={posting}
    failure={postFailure}
    forceAnonymous={forceAnonymous === true}
    anonymous={anonymous}
    onAnonymousChange={setAnonymous}
    submitLabel={target ? '发布回复' : '发布评论'}
    />
   </div>
   : <p className="hint">{closed
    ? '主题已锁定或问题已解决，暂时不能新增评论或回复。'
    : signedIn ? '当前账号暂时不能发表评论。' : '登录后可以参与讨论。'}</p>}
  <Status>{notice}</Status>
 </section>;
}
