import { useEffect, useState } from 'react';
import {
 deleteContent,
 getContent,
 getContentPermissions,
 isDeleted,
 setResolution,
 type ContentPermissions,
 type DeletionResult,
 type QuestionTopic,
 type Topic,
} from '../api/content';
import type { CurrentUser } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { authorLabel, TagList, TopicStatus } from '../components/ContentMeta';
import { AttachmentGallery } from '../components/AttachmentGallery';
import { Markdown } from '../components/Markdown';
import { AnswersSection } from './AnswersSection';
import { CommentsSection } from './CommentsSection';
import { formatDateTime } from '../lib/format';
import { describeError, isStatus, type Failure } from '../lib/messages';
import type { Viewer } from '../lib/ownership';

type TopicDetailViewProps = {
 topicId: string;
 user?: CurrentUser;
 sessionReady: boolean;
 onUnauthenticated: () => void;
};

/** The locked-ness a reader sees, plus the 已解决 rule that also stops writes. */
function writesBlocked(topic: Topic): boolean {
 return topic.is_locked || (topic.kind === 'question' && topic.resolution_status === 'solved');
}

export function TopicDetailView({ topicId, user, sessionReady, onUnauthenticated }: TopicDetailViewProps) {
 const [topic, setTopic] = useState<Topic>();
 const [loading, setLoading] = useState(true);
 const [loadFailure, setLoadFailure] = useState<Failure>();
 const [permissions, setPermissions] = useState<ContentPermissions>();
 const [deleted, setDeleted] = useState<string>();
 const [busy, setBusy] = useState(false);
 const [actionFailure, setActionFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 // Bumped after a write so the permission-derived actions are re-read: solving
 // a question stops answers and comments for everyone, including the asker.
 const [permissionsKey, setPermissionsKey] = useState(0);

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  setLoadFailure(undefined);
  setTopic(undefined);
  setPermissions(undefined);
  setDeleted(undefined);
  getContent(topicId, controller.signal).then(content => {
   if (controller.signal.aborted) return;
   if (isDeleted(content)) {
    setDeleted(content.message);
    return;
   }
   if (content.kind !== 'discussion' && content.kind !== 'question') {
    setLoadFailure({ text: '该地址不是主题，请从列表进入。' });
    return;
   }
   setTopic(content);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setLoadFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [topicId]);

 // Permissions are per session and never cached: a guest gets no write entry,
 // and a logged-in reader sees exactly what the server says they may do.
 useEffect(() => {
  if (!user) {
   setPermissions(undefined);
   return;
  }
  const controller = new AbortController();
  getContentPermissions(topicId, controller.signal).then(current => {
   if (!controller.signal.aborted) setPermissions(current);
  }).catch(() => {
   // A permission probe must never block reading the topic: without it the
   // page simply offers no write actions.
   if (!controller.signal.aborted) setPermissions(undefined);
  });
  return () => controller.abort();
 }, [topicId, user, permissionsKey]);

 async function handleDelete() {
  if (busy || !topic) return;
  const scope = topic.kind === 'question'
   ? '这会同时删除该问题下的全部回答及其评论'
   : '这会同时删除该主题下的全部评论及回复';
  if (!window.confirm(`确定删除《${topic.title}》吗？${scope}，删除后不可恢复。`)) return;
  setBusy(true);
  setActionFailure(undefined);
  try {
   const result: DeletionResult = await deleteContent(topic.id);
   setNotice(`已删除，连带删除 ${result.affected_count} 条内容。`);
   // The topic no longer exists, so leave the detail page. Landing on the board it came from is more
   // useful than dropping the reader on the home screen, where they would have to find the list again.
   window.location.hash = topic.board_id ? `#/board/${encodeURIComponent(topic.board_id)}` : '#/topics';
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setActionFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 async function handleResolution(next: QuestionTopic['resolution_status']) {
  if (busy || !topic || topic.kind !== 'question') return;
  // 只有提问者可以切换，管理员也不例外：界面不为管理员破例。
  if (!permissions?.can_set_resolution) return;
  setBusy(true);
  setActionFailure(undefined);
  setNotice(undefined);
  try {
   const updated = await setResolution(topic.id, { resolution_status: next });
   setTopic(updated);
   // 解决状态决定 can_create_answer / can_create_comment，所以重新读取权限。
   setPermissionsKey(key => key + 1);
   setNotice(next === 'solved' ? '问题已标记为解决，新增回答、评论和回复已停止。' : '问题已重新打开。');
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setActionFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 if (deleted) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>内容不可用</h1>
   <Status>{deleted}</Status>
   <nav className="links" aria-label="返回"><a href="#/">返回主题列表</a></nav>
  </main>;
 }

 if (loading) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>主题</h1>
   <Status>正在加载主题…</Status>
  </main>;
 }

 if (!topic) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>主题</h1>
   <Alert failure={loadFailure ?? { text: '主题加载失败，请稍后重试。' }} />
   <nav className="links" aria-label="返回"><a href="#/">返回主题列表</a></nav>
  </main>;
 }

 const createdAt = formatDateTime(topic.created_at);
 const editedAt = formatDateTime(topic.edited_at);
 const resolvedAt = topic.kind === 'question' ? formatDateTime(topic.resolved_at) : undefined;
 const blocked = writesBlocked(topic);
 // What the page knows about the viewer, for deriving which answers and comments
 // are theirs (lib/ownership.ts). The anonymous-owner flag only comes from the
 // permissions response, so it is false until it has been read.
 const viewer: Viewer = {
  publicId: user?.profile.public_id,
  isAnonymousTopicOwner: permissions?.force_anonymous_comment === true,
 };

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <nav className="links" aria-label="面包屑">
   <a href="#/">主题列表</a>
   <a href={`#/board/${encodeURIComponent(topic.board_id)}`}>板块 #{topic.board_id}</a>
  </nav>
  <h1>{topic.title}</h1>
  <p className="byline">
   {authorLabel(topic.author)}
   <TopicStatus topic={topic} />
   {createdAt && <span className="time">发布于 {createdAt}</span>}
   {editedAt && <span className="hint">已编辑 {editedAt}</span>}
   {resolvedAt && <span className="time">解决于 {resolvedAt}</span>}
  </p>
  <TagList tags={topic.tags} />

  <article className="topic-body">
   <Markdown source={topic.body_md} />
   <AttachmentGallery attachments={topic.attachments} />
  </article>

  <div className="actions">
   {permissions?.can_edit && <a href={`#/edit/${encodeURIComponent(topic.id)}`}>编辑</a>}
   {permissions?.can_delete && <button type="button" className="secondary" disabled={busy} onClick={() => void handleDelete()}>
    {busy ? '正在删除…' : '删除'}
   </button>}
   {topic.kind === 'question' && permissions?.can_set_resolution && (topic.resolution_status === 'solved'
    ? <button type="button" className="secondary" disabled={busy} onClick={() => void handleResolution('open')}>重新打开问题</button>
    : <button type="button" disabled={busy} onClick={() => void handleResolution('solved')}>标记为已解决</button>)}
   {!user && <button
    type="button"
    className="secondary"
    disabled={!sessionReady}
    onClick={onUnauthenticated}
   >
    登录后参与讨论
   </button>}
  </div>
  {sessionReady && user && !permissions && <p className="hint">无法读取当前账号对该内容的权限，因此只显示浏览入口。</p>}
  <Alert failure={actionFailure} />
  <Status>{notice}</Status>

  {topic.kind === 'question'
   ? <AnswersSection
    topicId={topic.id}
    permissions={permissions}
    isLocked={blocked}
    topicIsLocked={topic.is_locked}
    signedIn={user !== undefined}
    viewer={viewer}
    onUnauthenticated={onUnauthenticated}
   />
   : <CommentsSection
    topicId={topic.id}
    canComment={permissions?.can_create_comment === true && !blocked}
    closed={blocked}
    topicIsLocked={topic.is_locked}
    signedIn={user !== undefined}
    viewer={viewer}
    forceAnonymous={permissions?.force_anonymous_comment}
    onUnauthenticated={onUnauthenticated}
    emptyText="这条主题还没有评论。"
   />}
 </main>;
}
