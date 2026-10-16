import type { components } from '../api/generated';
import { Alert } from './Alert';
import { ReportButton } from './ReportButton';
import { authorLabel, replyTargetLabel } from './ContentMeta';
import { Markdown } from './Markdown';
import { unreferencedAttachments } from '../lib/attachments';
import { formatDateTime } from '../lib/format';
import type { Failure } from '../lib/messages';

export type Comment = components['schemas']['Comment'];

/** A top-level comment with the replies that hang off it (two-level display). */
export type CommentNode = {
 comment: Comment;
 replies: Comment[];
};

/**
 * Group a flat comment page into the two levels the interface shows.
 *
 * The API returns the real reply tree flat, in display order. A reply names its
 * display root in `parent_comment_id` — that is what makes two levels enough
 * even when the real tree is deeper. A reply whose root is missing from the
 * loaded page (or whose root is itself a top-level comment with no children) is
 * kept rather than dropped, so a reader never silently loses a comment.
 */
export function buildCommentTree(comments: Comment[]): CommentNode[] {
 const nodes = new Map<string, CommentNode>();
 const order: CommentNode[] = [];
 for (const comment of comments) {
  const parentId = comment.parent_comment_id;
  if (parentId === undefined) {
   const node: CommentNode = { comment, replies: [] };
   nodes.set(comment.id, node);
   order.push(node);
  }
 }
 for (const comment of comments) {
  const parentId = comment.parent_comment_id;
  if (parentId === undefined) continue;
  const parent = nodes.get(parentId);
  if (parent) {
   parent.replies.push(comment);
   continue;
  }
  // The display root is not in this page: show the reply as its own thread
  // instead of hiding it, and keep the 回复 marker from its own fields.
  if (nodes.has(comment.id)) continue;
  const orphan: CommentNode = { comment, replies: [] };
  nodes.set(comment.id, orphan);
  order.push(orphan);
 }
 return order;
}

/** Author lookup for the 回复 @xxx marker, by comment id within the loaded page. */
function authorById(comments: Comment[]): Map<string, Comment['author']> {
 const authors = new Map<string, Comment['author']>();
 for (const comment of comments) authors.set(comment.id, comment.author);
 return authors;
}

type CommentThreadProps = {
 node: CommentNode;
 authors: Map<string, Comment['author']>;
 canComment: boolean;
 onReply: (target: Comment) => void;
 /** Edit and delete entries for one comment, derived from what the page already knows. */
 actionsFor: (comment: Comment) => { canEdit: boolean; canDelete: boolean };
 deletingId?: string;
 onDelete: (comment: Comment) => void;
 /** Present only while this thread has another page of replies to fetch. */
 more?: { loading: boolean; onLoadMore: () => void };
 /** 举报入口对任何登录读者开放；未登录时会先走登录流程。 */
 signedIn: boolean;
 onUnauthenticated: () => void;
};

/** The edit link and delete button shown for a comment the viewer wrote. */
function CommentActions({ comment, canComment, actions, deleting, onReply, onDelete, signedIn, onUnauthenticated }: {
 comment: Comment;
 canComment: boolean;
 actions: { canEdit: boolean; canDelete: boolean };
 deleting: boolean;
 onReply: (target: Comment) => void;
 onDelete: (comment: Comment) => void;
 signedIn: boolean;
 onUnauthenticated: () => void;
}) {
 return <div className="comment-actions">
  {canComment && <button type="button" className="link" onClick={() => onReply(comment)}>回复</button>}
  {actions.canEdit && <a href={`#/edit/${encodeURIComponent(comment.id)}`}>编辑</a>}
  {actions.canDelete && <button type="button" className="link" disabled={deleting} onClick={() => onDelete(comment)}>
   {deleting ? '正在删除…' : '删除'}
  </button>}
  {/* 举报对任何登录读者开放，与"是不是我写的"无关；未登录时会先走登录流程。 */}
  <ReportButton
   contentId={comment.id}
   sessionReady={signedIn}
   signedIn={signedIn}
   onUnauthenticated={onUnauthenticated}
  />
 </div>;
}

export function CommentThread({ node, authors, canComment, onReply, actionsFor, deletingId, onDelete, more, signedIn, onUnauthenticated }: CommentThreadProps) {
 return <li className="comment">
  <CommentBody comment={node.comment} />
  <CommentActions
   comment={node.comment}
   canComment={canComment}
   actions={actionsFor(node.comment)}
   deleting={deletingId === node.comment.id}
   onReply={onReply}
   onDelete={onDelete}
   signedIn={signedIn}
   onUnauthenticated={onUnauthenticated}
  />
  {node.replies.length > 0 && <ul className="replies">
   {node.replies.map(reply => <li className="comment reply" key={reply.id}>
    <ReplyTarget comment={reply} authors={authors} />
    <CommentBody comment={reply} />
    <CommentActions
     comment={reply}
     canComment={canComment}
     actions={actionsFor(reply)}
     deleting={deletingId === reply.id}
     onReply={onReply}
     onDelete={onDelete}
     signedIn={signedIn}
     onUnauthenticated={onUnauthenticated}
    />
   </li>)}
  </ul>}
  {more && <div className="comment-actions">
   <button type="button" className="link" disabled={more.loading} onClick={more.onLoadMore}>
    {more.loading ? '正在加载…' : '加载更多回复'}
   </button>
  </div>}
 </li>;
}

