import { useEffect, useRef, useState } from 'react';
import {
 getNotificationPreferences,
 setNotificationPreferences,
 type NotificationPreferences,
} from '../api/preferences';
import { Alert, Status } from '../components/Alert';
import { describeError, isStatus, type Failure } from '../lib/messages';

type MailPreferencesProps = {
 /** 会话已确认存在才由父级渲染本组件；为 false 时不发请求。 */
 signedIn: boolean;
};

/** 三项的展示顺序与文案，与 NotificationPreferences 的字段一一对应。 */
const FIELDS: { key: keyof NotificationPreferences; label: string; hint: string }[] = [
 { key: 'email_on_comment', label: '帖子或问题收到评论时发邮件', hint: '有人评论我发布的帖子或问题。' },
 { key: 'email_on_answer', label: '问题收到回答时发邮件', hint: '有人回答我提出的问题。' },
 { key: 'email_on_reply', label: '发言收到回复时发邮件', hint: '有人回复我的评论或回答。' },
];

const NONE: NotificationPreferences = {
 email_on_comment: false,
 email_on_answer: false,
 email_on_reply: false,
};

/**
 * 全局邮件偏好（`GET/PUT /me/notification-preferences`）。
 *
 * 合同把 PUT 定义成整体替换且三项必填：少传一项是 422 而不是"保持原样"。因此
 * 这里把三项一起持有、一起提交，界面上没有"只保存这一项"的路径，也就不会因为
 * 省略字段而静默关掉另外两项。
 *
 * 邮件与站内通知是两件事：这里只影响邮件。站内通知即时写入，不受开关影响。
 */
export function MailPreferences({ signedIn }: MailPreferencesProps) {
 const [preferences, setPreferences] = useState<NotificationPreferences>();
 const [draft, setDraft] = useState<NotificationPreferences>(NONE);
 const [loading, setLoading] = useState(true);
 const [saving, setSaving] = useState(false);
 const [failure, setFailure] = useState<Failure>();
 const [notice, setNotice] = useState<string>();
 const [reloadKey, setReloadKey] = useState(0);
 /**
  * Whether the reader has changed anything since the last read.
  *
  * The read is asynchronous, so a checkbox the reader ticks while it is in
  * flight must not be overwritten by the answer that was already on its way.
  * Without this, the pattern "tick two boxes right after the section appears"
  * can end up submitting the server's original values instead of the ticks.
  */
 const edited = useRef(false);

 useEffect(() => {
  if (!signedIn) {
   setLoading(false);
   return;
  }
  const controller = new AbortController();
  setLoading(true);
  setFailure(undefined);
  edited.current = false;
  getNotificationPreferences(controller.signal).then(current => {
   if (controller.signal.aborted) return;
   setPreferences(current);
   // A read that is already answered must not clobber a live edit.
   if (!edited.current) setDraft(current);
  }).catch((error: unknown) => {
   if (controller.signal.aborted) return;
   setFailure(describeError(error));
  }).finally(() => {
   if (!controller.signal.aborted) setLoading(false);
  });
  return () => controller.abort();
 }, [signedIn, reloadKey]);

 const dirty = preferences !== undefined
  && (draft.email_on_comment !== preferences.email_on_comment
   || draft.email_on_answer !== preferences.email_on_answer
   || draft.email_on_reply !== preferences.email_on_reply);

 function toggle(key: keyof NotificationPreferences, value: boolean) {
  edited.current = true;
  setDraft(current => ({ ...current, [key]: value }));
  setNotice(undefined);
 }

 async function handleSubmit() {
  if (saving || !dirty) return;
  setSaving(true);
  setFailure(undefined);
  setNotice(undefined);
  try {
   // 三项一起提交：合同要求齐全，部分提交会被 422 拒绝。
   const saved = await setNotificationPreferences(draft);
   setPreferences(saved);
   edited.current = false;
   setDraft(saved);
   setNotice('邮件偏好已保存。');
  } catch (error) {
   if (isStatus(error, 401)) {
    setFailure({ text: '登录状态已失效，请重新登录后再设置邮件偏好。' });
    return;
   }
   setFailure(describeError(error));
  } finally {
   setSaving(false);
  }
 }

 if (!signedIn) {
  return <section aria-labelledby="mail-preferences-heading">
   <h2 id="mail-preferences-heading">邮件偏好</h2>
   <p className="hint">登录后可以设置是否接收通知邮件。</p>
  </section>;
 }

 return <section aria-labelledby="mail-preferences-heading">
  <h2 id="mail-preferences-heading">邮件偏好</h2>
  {loading && <Status>正在读取邮件偏好…</Status>}
  <Alert failure={failure} />
  {failure && <div className="actions">
   <button type="button" className="secondary" onClick={() => setReloadKey(key => key + 1)}>重新读取</button>
  </div>}
  {preferences !== undefined && <form onSubmit={event => { event.preventDefault(); void handleSubmit(); }}>
   <p className="hint">
    这里只决定邮件：站内通知不受影响，关闭邮件后站内照常收通知。新账号默认在收到回答与回复时
    发邮件、收到评论时不发，三项在这里都能改。三项一起保存，服务端按整体替换处理。
   </p>
   <div className="field" role="group" aria-label="邮件通知事件">
    <span className="label">发邮件通知我的事件</span>
    {FIELDS.map(field => <label className="checkbox" key={field.key} title={field.hint}>
     <input
      type="checkbox"
      checked={draft[field.key]}
      disabled={saving}
      onChange={event => toggle(field.key, event.target.checked)}
     />
     {field.label}
    </label>)}
    <p className="hint">每个主题还可以单独覆盖这里的设置，入口在该主题的详情页。</p>
   </div>
   <Status>{notice}</Status>
   <div className="actions">
    <button type="submit" disabled={saving || !dirty}>{saving ? '正在保存…' : '保存邮件偏好'}</button>
   </div>
  </form>}
 </section>;
}
