import { useState, type FormEvent } from 'react';
import { createReport } from '../api/reports';
import { describeError, type Failure } from '../lib/messages';
import { Alert } from './Alert';

type ReportButtonProps = {
 /** The content being reported. */
 contentId: string;
 /** Whether a session exists; reporting requires one. */
 sessionReady: boolean;
 signedIn: boolean;
 onUnauthenticated: () => void;
};

/**
 * 举报入口：一个按钮 + 就地展开的表单。
 *
 * 三条与后端一致的行为：
 *
 * - **举报不会隐藏内容。** 文案里写明这一点，否则读者会以为点了就等于举报成功、内容会被删掉。
 * - **未登录时不发请求**，而是走登录流程：服务端会返回 401，让用户先登录比让请求白跑一次更直接。
 * - **重复举报（409）当作结果展示**，不是错误弹窗：同一内容只能有一个未结案举报，这是规则而不是故障，
 *   所以成功与"已经举报过"都用同一段成功文案表达。
 */
export function ReportButton({ contentId, sessionReady, signedIn, onUnauthenticated }: ReportButtonProps) {
 const [open, setOpen] = useState(false);
 const [reason, setReason] = useState('');
 const [details, setDetails] = useState('');
 const [busy, setBusy] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [done, setDone] = useState(false);

 function handleClick() {
  if (!signedIn) {
   onUnauthenticated();
   return;
  }
  setOpen(current => !current);
 }

 async function handleSubmit(event: FormEvent) {
  event.preventDefault();
  if (busy || reason.trim() === '') return;
  setBusy(true);
  setFailure(undefined);
  try {
   await createReport({ content_id: contentId, reason: reason.trim(), details: details.trim() });
   setDone(true);
   setOpen(false);
  } catch (error) {
   // A second open report about the same content is answered with 409; that is the rule working, not a
   // failure the reader has to act on. The code lives on the thrown API error, not on the friendly Failure.
   const code = (error as { code?: string }).code;
   if (code === 'REPORT_ALREADY_ACTIVE') {
    setDone(true);
    setOpen(false);
   } else {
    setFailure(describeError(error));
   }
  } finally {
   setBusy(false);
  }
 }

 if (done) {
  return <span className="hint">已提交举报，管理员会处理；举报不会自动隐藏内容。</span>;
 }

 return <>
  {/* 未登录时按钮仍可点：点击会走登录流程，而不是变成一个点了没反应的死按钮。 */}
  <button type="button" className="link" onClick={handleClick}>
   {open ? '取消举报' : '举报'}
  </button>
  {open && <form className="report-form" onSubmit={handleSubmit}>
   <div className="field">
    <label htmlFor={`report-reason-${contentId}`}>举报原因</label>
    <input
     id={`report-reason-${contentId}`}
     value={reason}
     maxLength={500}
     onChange={event => setReason(event.target.value)}
     placeholder="例如：包含广告、人身攻击"
    />
   </div>
   <div className="field">
    <label htmlFor={`report-details-${contentId}`}>补充说明（可选）</label>
    <textarea
     id={`report-details-${contentId}`}
     value={details}
     maxLength={2000}
     rows={2}
     onChange={event => setDetails(event.target.value)}
    />
   </div>
   <p className="hint">举报会提交给管理员，不会自动隐藏或删除内容。</p>
   <Alert failure={failure} />
   <div className="actions">
    <button type="submit" disabled={busy || reason.trim() === ''}>{busy ? '正在提交…' : '提交举报'}</button>
   </div>
  </form>}
 </>;
}