/**
 * The reply marker: `reply_to_comment_id` is the real target, so the label uses
 * that id's author when the target is in the loaded page.
 */
function ReplyTarget({ comment, authors }: { comment: Comment; authors: Map<string, Comment['author']> }) {
 const targetId = comment.reply_to_comment_id;
 if (targetId === undefined) return null;
 return <>{replyTargetLabel(authors.get(targetId), targetId)}</>;
}

function CommentBody({ comment }: { comment: Comment }) {
 const createdAt = formatDateTime(comment.created_at);
 const editedAt = formatDateTime(comment.edited_at);
 // An image the body already embeds is not repeated in the strip below it.
 const rest = unreferencedAttachments(comment.body_md, comment.attachments);
 return <>
  <p className="byline">
   {authorLabel(comment.author)}
   {createdAt && <span className="time">{createdAt}</span>}
   {editedAt && <span className="hint">已编辑 {editedAt}</span>}
  </p>
  <Markdown source={comment.body_md} />
  {rest.length > 0 && <ul className="attachments-inline">
   {rest.map(attachment => <li key={attachment.id}>
    <img src={attachment.url} alt="评论附带的图片" loading="lazy" />
   </li>)}
  </ul>}
 </>;
}

type CommentListProps = {
 comments: Comment[];
 canComment: boolean;
 onReply: (target: Comment) => void;
 actionsFor: (comment: Comment) => { canEdit: boolean; canDelete: boolean };
 deletingId?: string;
 onDelete: (comment: Comment) => void;
 /** Replies of a thread root page on their own cursor, one root at a time. */
 threadMore?: (rootId: string) => { loading: boolean; onLoadMore: () => void } | undefined;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

/** Top-level comments plus their replies, in the order the API returned them. */
export function CommentList({ comments, canComment, onReply, actionsFor, deletingId, onDelete, threadMore, signedIn, onUnauthenticated }: CommentListProps) {
 const authors = authorById(comments);
 return <ul className="comments">
  {buildCommentTree(comments).map(node => <CommentThread
   key={node.comment.id}
   node={node}
   authors={authors}
   canComment={canComment}
   onReply={onReply}
   actionsFor={actionsFor}
   deletingId={deletingId}
   onDelete={onDelete}
   more={threadMore?.(node.comment.id)}
   signedIn={signedIn}
   onUnauthenticated={onUnauthenticated}
  />)}
 </ul>;
}

type CommentComposerProps = {
 label: string;
 /** 回复对象：undefined 表示新开一条顶层评论。 */
 target?: Comment;
 value: string;
 onChange: (value: string) => void;
 onSubmit: () => void;
 onCancel?: () => void;
 pending: boolean;
 failure?: Failure;
 /**
  * 当前用户在这个主题下是否被服务端强制匿名（匿名楼主的自答、评论与回复）。
  * 为 true 时开关固定选中且不可关闭：服务端同样强制执行，前端只是如实展示。
  */
 forceAnonymous: boolean;
 /** 本条评论/回复是否以匿名身份发布。由调用方持有，回复与评论共用同一选择。 */
 anonymous: boolean;
 onAnonymousChange: (anonymous: boolean) => void;
 submitLabel?: string;
};

/**
 * The one-shot comment/reply box. The topic and answer id live in the caller.
 *
 * 身份按条选择（requirements.md:19）：每条评论和回复都可以单独决定是否匿名，
 * 因此开关与正文一起提交，而不是在主题级设置一次。匿名楼主的开关被锁定，
 * 这与服务端的强制匿名一致，用户无法通过界面绕过。
 */
export function CommentComposer({ label, target, value, onChange, onSubmit, onCancel, pending, failure, forceAnonymous, anonymous, onAnonymousChange, submitLabel = '发布' }: CommentComposerProps) {
 const anonymousChecked = forceAnonymous || anonymous;
 return <form className="composer" onSubmit={event => { event.preventDefault(); onSubmit(); }}>
  <div className="field">
   <label htmlFor="comment-body">{label}</label>
   <textarea
    id="comment-body"
    value={value}
    disabled={pending}
    onChange={event => onChange(event.target.value)}
   />
   {target && <p className="hint">正在回复 {authorLabel(target.author)}，发布后会显示在对应评论的回复里。</p>}
   <p className="hint">支持 Markdown：原始 HTML 会被转义，链接只允许 http、https 与 mailto。站外链接会先经过提示页。</p>
  </div>
  <div className="field">
   <span className="label">身份</span>
   <label className="checkbox">
    <input
     type="checkbox"
     checked={anonymousChecked}
     disabled={pending || forceAnonymous}
     onChange={event => onAnonymousChange(event.target.checked)}
    />
    匿名发表这条{target ? '回复' : '评论'}（发布后不可切换）
   </label>
   {forceAnonymous && <p className="hint">你在这个主题下被强制匿名，展示为楼主，无法取消。</p>}
  </div>
  <Alert failure={failure} />
  <div className="actions">
   <button type="submit" disabled={pending || value.trim() === ''}>{pending ? '正在发布…' : submitLabel}</button>
   {onCancel && <button type="button" className="secondary" disabled={pending} onClick={onCancel}>取消</button>}
  </div>
 </form>;
}
