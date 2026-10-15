import { useEffect, useRef, useState, type FormEvent } from 'react';
import {
 createTopic,
 editContent,
 getContent,
 getContentPermissions,
 isDeleted,
 isQuestion,
 isTopic,
 listAllBoards,
 listAllTags,
 type Board,
 type ContentPermissions,
 type PublicContent,
 type Tag,
} from '../api/content';
import type { CurrentUser } from '../api/auth';
import { Alert, Status } from '../components/Alert';
import { ImageUploader, type UploadItem } from '../components/ImageUploader';
import { Markdown } from '../components/Markdown';
import { describeError, isStatus, type Failure } from '../lib/messages';
import { bodyProblem, charCount, tagCountProblem, titleProblem, BODY_MAX, TAGS_MAX, TITLE_MAX } from '../lib/validate';

export type EditorSaveResult =
 | { mode: 'created'; content: PublicContent }
 | { mode: 'edited'; content: PublicContent };

type TopicEditorViewProps = {
 /** Absent when publishing a new topic. */
 contentId?: string;
 /** `board_id` from `#/new?board_id=…`. */
 initialBoardId?: string;
 user?: CurrentUser;
 sessionReady: boolean;
 /** Called after a successful save, with the content the server returned. */
 onSaved: (result: EditorSaveResult) => void;
 onUnauthenticated: () => void;
};

/**
 * One editor for 发帖 and 提问 — the board's `kind` decides which one the server
 * creates — and for editing an existing topic or answer. Creating and editing
 * share the Markdown body, the live preview and the attachment list; editing
 * additionally sends `attachment_ids` with the contract's "omit to keep, empty
 * array to detach everything" meaning.
 */
