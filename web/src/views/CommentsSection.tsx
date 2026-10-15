import { useEffect, useState } from 'react';
import { createComment, deleteContent, listComments, type Comment } from '../api/content';
import { Alert, Status } from '../components/Alert';
import { CommentComposer, CommentList } from '../components/CommentThread';
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

/**
 * One comment list over one topic or one answer, with the composer beneath it.
 *
 * 两层展示: the page holds top-level comments (`parent_comment_id` omitted) and
 * renders their replies under them. A reply posts `reply_to_comment_id` — the
 * real target — and the server derives the display root, so the tree can be
 * deeper than the two levels the interface shows.
 */
export function CommentsSection({ topicId, answerId, canComment, closed, topicIsLocked, signedIn, viewer, forceAnonymous, onUnauthenticated, emptyText }: CommentsSectionProps) {
 const [comments, setComments] = useState<Comment[]>([]);
 const [loading, setLoading] = useState(true);
 const [loadFailure, setLoadFailure] = useState<Failure>();
 const [reloadKey, setReloadKey] = useState(0);
 const [draft, setDraft] = useState('');
 const [target, setTarget] = useState<Comment>();
 const [posting, setPosting] = useState(false);
 const [postFailure, setPostFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [deletingId, setDeletingId] = useState<string>();

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  setLoadFailure(undefined);
  /**
   * Load the two levels the section displays.
   *
   * The endpoint pages *one* level at a time by design: `parent_comment_id`
   * omitted returns the top-level comments, and one thread root's replies are
   * asked for separately. That is what keeps a page of a two-level display
   * consistent — a page mixing both levels could show a reply whose root sits on
   * another page. The cost is one extra request per top-level comment on this
   * page, which is why they are issued together rather than one after another.
   */
  async function load() {
   const page = await listComments(topicId, { answer_id: answerId }, controller.signal);
   if (controller.signal.aborted) return;
   const tops = page.items.filter((item): item is Comment => item.kind === 'comment');
   const threads = await Promise.all(tops.map(async top => {
    const replies = await listComments(topicId, { answer_id: answerId, parent_comment_id: top.id }, controller.signal);
    if (controller.signal.aborted) return [];
    return replies.items.filter((item): item is Comment => item.kind === 'comment');
   }));
   if (controller.signal.aborted) return;
   setComments([...tops, ...threads.flat()]);
  }
  load().catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setLoadFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [topicId, answerId, reloadKey]);

 /**
  * Delete one comment or reply. The server authorizes it again; the entry is only
  * rendered for content the page derived as the viewer's own. Deleting a
  * top-level comment removes the replies shown under it, because a reply's
  * `parent_comment_id` is that display root.
  */
 async function handleDelete(comment: Comment) {
  const scope = comment.parent_comment_id === undefined ? '它下面的回复会一并删除，' : '';
  if (!window.confirm(`确定删除这条${comment.parent_comment_id === undefined ? '评论' : '回复'}吗？${scope}删除后不可恢复。`)) return;
  setDeletingId(comment.id);
  setPostFailure(undefined);
  setNotice(undefined);
  try {
   await deleteContent(comment.id);
   setComments(current => current.filter(candidate =>
    candidate.id !== comment.id && candidate.parent_comment_id !== comment.id));
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
    is_anonymous: forceAnonymous === true,
   });
   // Show the new comment through the server's own answer rather than guessing
   // where it lands in the two-level display.
   setComments(current => [...current, created]);
   setDraft('');
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

 return <section aria-label={answerId === undefined ? '主题评论' : '回答的评论'}>
  <h3>{answerId === undefined ? '评论' : '回答的评论'}</h3>
  {loading && <Status>正在加载评论…</Status>}
  {!loading && comments.length === 0 && !loadFailure && <p className="hint">{emptyText}</p>}
  {comments.length > 0 && <CommentList
   comments={comments}
   canComment={canComment}
   onReply={setTarget}
   actionsFor={comment => contentActions(comment, viewer, { topicIsLocked: topicIsLocked === true })}
   deletingId={deletingId}
   onDelete={comment => void handleDelete(comment)}
  />}
  <Alert failure={loadFailure} />
  {loadFailure && <div className="actions">
   <button type="button" className="secondary" onClick={() => setReloadKey(key => key + 1)}>重新加载</button>
  </div>}

  {canComment
   ? <CommentComposer
    label={target ? '回复' : '发表评论'}
    target={target}
    value={draft}
    onChange={setDraft}
    onSubmit={() => void handleSubmit()}
    onCancel={target ? () => setTarget(undefined) : undefined}
    pending={posting}
    failure={postFailure}
    hint={forceAnonymous
     ? '你在此主题下发言会被强制匿名，展示为楼主或匿名编号。'
     : '支持 Markdown：原始 HTML 会被转义，链接只允许 http、https 与 mailto。'}
    submitLabel={target ? '发布回复' : '发布评论'}
   />
   : <p className="hint">{closed
    ? '主题已锁定或问题已解决，暂时不能新增评论或回复。'
    : signedIn ? '当前账号暂时不能发表评论。' : '登录后可以参与讨论。'}</p>}
  <Status>{notice}</Status>
 </section>;
}
