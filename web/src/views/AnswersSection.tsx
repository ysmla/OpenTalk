import { useEffect, useState, type FormEvent } from 'react';
import { createAnswer, deleteContent, listAnswers, type Answer, type ContentPermissions } from '../api/content';
import { Alert, Status } from '../components/Alert';
import { authorLabel } from '../components/ContentMeta';
import { AttachmentGallery } from '../components/AttachmentGallery';
import { ImageUploader, type UploadItem } from '../components/ImageUploader';
import { Markdown } from '../components/Markdown';
import { CommentsSection } from './CommentsSection';
import { formatDateTime } from '../lib/format';
import { describeError, isStatus, type Failure } from '../lib/messages';
import { contentActions, type Viewer } from '../lib/ownership';
import { bodyProblem, BODY_MAX, charCount } from '../lib/validate';

type AnswersSectionProps = {
 topicId: string;
 permissions?: ContentPermissions;
 /** 主题被锁定或问题已解决时，任何人（包括管理员）都不能新增互动。 */
 isLocked: boolean;
 /**
  * 主题是否被锁定。与 `isLocked` 不同：已解决只禁止新增互动，**不**禁止编辑正文，
  * 所以编辑入口要看这个字段。
  */
 topicIsLocked: boolean;
 /** 当前是否有会话，用于把"登录后可以参与"与"账号受限"分开。 */
 signedIn: boolean;
 /** 用于推导"这条内容是不是我的"（见 lib/ownership.ts）。 */
 viewer: Viewer;
 onUnauthenticated: () => void;
};

/**
 * 问答主题下的回答列表。The contract returns answers as a cursor page, so the
 * section loads every page up front — answer counts are small, and showing a
 * partial answer list with its own 加载更多 would hide the discussion the
 * question exists for.
 */