export function TopicEditorView({ contentId, initialBoardId, user, sessionReady, onSaved, onUnauthenticated }: TopicEditorViewProps) {
 const [boards, setBoards] = useState<Board[]>([]);
 const [tags, setTags] = useState<Tag[]>([]);
 const [loading, setLoading] = useState(true);
 const [loadFailure, setLoadFailure] = useState<Failure>();
 // Set when editing: the server's own view of the content being edited.
 const [loaded, setLoaded] = useState<PublicContent>();
 const [permissions, setPermissions] = useState<ContentPermissions>();
 const [denied, setDenied] = useState(false);

 const [boardId, setBoardId] = useState('');
 const [title, setTitle] = useState('');
 const [body, setBody] = useState('');
 const [initialBody, setInitialBody] = useState('');
 const [tagIds, setTagIds] = useState<string[]>([]);
 const [anonymous, setAnonymous] = useState(false);
 // Create: the associated set. Edit: the attachments still kept.
 const [images, setImages] = useState<UploadItem[]>([]);
 const [initialImageIds, setInitialImageIds] = useState<string[]>([]);
 const [touchedImages, setTouchedImages] = useState(false);

 const [saving, setSaving] = useState(false);
 const [saveFailure, setSaveFailure] = useState<Failure>();
 const bodyRef = useRef<HTMLTextAreaElement>(null);

 const editing = contentId !== undefined;
 const editingTopic = loaded !== undefined && isTopic(loaded);

 useEffect(() => {
  // Nothing to load before the session is known: the content and its
  // permissions are per session, and an anonymous request would only 401.
  if (!sessionReady || !user) {
   setLoading(false);
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setLoadFailure(undefined);
  setDenied(false);
  async function load() {
   // The whole board and tag catalogue, not one page: the picker must not hide
   // a choice just because the list outgrew its first page.
   const [boardItems, tagItems] = await Promise.all([listAllBoards(controller.signal), listAllTags(controller.signal)]);
   if (controller.signal.aborted) return;
   setBoards(boardItems);
   setTags(tagItems);
   if (contentId === undefined) {
    const preselect = initialBoardId !== undefined && boardItems.some(board => board.id === initialBoardId)
     ? initialBoardId
     : boardItems.find(board => !board.is_archived)?.id ?? '';
    setBoardId(preselect);
    return;
   }
   const content = await getContent(contentId, controller.signal);
   if (controller.signal.aborted) return;
   if (isDeleted(content)) {
    // A deletion placeholder is not editable.
    setDenied(true);
    return;
   }
   setLoaded(content);
   if (!isTopic(content) && content.kind !== 'answer') {
    // A comment is not editable on a page of its own.
    setDenied(true);
    return;
   }
   setBody(content.body_md);
   setInitialBody(content.body_md);
   setImages(content.attachments.map(attachment => ({ attachment, name: `已有图片 #${attachment.id}` })));
   setInitialImageIds(content.attachments.map(attachment => attachment.id));
   if (isTopic(content)) {
    setBoardId(content.board_id);
    setTitle(content.title);
    setTagIds(content.tags.map(tag => tag.id));
   }
   const current = await getContentPermissions(contentId, controller.signal);
   if (controller.signal.aborted) return;
   setPermissions(current);
   if (!current.can_edit) {
    setDenied(true);
    return;
   }
   // 匿名提问者自答会被服务端强制匿名，所以开关按权限预置且不可关闭。
   if (current.force_anonymous_comment) setAnonymous(true);
  }
  load().catch((error: unknown) => {
   if (controller.signal.aborted) return;
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setLoadFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [contentId, initialBoardId, sessionReady, user, onUnauthenticated]);

 const board = boards.find(candidate => candidate.id === boardId);
 const isQuestionBoard = board?.kind === 'qa';
 const forceAnonymous = permissions?.force_anonymous_comment === true;
 const bodyIsTitle = !editing || editingTopic;

 /**
  * Insert an uploaded image at the caret, as its own paragraph.
  *
  * A blank line is required on both sides, and a single newline is not enough: Markdown keeps
  * consecutive lines inside one paragraph, so an image placed right after a line of text shares that
  * paragraph's line box. The image is taller than the text and sits on the baseline, so the text ends up
  * looking buried under the picture — which is exactly what the browser walkthrough showed when the upload
  * was appended straight after a link. The blank line is therefore added unless the boundary already is one
  * or is the edge of the document.
  */
 function insertImage(item: UploadItem) {
  const markup = `![${item.name}](${item.attachment.url})`;
  const textarea = bodyRef.current;
  const selectionStart = textarea?.selectionStart ?? body.length;
  const selectionEnd = textarea?.selectionEnd ?? body.length;
  const before = body.slice(0, selectionStart);
  const after = body.slice(selectionEnd);
  const prefix = before === '' || before.endsWith('\n\n') ? '' : before.endsWith('\n') ? '\n' : '\n\n';
  const suffix = after === '' || after.startsWith('\n\n') ? '' : after.startsWith('\n') ? '\n' : '\n\n';
  setBody(`${before}${prefix}${markup}${suffix}${after}`);
  setTouchedImages(true);
 }

 function toggleTag(id: string) {
  setTagIds(current => {
   if (current.includes(id)) return current.filter(candidate => candidate !== id);
   // 超过上限时不追加：服务端同样拒绝，这里给出即时反馈。
   if (current.length >= TAGS_MAX) return current;
   return [...current, id];
  });
 }

 /** The same checks the server runs, so an obvious mistake never leaves the page. */
 function validate(): string | undefined {
  if (!editing && boardId === '') return '请选择板块。';
  if (bodyIsTitle) {
   const titleIssue = titleProblem(title);
   if (titleIssue) return titleIssue;
  }
  const bodyIssue = bodyProblem(body);
  if (bodyIssue) return bodyIssue;
  if (!editing) {
   const tagIssue = tagCountProblem(tagIds.length);
   if (tagIssue) return tagIssue;
  }
  return undefined;
 }

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (saving) return;
  const problem = validate();
  if (problem) {
   setSaveFailure({ text: problem });
   return;
  }
  setSaving(true);
  setSaveFailure(undefined);
  try {
   if (editing && contentId !== undefined) {
    const currentIds = images.map(item => item.attachment.id);
    const attachmentsChanged = touchedImages
     || currentIds.length !== initialImageIds.length
     || currentIds.some(id => !initialImageIds.includes(id));
    const published = loaded;
    const titleChanged = published !== undefined && isTopic(published) && title !== published.title;
    if (!attachmentsChanged && !titleChanged && body === initialBody) {
     setSaveFailure({ text: '没有需要保存的修改。' });
     return;
    }
    const updated = await editContent(contentId, {
     body_md: body,
     // title 只对主题有意义，回答与评论不接受该字段。
     ...(editingTopic ? { title } : {}),
     // 省略表示不改附件；用户动过附件或清单确实变了才提交完整清单，
     // 空数组表示解除全部附件关联。
     ...(attachmentsChanged ? { attachment_ids: currentIds } : {}),
    });
    onSaved({ mode: 'edited', content: updated });
    return;
   }
   const created = await createTopic({
    board_id: boardId,
    title,
    body_md: body,
    attachment_ids: images.map(item => item.attachment.id),
    is_anonymous: forceAnonymous || anonymous,
    tag_ids: tagIds,
   });
   onSaved({ mode: 'created', content: created });
  } catch (error) {
   if (isStatus(error, 401)) {
    onUnauthenticated();
    return;
   }
   setSaveFailure(describeError(error));
  } finally {
   setSaving(false);
  }
 }

 if (editing && denied) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>编辑内容</h1>
   <Alert failure={{ text: '当前账号无法编辑该内容。' }} />
   <nav className="links" aria-label="返回">
    <a href={`#/topic/${encodeURIComponent(contentId)}`}>返回内容</a>
    <a href="#/">返回主题列表</a>
   </nav>
  </main>;
 }

 if (loadFailure) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>{editing ? '编辑内容' : '发布主题'}</h1>
   <Alert failure={loadFailure} />
   <nav className="links" aria-label="返回"><a href="#/">返回主题列表</a></nav>
  </main>;
 }

 if (!sessionReady) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>{editing ? '编辑内容' : '发布主题'}</h1>
   <Status>正在确认登录状态…</Status>
  </main>;
 }

 if (!user) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>{editing ? '编辑内容' : '发布主题'}</h1>
   <Status>需要登录后才能发布或编辑内容。</Status>
   <nav className="links" aria-label="账号入口">
    <a href="#/login">去登录</a>
    <a href="#/register">注册</a>
   </nav>
  </main>;
 }

 if (loading) {
  return <main className="page">
   <p className="eyebrow">OPENTALK</p>
   <h1>{editing ? '编辑内容' : '发布主题'}</h1>
   <Status>正在加载…</Status>
  </main>;
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>{editing ? '编辑内容' : isQuestionBoard ? '提问' : '发帖'}</h1>
  {!editing && <p>板块类型决定发布的是普通帖还是问题；发布后不能更改板块或匿名状态。</p>}
  {editing && <p>编辑只会修改正文{bodyIsTitle ? '与标题' : ''}以及附件清单；匿名状态、板块与回复关系不会被改动。</p>}

  <form onSubmit={handleSubmit} noValidate>
   {!editing && <div className="field">
    <label htmlFor="editor-board">板块</label>
    <select
     id="editor-board"
     value={boardId}
     disabled={saving}
     onChange={event => setBoardId(event.target.value)}
    >
     <option value="">请选择板块</option>
     {boards.map(candidate => <option key={candidate.id} value={candidate.id} disabled={candidate.is_archived}>
      {candidate.name}{candidate.kind === 'qa' ? '（问答）' : '（讨论）'}{candidate.is_archived ? '（已停用）' : ''}
     </option>)}
    </select>
    {board && <p className="hint">{board.description} 类型：{board.kind === 'qa' ? '问答' : '讨论'}。</p>}
   </div>}

   {editing && board && <p className="hint">板块：{board.name}（发布后不能更改）。</p>}

   {bodyIsTitle && <div className="field">
    <label htmlFor="editor-title">标题</label>
    <input
     id="editor-title"
     value={title}
     disabled={saving}
     aria-describedby="editor-title-hint"
     onChange={event => setTitle(event.target.value)}
    />
    <p className="hint" id="editor-title-hint">单行纯文本，最多 {TITLE_MAX} 个字符（当前 {charCount(title)}）。</p>
   </div>}

   <div className="field">
    <label htmlFor="editor-body">正文（Markdown）</label>
    <textarea
     id="editor-body"
     ref={bodyRef}
     value={body}
     disabled={saving}
     aria-describedby="editor-body-hint"
     onChange={event => setBody(event.target.value)}
    />
    <p className="hint" id="editor-body-hint">
     最多 {BODY_MAX} 个字符（当前 {charCount(body)}）。支持 Markdown：原始 HTML 会被转义，链接只允许 http、https 与 mailto。
    </p>
   </div>

   {!editing && <div className="field" role="group" aria-label={`标签（最多 ${TAGS_MAX} 个）`}>
    <span className="label">标签（最多 {TAGS_MAX} 个）</span>
    {tags.length === 0
     ? <p className="hint">暂时没有可选标签。</p>
     : <ul className="tag-picker">
      {tags.map(tag => <li key={tag.id}>
       <label className="checkbox" title={tag.description}>
        <input
         type="checkbox"
         checked={tagIds.includes(tag.id)}
         disabled={saving || (!tagIds.includes(tag.id) && tagIds.length >= TAGS_MAX)}
         onChange={() => toggleTag(tag.id)}
        />
        {tag.name}
       </label>
      </li>)}
     </ul>}
    <p className="hint">已选 {tagIds.length} 个{tagCountProblem(tagIds.length) ? `，${tagCountProblem(tagIds.length)}` : '。'}</p>
   </div>}

   <ImageUploader
    images={images}
    onChange={next => { setImages(next); setTouchedImages(true); }}
    disabled={saving}
    onUploaded={insertImage}
   />

   <div className="field">
    <span className="label">匿名</span>
    <label className="checkbox">
     <input
      type="checkbox"
      checked={forceAnonymous || anonymous}
      disabled={saving || forceAnonymous || editing}
      onChange={event => setAnonymous(event.target.checked)}
     />
     以匿名身份发布（发布后不可切换）
    </label>
    {forceAnonymous && <p className="hint">你在该主题下发言会被服务端强制匿名。</p>}
    {editing && <p className="hint">编辑不会改变原有的匿名状态。</p>}
   </div>

   <section aria-label="实时预览">
    <h2>实时预览</h2>
    <p className="hint">预览使用与服务端一致的链接白名单，渲染结果与发布后的展示相同。</p>
    {bodyIsTitle && title.trim() !== '' && <p className="preview-title">{title}</p>}
    {body.trim() === ''
     ? <p className="hint">还没有内容可预览。</p>
     : <div className="preview"><Markdown source={body} /></div>}
   </section>

   <Alert failure={saveFailure} />
   <div className="actions">
    <button type="submit" disabled={saving}>{saving ? '正在提交…' : editing ? '保存修改' : isQuestionBoard ? '发布问题' : '发布主题'}</button>
    {editing && contentId !== undefined && <a href={`#/topic/${encodeURIComponent(contentId)}`}>返回内容</a>}
    <a href="#/">返回主题列表</a>
   </div>
  </form>
 </main>;
}
