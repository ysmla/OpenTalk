import { useEffect, useState, type FormEvent } from 'react';
import {
 createBoard,
 createRestriction,
 createTag,
 listAdminUsers,
 listRestrictions,
 moderateTopic,
 readDeletedArchive,
 revokeRestriction,
 traceAnonymousAuthor,
 updateBoard,
 updateTag,
 type AdminRestriction,
 type AdminUser,
 type AdminDeletedArchive,
 type AdminAnonymousTrace,
} from '../api/adminOps';
import { listAllBoards, listAllTags } from '../api/content';
import { Alert, Status } from '../components/Alert';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type AdminOperationsViewProps = {
 sessionReady: boolean;
 signedIn: boolean;
 isAdmin: boolean;
 onUnauthenticated: () => void;
};

const KIND_LABEL: Record<string, string> = { mute: '禁言', ban: '封禁' };

/**
 * 管理端：账号处罚、站点结构（板块与标签）与"揭示类"操作（删除原文、匿名追溯）。
 *
 * 把这几件事放在一页，是因为它们都是管理员**对站点或对账号**动手的地方，而举报队列在
 * `#/admin/moderation`。界面提供输入框而不是下拉选择，理由很直接：处罚与追溯都以**内部用户 ID / 内容 ID**
 * 为参数，而这两类 ID 本来就不出现在公开响应里，界面无法"列出候选"。
 *
 * 每个动作都要求原因，因为服务端会把原因写进审计；界面不提供"无原因"的路径，否则那条路径必然失败。
 */