export function AnswersSection({ topicId, permissions, isLocked, topicIsLocked, signedIn, viewer, onUnauthenticated }: AnswersSectionProps) {
 const [answers, setAnswers] = useState<Answer[]>([]);
 const [loading, setLoading] = useState(true);
 const [loadFailure, setLoadFailure] = useState<Failure>();
 const [reloadKey, setReloadKey] = useState(0);
 const [draft, setDraft] = useState('');
 const [images, setImages] = useState<UploadItem[]>([]);
 const [anonymous, setAnonymous] = useState(false);
 const [posting, setPosting] = useState(false);
 const [postFailure, setPostFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [deletingId, setDeletingId] = useState<string>();

 /**
  * Delete one answer. The server decides whether it may go — the entry is only
  * shown for content the page derived as the viewer's own — and deleting an
  * answer takes its comments with it, which the confirmation says out loud.
  */
 async function handleDeleteAnswer(answer: Answer) {
  // The payload carries no comment count, so the wording covers both cases rather
  // than claiming a cascade that may have nothing to cascade to.
  if (!window.confirm('确定删除这条回答吗？它下面的评论及回复会一并删除，且不可恢复。')) return;
  setDeletingId(answer.id);
  setPostFailure(undefined);
  setNotice(undefined);
  try {
   await deleteContent(answer.id);
   setAnswers(current => current.filter(candidate => candidate.id !== answer.id));
   setNotice('回答已删除。');
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

 useEffect(() => {
  const controller = new AbortController();
  setLoading(true);
  setLoadFailure(undefined);
  // Follow the cursor to the end; a failure keeps what was already loaded.
  async function load() {
   const collected: Answer[] = [];
   let cursor: string | undefined;
   do {
    const page = await listAnswers(topicId, cursor, controller.signal);
    if (controller.signal.aborted) return;
    collected.push(...page.items.filter((item): item is Answer => item.kind === 'answer'));
    cursor = page.has_more ? page.next_cursor : undefined;
   } while (cursor !== undefined);
   setAnswers(collected);
  }
  load().catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setLoadFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [topicId, reloadKey]);

 const canAnswer = permissions?.can_create_answer === true && !isLocked;
 const forceAnonymous = permissions?.force_anonymous_comment === true;

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (posting) return;
  const problem = bodyProblem(draft);
  if (problem) {
   setPostFailure({ text: problem });
   return;
  }
  setPosting(true);
  setPostFailure(undefined);
  setNotice(undefined);
  try {
   const created = await createAnswer({
    topic_id: topicId,
    body_md: draft,
    attachment_ids: images.map(item => item.attachment.id),
    is_anonymous: forceAnonymous || anonymous,
   });
   setAnswers(current => [...current, created]);
   setDraft('');
   setImages([]);
   setAnonymous(false);
   setNotice('回答已发布。');
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

 return <section aria-label="回答">
  <h2>回答</h2>
  {loading && <Status>正在加载回答…</Status>}
  {!loading && answers.length === 0 && !loadFailure && <p className="hint">还没有回答。</p>}
  {loadFailure && <div className="actions">
   <button type="button" className="secondary" onClick={() => setReloadKey(key => key + 1)}>重新加载</button>
  </div>}
  <Alert failure={loadFailure} />

  {answers.map(answer => {
   const actions = contentActions(answer, viewer, { topicIsLocked });
   return <article className="answer" key={answer.id}>
    <AnswerBody answer={answer} />
    <div className="actions">
     {actions.canEdit && <a href={`#/edit/${encodeURIComponent(answer.id)}`}>编辑</a>}
     {actions.canDelete && <button
      type="button"
      className="secondary"
      disabled={deletingId === answer.id}
      onClick={() => void handleDeleteAnswer(answer)}
     >
      {deletingId === answer.id ? '正在删除…' : '删除'}
     </button>}
    </div>
    <CommentsSection
     topicId={topicId}
     answerId={answer.id}
     canComment={permissions?.can_create_comment === true && !isLocked}
     closed={isLocked}
     topicIsLocked={topicIsLocked}
     signedIn={signedIn}
     viewer={viewer}
     forceAnonymous={forceAnonymous}
     onUnauthenticated={onUnauthenticated}
     emptyText="这条回答还没有评论。"
    />
   </article>;
  })}

  {canAnswer
   ? <form className="composer" onSubmit={handleSubmit} noValidate>
    <h3>写下你的回答</h3>
    <div className="field">
     <label htmlFor="answer-body">回答正文（Markdown）</label>
     <textarea
      id="answer-body"
      value={draft}
      disabled={posting}
      onChange={event => setDraft(event.target.value)}
     />
     <p className="hint">最多 {BODY_MAX} 个字符（当前 {charCount(draft)}）。</p>
    </div>
    <ImageUploader
     images={images}
     onChange={setImages}
     disabled={posting}
    />
    <div className="actions">
     {(forceAnonymous || anonymous) && <span className="hint" role="status">将以匿名身份发布。</span>}
     {!forceAnonymous && <label className="checkbox">
      <input type="checkbox" checked={anonymous} disabled={posting} onChange={event => setAnonymous(event.target.checked)} />
      匿名回答
     </label>}
    </div>
    <Alert failure={postFailure} />
    <div className="actions">
     <button type="submit" disabled={posting || draft.trim() === ''}>{posting ? '正在发布…' : '发布回答'}</button>
    </div>
    <Status>{notice}</Status>
   </form>
   : <p className="hint">
    {isLocked ? '主题已锁定或问题已解决，暂时不能新增回答。' : '当前账号暂时不能新增回答。'}
   </p>}
 </section>;
}

function AnswerBody({ answer }: { answer: Answer }) {
 const createdAt = formatDateTime(answer.created_at);
 const editedAt = formatDateTime(answer.edited_at);
 return <>
  <p className="byline">
   {authorLabel(answer.author)}
   {createdAt && <span className="time">{createdAt}</span>}
   {editedAt && <span className="hint">已编辑 {editedAt}</span>}
  </p>
  <Markdown source={answer.body_md} />
  <AttachmentGallery attachments={answer.attachments} bodyMd={answer.body_md} />
 </>;
}
