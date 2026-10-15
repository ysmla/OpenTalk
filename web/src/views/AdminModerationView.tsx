import { useEffect, useState, type FormEvent } from 'react';
import {
 listAdminReports,
 listAudit,
 resolveReport,
 setContentVisibility,
 type AdminAuditEntry,
 type AdminReport,
} from '../api/reports';
import { Alert, Status } from '../components/Alert';
import { formatDateTime } from '../lib/format';
import { describeError, type Failure } from '../lib/messages';

type AdminModerationViewProps = {
 sessionReady: boolean;
 signedIn: boolean;
 isAdmin: boolean;
 onUnauthenticated: () => void;
};

const ACTION_LABEL: Record<string, string> = {
 hide_content: '隐藏内容',
 restore_content: '恢复内容',
 resolve_report: '结案举报',
};

/**
 * 管理端：举报队列、内容处置与审计。
 *
 * 三块刻意放在一起，因为它们是一条链：队列里看到举报 → 处置内容（可选）→ 结案 → 审计里留下两条记录。
 * 界面明确区分"结案"与"隐藏内容"两个动作，因为服务端也是分开的：结案只记录对举报的决定，隐藏内容
 * 另走一个端点。把它们合成一个按钮，审计就无法说清管理员到底做了哪一种。
 */