export function AdminOperationsView({ sessionReady, signedIn, isAdmin, onUnauthenticated }: AdminOperationsViewProps) {
 const [users, setUsers] = useState<AdminUser[]>([]);
 const [restrictions, setRestrictions] = useState<AdminRestriction[]>([]);
 const [boards, setBoards] = useState<{ id: string; name: string; kind: string }[]>([]);
 const [tags, setTags] = useState<{ id: string; name: string }[]>([]);
 const [loading, setLoading] = useState(true);
 const [busy, setBusy] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState('');
 const [archive, setArchive] = useState<AdminDeletedArchive>();
 const [trace, setTrace] = useState<AdminAnonymousTrace>();

 // Form state. Every field is a string because the API takes ids as strings and dates as RFC3339 text.
 const [restrictUser, setRestrictUser] = useState('');
 const [restrictKind, setRestrictKind] = useState<'mute' | 'ban'>('mute');
 const [restrictReason, setRestrictReason] = useState('');
 const [restrictEnds, setRestrictEnds] = useState('');
 const [boardName, setBoardName] = useState('');
 const [boardDescription, setBoardDescription] = useState('');
 const [boardKind, setBoardKind] = useState<'discussion' | 'qa'>('discussion');
 const [boardEditId, setBoardEditId] = useState('');
 const [tagName, setTagName] = useState('');
 const [tagDescription, setTagDescription] = useState('');
 const [tagEditId, setTagEditId] = useState('');
 const [topicId, setTopicId] = useState('');
 const [topicReason, setTopicReason] = useState('');
 const [topicLocked, setTopicLocked] = useState(false);
 const [topicPinned, setTopicPinned] = useState(false);
 const [topicBoard, setTopicBoard] = useState('');
 const [revealId, setRevealId] = useState('');
 const [revealReason, setRevealReason] = useState('');

 async function refresh() {
  setLoading(true);
  setFailure(undefined);
  try {
   const [userPage, restrictionPage, boardList, tagList] = await Promise.all([
    listAdminUsers(),
    listRestrictions(),
    listAllBoards(),
    listAllTags(),
   ]);
   setUsers(userPage.items);
   setRestrictions(restrictionPage.items);
   setBoards(boardList.map(board => ({ id: board.id, name: board.name, kind: board.kind })));
   setTags(tagList.map(tag => ({ id: tag.id, name: tag.name })));
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setLoading(false);
  }
 }

 useEffect(() => {
  if (!sessionReady) return;
  if (!signedIn || !isAdmin) {
   setLoading(false);
   if (!signedIn) onUnauthenticated();
   return;
  }
  void refresh();
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [sessionReady, signedIn, isAdmin]);

 if (sessionReady && signedIn && !isAdmin) {
  return <main className="page"><h1>管理</h1><p className="hint">当前账号不是管理员。</p></main>;
 }

 /** Runs one administrative call, reporting the outcome the same way for every section. */
 async function run(success: string, action: () => Promise<unknown>) {
  if (busy) return;
  setBusy(true);
  setFailure(undefined);
  setNotice('');
  try {
   await action();
   setNotice(success);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 function submitRestriction(event: FormEvent) {
  event.preventDefault();
  void run('处罚已记录。封禁会立即撤销该账号的全部会话。', async () => {
   await createRestriction({
    user_id: restrictUser.trim(),
    kind: restrictKind,
    reason: restrictReason.trim(),
    // An empty field means permanent: omitted rather than sent as "".
    ...(restrictEnds.trim() === '' ? {} : { ends_at: new Date(restrictEnds).toISOString() }),
   });
   setRestrictReason('');
   setRestrictEnds('');
   await refresh();
  });
 }

 function submitBoard(event: FormEvent, mode: 'create' | 'update') {
  event.preventDefault();
  const body = {
   name: boardName.trim(),
   description: boardDescription.trim(),
   kind: boardKind,
  };
  void run(mode === 'create' ? `板块「${body.name}」已创建。` : '板块已更新。', async () => {
   if (mode === 'create') await createBoard(body);
   else await updateBoard(boardEditId.trim(), body);
   setBoardName('');
   setBoardDescription('');
   await refresh();
  });
 }

 function submitTag(event: FormEvent, mode: 'create' | 'update') {
  event.preventDefault();
  const body = { name: tagName.trim(), description: tagDescription.trim() };
  void run(mode === 'create' ? `标签「${body.name}」已创建。` : '标签已更新。', async () => {
   if (mode === 'create') await createTag(body);
   else await updateTag(tagEditId.trim(), body);
   setTagName('');
   setTagDescription('');
   await refresh();
  });
 }

 function submitTopic(event: FormEvent) {
  event.preventDefault();
  void run('主题处置已记录并写入审计。', async () => {
   await moderateTopic(topicId.trim(), {
    reason: topicReason.trim(),
    is_locked: topicLocked,
    is_pinned: topicPinned,
    ...(topicBoard.trim() === '' ? {} : { board_id: topicBoard.trim() }),
   });
   setTopicReason('');
  });
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>账号与站点</h1>
  <p className="hint">处罚、板块、标签与主题处置；每个动作都会写入审计。举报队列在<a href="#/admin/moderation">审核与举报</a>。</p>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {notice !== '' && <p className="hint" role="status">{notice}</p>}

  <section aria-labelledby="users-heading">
   <h2 id="users-heading">账号</h2>
   <p className="hint">列表不含邮箱；处罚以内部用户 ID 为参数，因此需要一个输入框而不是下拉选择。</p>
   <ul className="reports">
    {users.map(user => <li key={user.internal_user_id}>
     <p className="byline">
      <span className="chip">{user.role === 'admin' ? '管理员' : '普通用户'}</span>
      {user.account_status === 'banned' && <span className="chip">已封禁</span>}
      <span>#{user.internal_user_id}</span>
      <span>{user.profile.nickname}</span>
     </p>
    </li>)}
   </ul>
   <form className="report-form" onSubmit={submitRestriction}>
    <div className="field">
     <label htmlFor="restrict-user">账号内部 ID</label>
     <input id="restrict-user" value={restrictUser} onChange={event => setRestrictUser(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="restrict-kind">处罚类型</label>
     <select id="restrict-kind" value={restrictKind} onChange={event => setRestrictKind(event.target.value === 'ban' ? 'ban' : 'mute')}>
      <option value="mute">禁言（限制发言，保留会话）</option>
      <option value="ban">封禁（立即撤销会话）</option>
     </select>
    </div>
    <div className="field">
     <label htmlFor="restrict-reason">原因（写入审计）</label>
     <input id="restrict-reason" value={restrictReason} maxLength={1000} onChange={event => setRestrictReason(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="restrict-ends">截止时间（留空表示永久）</label>
     <input id="restrict-ends" type="datetime-local" value={restrictEnds} onChange={event => setRestrictEnds(event.target.value)} />
    </div>
    <div className="actions">
     <button type="submit" disabled={busy || restrictUser.trim() === '' || restrictReason.trim() === ''}>提交处罚</button>
    </div>
   </form>
   <h3>处罚记录</h3>
   {restrictions.length === 0 && <p className="hint">暂无处罚记录。</p>}
   <ul className="reports">
    {restrictions.map(item => <li key={item.id}>
     <p className="byline">
      <span className="chip">{KIND_LABEL[item.kind] ?? item.kind}</span>
      {item.revoked_at !== undefined ? <span className="chip">已撤销</span> : <span className="chip">生效中</span>}
      <span>#{item.id}</span>
      <span className="time">{formatDateTime(item.starts_at)}</span>
     </p>
     <p>账号 #{item.user_id}　原因：{item.reason}{item.ends_at !== undefined && <>　截止 {formatDateTime(item.ends_at)}</>}</p>
     {item.revoked_at === undefined && <div className="actions">
      <button type="button" className="link" disabled={busy}
       onClick={() => void run(`处罚 #${item.id} 已撤销（记录保留）。`, async () => {
        await revokeRestriction(item.id, '界面撤销');
        await refresh();
       })}>撤销</button>
     </div>}
    </li>)}
   </ul>
  </section>

  <section aria-labelledby="structure-heading">
   <h2 id="structure-heading">板块与标签</h2>
   <p className="hint">板块一旦有主题，类型就不能再改（讨论板变问答板会改变板内每个主题的含义）。标签名按规范化后唯一，大小写不同视为同名。</p>
   <form className="report-form" onSubmit={event => submitBoard(event, 'create')}>
    <div className="field">
     <label htmlFor="board-name">板块名称</label>
     <input id="board-name" value={boardName} onChange={event => setBoardName(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="board-description">板块说明</label>
     <input id="board-description" value={boardDescription} onChange={event => setBoardDescription(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="board-kind">类型</label>
     <select id="board-kind" value={boardKind} onChange={event => setBoardKind(event.target.value === 'qa' ? 'qa' : 'discussion')}>
      <option value="discussion">讨论板块</option>
      <option value="qa">问答板块</option>
     </select>
    </div>
    <div className="field">
     <label htmlFor="board-edit-id">要修改的板块 ID（留空则新建）</label>
     <input id="board-edit-id" value={boardEditId} onChange={event => setBoardEditId(event.target.value)} />
    </div>
    <div className="actions">
     <button type="submit" disabled={busy || boardName.trim() === ''}>保存板块</button>
     <button type="button" className="secondary" disabled={busy || boardEditId.trim() === '' || boardName.trim() === ''}
      onClick={event => submitBoard(event, 'update')}>按 ID 更新</button>
    </div>
   </form>
   <p className="hint">现有板块：{boards.map(board => `#${board.id} ${board.name}（${board.kind === 'qa' ? '问答' : '讨论'}）`).join('、') || '无'}</p>
   <form className="report-form" onSubmit={event => submitTag(event, 'create')}>
    <div className="field">
     <label htmlFor="tag-name">标签名</label>
     <input id="tag-name" value={tagName} maxLength={80} onChange={event => setTagName(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="tag-description">标签说明</label>
     <input id="tag-description" value={tagDescription} onChange={event => setTagDescription(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="tag-edit-id">要修改的标签 ID（留空则新建）</label>
     <input id="tag-edit-id" value={tagEditId} onChange={event => setTagEditId(event.target.value)} />
    </div>
    <div className="actions">
     <button type="submit" disabled={busy || tagName.trim() === ''}>保存标签</button>
     <button type="button" className="secondary" disabled={busy || tagEditId.trim() === '' || tagName.trim() === ''}
      onClick={event => submitTag(event, 'update')}>按 ID 更新</button>
    </div>
   </form>
   <p className="hint">现有标签：{tags.map(tag => `#${tag.id} ${tag.name}`).join('、') || '无'}</p>
  </section>

  <section aria-labelledby="topic-heading">
   <h2 id="topic-heading">主题处置</h2>
   <form className="report-form" onSubmit={submitTopic}>
    <div className="field">
     <label htmlFor="topic-id">主题内容 ID</label>
     <input id="topic-id" value={topicId} onChange={event => setTopicId(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="topic-reason">原因（写入审计）</label>
     <input id="topic-reason" value={topicReason} maxLength={2000} onChange={event => setTopicReason(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="topic-board">移动到板块 ID（留空表示不移动）</label>
     <input id="topic-board" value={topicBoard} onChange={event => setTopicBoard(event.target.value)} />
    </div>
    <div className="field">
     <label><input type="checkbox" checked={topicLocked} onChange={event => setTopicLocked(event.target.checked)} /> 锁定</label>
     <label><input type="checkbox" checked={topicPinned} onChange={event => setTopicPinned(event.target.checked)} /> 置顶</label>
    </div>
    <div className="actions">
     <button type="submit" disabled={busy || topicId.trim() === '' || topicReason.trim() === ''}>提交处置</button>
    </div>
   </form>
  </section>

  <section aria-labelledby="reveal-heading">
   <h2 id="reveal-heading">删除原文与匿名追溯</h2>
   <p className="hint">两者都会按你填写的原因写入审计，且先审计再返回内容。删除原文超过 30 天保留期不可读取。</p>
   <form className="report-form" onSubmit={event => {
    event.preventDefault();
    void run('已记录访问并返回结果。', async () => {
     setArchive(await readDeletedArchive({ content_id: revealId.trim(), reason: revealReason.trim() }));
     setTrace(undefined);
    });
   }}>
    <div className="field">
     <label htmlFor="reveal-id">内容 ID</label>
     <input id="reveal-id" value={revealId} onChange={event => setRevealId(event.target.value)} />
    </div>
    <div className="field">
     <label htmlFor="reveal-reason">访问原因（写入审计）</label>
     <input id="reveal-reason" value={revealReason} maxLength={2000} onChange={event => setRevealReason(event.target.value)} />
    </div>
    <div className="actions">
     <button type="submit" disabled={busy || revealId.trim() === '' || revealReason.trim() === ''}>读取删除原文</button>
     <button type="button" className="secondary" disabled={busy || revealId.trim() === '' || revealReason.trim() === ''}
      onClick={() => void run('已记录追溯并返回作者。', async () => {
       setTrace(await traceAnonymousAuthor({ content_id: revealId.trim(), reason: revealReason.trim() }));
       setArchive(undefined);
      })}>追溯匿名作者</button>
    </div>
   </form>
   {archive && <article className="result">
    <p className="byline"><span className="chip">删除原文</span><span>内容 #{archive.content_id}</span></p>
    {archive.original_title !== undefined && <h3>{archive.original_title}</h3>}
    <pre className="archive">{archive.original_body_md}</pre>
    <p className="hint">删除于 {formatDateTime(archive.deleted_at)}，保留至 {formatDateTime(archive.purge_after)}。</p>
   </article>}
   {trace && <article className="result">
    <p className="byline"><span className="chip">匿名追溯</span><span>内容 #{trace.content_id}</span></p>
    <p>作者：{trace.author.profile.nickname}（公开 ID {trace.author.profile.public_id}，内部 ID #{trace.author.internal_user_id}，{trace.author.account_status === 'banned' ? '已封禁' : '正常'}）</p>
    <p className="hint">本次追溯的审计记录：#{trace.audit_id}。</p>
   </article>}
  </section>
 </main>;
}
