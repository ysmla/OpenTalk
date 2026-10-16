import { useCallback, useState } from 'react';
import { unsubscribeByToken } from '../api/preferences';
import { Alert, Status } from '../components/Alert';
import { describeError, isStatus, type Failure } from '../lib/messages';

type UnsubscribeViewProps = {
 /** `token` from `#/unsubscribe?token=…`, exactly as it appeared in the mail. */
 token?: string;
};

type Phase = 'confirm' | 'pending' | 'done' | 'failed';

/**
 * 邮件退订确认页（requirements.md:97；`POST /subscriptions/unsubscribe`）。
 *
 * The mail carries a link to this page, and the link deliberately does **not**
 * unsubscribe anyone by itself: mail scanners and link previewers fetch URLs, so
 * a GET that changed state would let a robot unsubscribe a reader. Opening this
 * page therefore only *shows* what the link is for; the write happens when the
 * reader presses 确认退订.
 *
 * No session is required — that is the point of a link in a mail — and the
 * signed token names exactly one account and one topic, so the most this page
 * can ever do is turn off mail for the discussion the mail was about.
 *
 * A token that is malformed, forged or expired is refused by the server in the
 * same way, and the page says so without pretending to know which of the three
 * it was. The token is never echoed back into the page.
 *
 * The whole screen is keyed by the token in `App`, so a second link — or a
 * refresh with a token that no longer verifies — starts from a clean state
 * instead of showing the previous result. A confirmation that says "已退订"
 * while the address bar holds a different, broken token is exactly the sort of
 * stale success this page must never show.
 */
export function UnsubscribeView({ token }: UnsubscribeViewProps) {
 // A link without a token cannot unsubscribe anything, so the page starts out
 // saying exactly that instead of offering a button that must fail. Nothing is
 // sent to the server to find this out.
 const usable = token !== undefined && token.trim() !== '';
 const [phase, setPhase] = useState<Phase>(usable ? 'confirm' : 'failed');
 const [failure, setFailure] = useState<Failure>();

 const confirm = useCallback(async () => {
  if (token === undefined) return;
  setPhase('pending');
  setFailure(undefined);
  try {
   await unsubscribeByToken(token);
   setPhase('done');
  } catch (error) {
   if (isStatus(error, 400) || isStatus(error, 404)) {
    setFailure({ text: '这个退订链接无效或已过期。' });
   } else if (isStatus(error, 403)) {
    setFailure({ text: '退订请求未被接受。请从邮件中的链接重新打开本页后再试。' });
   } else {
    setFailure(describeError(error));
   }
   setPhase('failed');
  }
 }, [token]);

 return <main className="page">
  <p className="eyebrow">OPENTALK</p>
  <h1>取消邮件订阅</h1>

  {phase === 'confirm' && <>
   <p>你打开的是通知邮件里的退订链接。确认后，这个主题不再向你发送邮件通知。</p>
   <p className="hint">
    退订只影响邮件：站内通知照常，其他主题的邮件设置也不变。打开本页本身不会退订，
    需要你点击下面的按钮。
   </p>
   <div className="actions">
    <button type="button" onClick={() => void confirm()}>确认退订这个主题的邮件</button>
    <a href="#/me">去个人资料管理邮件偏好</a>
   </div>
  </>}

  {phase === 'pending' && <Status>正在提交退订…</Status>}

  {phase === 'done' && <>
   <Status>已退订：这个主题不再向你发送邮件通知，站内通知不受影响。</Status>
   <nav className="links" aria-label="继续">
    <a href="#/me">管理其他邮件偏好</a>
    <a href="#/">返回首页</a>
   </nav>
  </>}

  {phase === 'failed' && <>
   <Alert failure={failure ?? { text: '这个退订链接无效或已过期。' }} />
   <p className="hint">退订链接有有效期。你可以在登录后到个人资料页调整邮件偏好，或在该主题的详情页单独设置。</p>
   <nav className="links" aria-label="继续">
    <a href="#/me">去个人资料</a>
    <a href="#/">返回首页</a>
   </nav>
  </>}
 </main>;
}