export function AdminModerationView({ sessionReady, signedIn, isAdmin, onUnauthenticated }: AdminModerationViewProps) {
 const [queue, setQueue] = useState<AdminReport[]>([]);
 const [queueCursor, setQueueCursor] = useState<string>();
 const [queueHasMore, setQueueHasMore] = useState(false);
 const [audit, setAudit] = useState<AdminAuditEntry[]>([]);
 const [auditCursor, setAuditCursor] = useState<string>();
 const [auditHasMore, setAuditHasMore] = useState(false);
 const [loading, setLoading] = useState(true);
 const [busy, setBusy] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState('');
 const [note, setNote] = useState<Record<string, string>>({});
 const [disposeId, setDisposeId] = useState('');
 const [disposeReason, setDisposeReason] = useState('');

 async function refresh() {
  setLoading(true);
  setFailure(undefined);
  try {
   const [reports, entries] = await Promise.all([listAdminReports(), listAudit()]);
   setQueue(reports.items);
   setQueueCursor(reports.next_cursor);
   setQueueHasMore(reports.has_more);
   setAudit(entries.items);
   setAuditCursor(entries.next_cursor);
   setAuditHasMore(entries.has_more);
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
   // 非管理员不进入这个界面：服务端会返回 403，但让一个没有权限的人看到管理界面本身就是误导。
   if (!signedIn) onUnauthenticated();
   return;
  }
  void refresh();
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [sessionReady, signedIn, isAdmin]);

 if (sessionReady && signedIn && !isAdmin) {
  return <main className="page"><h1>管理</h1><p className="hint">当前账号不是管理员。</p></main>;
 }

 async function closeReport(report: AdminReport, status: 'resolved' | 'dismissed') {
  const resolution = (note[report.report.id] ?? '').trim();
  if (busy) return;
  if (resolution === '') {
   setFailure({ text: '请先填写处理说明' });
   return;
  }
  setBusy(true);
  setFailure(undefined);
  setNotice('');
  try {
   await resolveReport(report.report.id, { status, resolution_note: resolution });
   setNotice(`举报 #${report.report.id} 已结案（${status === 'resolved' ? '已处理' : '不成立'}）。`);
   await refresh();
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 async function changeVisibility(event: FormEvent, visibility: 'visible' | 'hidden') {
  event.preventDefault();
  if (busy) return;
  if (disposeId.trim() === '' || disposeReason.trim() === '') {
   setFailure({ text: '请填写内容 ID 与操作原因' });
   return;
  }
  setBusy(true);
  setFailure(undefined);
  setNotice('');
  try {
   await setContentVisibility(disposeId.trim(), { visibility, reason: disposeReason.trim() });
   setNotice(visibility === 'hidden'
    ? `内容 #${disposeId.trim()} 已隐藏。隐藏会连同其后代与附件一起停止公开访问。`
    : `内容 #${disposeId.trim()} 已恢复。恢复后重新受祖先状态与删除状态约束。`);
   setDisposeReason('');
   const entries = await listAudit();
   setAudit(entries.items);
   setAuditCursor(entries.next_cursor);
   setAuditHasMore(entries.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>审核与举报</h1>
  <p className="hint">结案只记录对举报的决定；隐藏内容是另一个动作，两者都会写入审计。</p>
  {loading && <Status>正在读取…</Status>}
  <Alert failure={failure} />
  {notice !== '' && <p className="hint" role="status">{notice}</p>}

  <section aria-labelledby="queue-heading">
   <h2 id="queue-heading">举报队列</h2>
   {!loading && queue.length === 0 && <p className="hint">没有待处理的举报。</p>}
   <ul className="reports">
    {queue.map(item => <li key={item.report.id}>
     <p className="byline">
      <span className="chip">{item.report.status === 'processing' ? '处理中' : '待处理'}</span>
      <span>举报 #{item.report.id}</span>
      <span className="time">{formatDateTime(item.report.created_at)}</span>
     </p>
     <p><strong>目标内容：</strong>#{item.report.content_id}　
      <strong>举报者：</strong>{item.reporter.profile.nickname}
      {item.reporter.account_status === 'banned' && <span className="chip">已封禁</span>}</p>
     <p><strong>原因：</strong>{item.report.reason}</p>
     {item.report.details !== '' && <p className="hint">补充说明：{item.report.details}</p>}
     <div className="field">
      <label htmlFor={`note-${item.report.id}`}>处理说明（结案必填，举报者可见）</label>
      <input
       id={`note-${item.report.id}`}
       value={note[item.report.id] ?? ''}
       maxLength={2000}
       onChange={event => setNote(current => ({ ...current, [item.report.id]: event.target.value }))}
      />
     </div>
     <div className="actions">
      <button type="button" disabled={busy} onClick={() => void closeReport(item, 'resolved')}>标记已处理并结案</button>
      <button type="button" className="secondary" disabled={busy} onClick={() => void closeReport(item, 'dismissed')}>判定不成立并结案</button>
     </div>
    </li>)}
   </ul>
   {queueHasMore && <div className="actions">
    <button type="button" className="secondary" disabled={busy} onClick={() => void loadMore()}>加载更多举报</button>
   </div>}
  </section>

  <section aria-labelledby="dispose-heading">
   <h2 id="dispose-heading">内容处置</h2>
   <p className="hint">隐藏与删除独立：隐藏不写删除归档，因此可以恢复；隐藏一个主题会连同它的回答、回复与附件一起停止公开访问。</p>
   <form className="report-form" onSubmit={event => void changeVisibility(event, 'hidden')}>
    <div className="field">
     <label htmlFor="dispose-id">内容 ID</label>
     <input id="dispose-id" value={disposeId} onChange={event => setDisposeId(event.target.value)} placeholder="主题、回答或评论的 ID" />
    </div>
    <div className="field">
     <label htmlFor="dispose-reason">操作原因（写入审计）</label>
     <input id="dispose-reason" value={disposeReason} maxLength={2000} onChange={event => setDisposeReason(event.target.value)} />
    </div>
    <div className="actions">
     <button type="submit" disabled={busy}>隐藏内容</button>
     <button type="button" className="secondary" disabled={busy} onClick={event => void changeVisibility(event, 'visible')}>恢复内容</button>
    </div>
   </form>
  </section>

  <section aria-labelledby="audit-heading">
   <h2 id="audit-heading">审计</h2>
   <p className="hint">只追加；记录操作者、动作、原因与目标 ID，不含内容正文。</p>
   {!loading && audit.length === 0 && <p className="hint">暂无审计记录。</p>}
   <ul className="reports">
    {audit.map(entry => <li key={entry.id}>
     <p className="byline">
      <span className="chip">{ACTION_LABEL[entry.action] ?? entry.action}</span>
      <span className="time">{formatDateTime(entry.created_at)}</span>
     </p>
     <p>操作者 #{entry.actor_user_id}
      {entry.target_content_id !== undefined && <>　目标内容 #{entry.target_content_id}</>}
      {entry.target_user_id !== undefined && <>　目标用户 #{entry.target_user_id}</>}</p>
     <p className="hint">原因：{entry.reason}</p>
    </li>)}
   </ul>
   {auditHasMore && <div className="actions">
    <button type="button" className="secondary" disabled={busy} onClick={() => void loadMoreAudit()}>加载更多审计</button>
   </div>}
  </section>
 </main>;

 async function loadMore() {
  if (busy || queueCursor === undefined) return;
  setBusy(true);
  try {
   const page = await listAdminReports({ cursor: queueCursor });
   setQueue(current => [...current, ...page.items]);
   setQueueCursor(page.next_cursor);
   setQueueHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }

 async function loadMoreAudit() {
  if (busy || auditCursor === undefined) return;
  setBusy(true);
  try {
   const page = await listAudit({ cursor: auditCursor });
   setAudit(current => [...current, ...page.items]);
   setAuditCursor(page.next_cursor);
   setAuditHasMore(page.has_more);
  } catch (error) {
   setFailure(describeError(error));
  } finally {
   setBusy(false);
  }
 }
}
